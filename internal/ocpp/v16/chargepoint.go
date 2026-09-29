package v16

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// Caller sends one CALL and returns the raw CALLRESULT payload.
//
// *ocppj.Conn satisfies it. It is an interface so the adapter can be tested
// against a fake that records what would have gone on the wire.
type Caller interface {
	ID() string
	Version() ocpp.Version
	Call(ctx context.Context, action string, payload any) (json.RawMessage, error)
}

// ChargePoint is the OCPP 1.6 implementation of ocpp.ChargePoint. It turns
// version-agnostic commands into 1.6 messages, and 1.6 answers back into
// ocpp.Result.
type ChargePoint struct {
	c   Caller
	cfg *config.Config
}

var _ ocpp.ChargePoint = (*ChargePoint)(nil)

// NewChargePoint wraps a 1.6 connection. cfg supplies the mapping from EVSE
// uid to connector id.
func NewChargePoint(c Caller, cfg *config.Config) *ChargePoint {
	return &ChargePoint{c: c, cfg: cfg}
}

// ID is the charge point identity.
func (cp *ChargePoint) ID() string { return cp.c.ID() }

// Version is the OCPP version of the underlying connection.
func (cp *ChargePoint) Version() ocpp.Version { return cp.c.Version() }

// ReserveNow sends ReserveNow for the EVSE's connector.
func (cp *ChargePoint) ReserveNow(ctx context.Context, r ocpp.ReserveRequest) (ocpp.Result, error) {
	connectorID, err := cp.connectorID(r.EVSEUID)
	if err != nil {
		return ocpp.Result{}, err
	}
	return cp.command(ctx, ActionReserveNow, ReserveNowReq{
		ConnectorID: connectorID,
		// Always UTC with a Z: stations disagree about offsets, and none of
		// them misreads a Z.
		ExpiryDate:    r.ExpiresAt.UTC().Format(time.RFC3339),
		IDTag:         r.IDTag,
		ReservationID: r.ReservationID,
	})
}

// CancelReservation sends CancelReservation.
func (cp *ChargePoint) CancelReservation(ctx context.Context, reservationID int) (ocpp.Result, error) {
	return cp.command(ctx, ActionCancelReservation, CancelReservationReq{ReservationID: reservationID})
}

// RemoteStart sends RemoteStartTransaction. The connector is always named:
// leaving it out lets the station pick one, which is not what "start on this
// EVSE" means. r.RemoteStartID has no 1.6 counterpart and is not sent.
func (cp *ChargePoint) RemoteStart(ctx context.Context, r ocpp.RemoteStartRequest) (ocpp.Result, error) {
	connectorID, err := cp.connectorID(r.EVSEUID)
	if err != nil {
		return ocpp.Result{}, err
	}
	return cp.command(ctx, ActionRemoteStartTransaction, RemoteStartTransactionReq{
		ConnectorID: &connectorID,
		IDTag:       r.IDTag,
	})
}

// RemoteStop sends RemoteStopTransaction. 1.6 transaction ids are integers,
// so anything else is refused before it is sent.
func (cp *ChargePoint) RemoteStop(ctx context.Context, transactionID string) (ocpp.Result, error) {
	id, err := strconv.Atoi(transactionID)
	if err != nil {
		return ocpp.Result{}, fmt.Errorf("ocpp 1.6: transaction id %q is not an integer", transactionID)
	}
	return cp.command(ctx, ActionRemoteStopTransaction, RemoteStopTransactionReq{TransactionID: id})
}

// UnlockConnector sends UnlockConnector for the EVSE's connector.
func (cp *ChargePoint) UnlockConnector(ctx context.Context, evseUID string) (ocpp.Result, error) {
	connectorID, err := cp.connectorID(evseUID)
	if err != nil {
		return ocpp.Result{}, err
	}
	return cp.command(ctx, ActionUnlockConnector, UnlockConnectorReq{ConnectorID: connectorID})
}

// TriggerStatus asks for a StatusNotification for the EVSE's connector.
func (cp *ChargePoint) TriggerStatus(ctx context.Context, evseUID string) (ocpp.Result, error) {
	connectorID, err := cp.connectorID(evseUID)
	if err != nil {
		return ocpp.Result{}, err
	}
	return cp.command(ctx, ActionTriggerMessage, TriggerMessageReq{
		RequestedMessage: ActionStatusNotification,
		ConnectorID:      &connectorID,
	})
}

// Feature profile names as they appear in SupportedFeatureProfiles.
const (
	profileReservation   = "Reservation"
	profileRemoteTrigger = "RemoteTrigger"
)

// Capabilities reads SupportedFeatureProfiles. A station that does not know
// the key advertises nothing, which is an answer rather than a failure.
func (cp *ChargePoint) Capabilities(ctx context.Context) (ocpp.Capabilities, error) {
	conf, err := cp.GetConfiguration(ctx, []string{KeySupportedFeatureProfiles})
	if err != nil {
		return ocpp.Capabilities{}, err
	}

	var caps ocpp.Capabilities
	for _, k := range conf.ConfigurationKey {
		if k.Key != KeySupportedFeatureProfiles {
			continue
		}
		// A CSL: "Core,Reservation", sometimes with spaces after the commas.
		for _, p := range strings.Split(k.Value, ",") {
			switch strings.TrimSpace(p) {
			case profileReservation:
				caps.Reservation = true
			case profileRemoteTrigger:
				caps.RemoteTrigger = true
			}
		}
	}
	return caps, nil
}

// GetConfiguration reads configuration keys; nil keys means all of them.
//
// It is deliberately not part of ocpp.ChargePoint: 2.0.1 has no flat
// key/value configuration. It is here for Capabilities and for debugging.
func (cp *ChargePoint) GetConfiguration(ctx context.Context, keys []string) (GetConfigurationConf, error) {
	var conf GetConfigurationConf
	raw, err := cp.c.Call(ctx, ActionGetConfiguration, GetConfigurationReq{Key: keys})
	if err != nil {
		return conf, err
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return conf, fmt.Errorf("%s response: %w", ActionGetConfiguration, err)
	}
	return conf, nil
}

// connectorID maps an EVSE uid to its 1.6 connector id.
func (cp *ChargePoint) connectorID(evseUID string) (int, error) {
	e, ok := cp.cfg.EVSEByUID(evseUID)
	if !ok {
		return 0, fmt.Errorf("%w %q", ocpp.ErrUnknownEVSE, evseUID)
	}
	return e.OCPPConnectorID, nil
}

// command sends a request whose answer is a bare {"status": ...} and maps
// that status.
func (cp *ChargePoint) command(ctx context.Context, action string, req any) (ocpp.Result, error) {
	raw, err := cp.c.Call(ctx, action, req)
	if err != nil {
		return ocpp.Result{}, err
	}
	var conf struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return ocpp.Result{}, fmt.Errorf("%s response: %w", action, err)
	}
	return ocpp.Result{Status: MapCommandStatus(action, conf.Status), Raw: conf.Status}, nil
}
