// Package v16 implements the OCPP 1.6-J message set: the payload shapes and
// the adapter that turns inbound messages into domain state changes.
//
// Everything version-specific about OCPP 1.6 lives here. The transport in
// internal/ocpp/csms knows none of it, which is what lets OCPP 2.0.1 arrive
// later as a sibling package.
package v16

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// Handler processes inbound OCPP 1.6-J calls.
type Handler struct {
	cfg  *config.Config
	core *core.Service
	log  *slog.Logger
	now  func() time.Time
}

// Option customises a Handler.
type Option func(*Handler)

// WithClock replaces the clock, so tests can assert on exact timestamps.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) { h.now = now }
}

// NewHandler builds the 1.6 adapter.
func NewHandler(cfg *config.Config, svc *core.Service, log *slog.Logger, opts ...Option) *Handler {
	h := &Handler{cfg: cfg, core: svc, log: log, now: time.Now}
	if h.log == nil {
		h.log = slog.Default()
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Version reports the OCPP version this handler speaks.
func (h *Handler) Version() ocpp.Version { return ocpp.Version16 }

// HandleCall dispatches one inbound message.
func (h *Handler) HandleCall(_ context.Context, cpID, action string, payload json.RawMessage) (any, *ocpp.RPCError) {
	switch action {
	case ActionBootNotification:
		return h.bootNotification(cpID, payload)
	case ActionHeartbeat:
		return h.heartbeat(cpID)
	case ActionStatusNotification:
		return h.statusNotification(cpID, payload)
	case ActionAuthorize:
		return h.authorize(cpID, payload)
	case ActionStartTransaction:
		return h.startTransaction(cpID, payload)
	case ActionStopTransaction:
		return h.stopTransaction(cpID, payload)
	case ActionMeterValues:
		return h.meterValues(cpID, payload)
	case ActionDataTransfer:
		return h.dataTransfer(cpID, payload)
	case ActionFirmwareStatusNotification, ActionDiagnosticsStatusNotification:
		return h.statusOnly(cpID, action, payload)
	default:
		return nil, ocpp.Errorf(ocpp.ErrNotImplemented, "action %q is not implemented", action)
	}
}

func (h *Handler) bootNotification(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[BootNotificationReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := req.validate(); rpcErr != nil {
		return nil, rpcErr
	}

	serial := req.ChargePointSerialNumber
	if serial == "" {
		serial = req.ChargeBoxSerialNumber
	}
	h.core.Booted(cpID, core.BootInfo{
		Vendor:          req.ChargePointVendor,
		Model:           req.ChargePointModel,
		SerialNumber:    serial,
		FirmwareVersion: req.FirmwareVersion,
	})

	return BootNotificationConf{
		Status:      "Accepted",
		CurrentTime: formatTime(h.now()),
		Interval:    int(h.cfg.Charger.HeartbeatInterval.Duration().Seconds()),
	}, nil
}

func (h *Handler) heartbeat(cpID string) (any, *ocpp.RPCError) {
	h.core.Heartbeat(cpID)
	return HeartbeatConf{CurrentTime: formatTime(h.now())}, nil
}

func (h *Handler) statusNotification(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[StatusNotificationReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := req.validate(); rpcErr != nil {
		return nil, rpcErr
	}

	h.core.SetConnectorStatus(cpID, req.ConnectorID, MapStatus(req.Status), req.ErrorCode)
	return emptyConf{}, nil
}

func (h *Handler) authorize(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[AuthorizeReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if req.IDTag == "" {
		return nil, missing("idTag")
	}

	h.core.Seen(cpID)
	status := AuthInvalid
	if h.knownTag(req.IDTag) {
		status = AuthAccepted
	} else {
		h.log.Warn("rejecting unknown id tag", "charge_point", cpID, "id_tag", req.IDTag)
	}
	return AuthorizeConf{IDTagInfo: IDTagInfo{Status: status}}, nil
}

// knownTag reports whether a tag is the one we are configured to accept.
// OCPP idTags are case-insensitive.
func (h *Handler) knownTag(tag string) bool {
	return strings.EqualFold(tag, h.cfg.Auth.DefaultIDTag)
}

func (h *Handler) startTransaction(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[StartTransactionReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := req.validate(); rpcErr != nil {
		return nil, rpcErr
	}

	// An unknown tag is refused with an Invalid idTagInfo rather than a
	// CALLERROR: the message was perfectly well formed, the answer is just no.
	if !h.knownTag(req.IDTag) {
		h.core.Seen(cpID)
		h.log.Warn("refusing transaction for unknown id tag", "charge_point", cpID, "id_tag", req.IDTag)
		return StartTransactionConf{
			TransactionID: 0,
			IDTagInfo:     IDTagInfo{Status: AuthInvalid},
		}, nil
	}

	at := parseTime(req.Timestamp, h.now())
	txID := h.core.StartTransaction(cpID, req.ConnectorID, req.IDTag, req.MeterStart, at)

	return StartTransactionConf{
		TransactionID: txID,
		IDTagInfo:     IDTagInfo{Status: AuthAccepted},
	}, nil
}

func (h *Handler) stopTransaction(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[StopTransactionReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	at := parseTime(req.Timestamp, h.now())
	if _, ok := h.core.StopTransaction(cpID, req.TransactionID, req.MeterStop, req.Reason, at); !ok {
		// The spec has no way to say "no such transaction" here, and refusing
		// the message would make the charger retry forever. Log and accept.
		h.log.Warn("stop for unknown transaction",
			"charge_point", cpID, "transaction_id", req.TransactionID)
	}
	return StopTransactionConf{}, nil
}

func (h *Handler) meterValues(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[MeterValuesReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	h.core.RecordMeterValues(cpID, req.ConnectorID, summariseMeterValues(req.MeterValue))
	return emptyConf{}, nil
}

// summariseMeterValues renders the newest reading for the log and the TUI. The
// full sample set is not modelled: nothing in cpms bills, and the CDR module
// is explicitly out of scope.
func summariseMeterValues(values []MeterValue) string {
	if len(values) == 0 {
		return "no samples"
	}
	last := values[len(values)-1]
	if len(last.SampledValue) == 0 {
		return "no samples"
	}

	parts := make([]string, 0, len(last.SampledValue))
	for _, sv := range last.SampledValue {
		measurand := sv.Measurand
		if measurand == "" {
			// The 1.6 default when the field is absent.
			measurand = "Energy.Active.Import.Register"
		}
		parts = append(parts, fmt.Sprintf("%s=%s%s", measurand, sv.Value, sv.Unit))
	}
	return strings.Join(parts, " ")
}

func (h *Handler) dataTransfer(cpID string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[DataTransferReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	h.core.Seen(cpID)
	h.log.Info("rejecting vendor data transfer",
		"charge_point", cpID, "vendor_id", req.VendorID, "message_id", req.MessageID)
	// We implement no vendor extensions, and saying so is the correct answer.
	return DataTransferConf{Status: "Rejected"}, nil
}

func (h *Handler) statusOnly(cpID, action string, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decode[StatusOnlyReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	h.core.Seen(cpID)
	h.log.Info("status notification", "charge_point", cpID, "action", action, "status", req.Status)
	return emptyConf{}, nil
}
