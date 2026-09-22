package v16_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

const (
	testCP  = "ALP-HYC-001"
	testTag = "04A1B2C3D4"
)

var testNow = time.Date(2026, 8, 14, 12, 30, 0, 0, time.UTC)

func testConfig() *config.Config {
	return &config.Config{
		Charger: config.Charger{
			ID:                testCP,
			HeartbeatInterval: config.NewDuration(60 * time.Second),
		},
		Auth: config.Auth{DefaultIDTag: testTag},
		Location: config.Location{
			EVSEs: []config.EVSE{
				{UID: "EVSE-1", OCPPConnectorID: 1},
				{UID: "EVSE-2", OCPPConnectorID: 2},
			},
		},
	}
}

func newHandler(t *testing.T) (*v16.Handler, *core.Service) {
	t.Helper()

	cfg := testConfig()
	svc := core.New(cfg)
	h := v16.NewHandler(cfg, svc, slog.New(slog.NewTextHandler(io.Discard, nil)),
		v16.WithClock(func() time.Time { return testNow }))
	return h, svc
}

// call invokes the handler and decodes the result into T.
func call[T any](t *testing.T, h *v16.Handler, action string, payload any) T {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}

	result, rpcErr := h.HandleCall(context.Background(), testCP, action, raw)
	if rpcErr != nil {
		t.Fatalf("%s: unexpected %s: %s", action, rpcErr.Code, rpcErr.Description)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshalling result: %v", err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decoding result %s: %v", encoded, err)
	}
	return out
}

// callExpectingError invokes the handler and requires a CALLERROR.
func callExpectingError(t *testing.T, h *v16.Handler, action string, payload any) *ocpp.RPCError {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}

	_, rpcErr := h.HandleCall(context.Background(), testCP, action, raw)
	if rpcErr == nil {
		t.Fatalf("%s succeeded, expected an error", action)
	}
	return rpcErr
}

func TestMapStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ocpp string
		want core.EVSEStatus
	}{
		{v16.StatusAvailable, core.StatusAvailable},
		// A car is plugged in but no energy flows; the connector can still be
		// started, so AVAILABLE is the honest answer.
		{v16.StatusPreparing, core.StatusAvailable},
		{v16.StatusFinishing, core.StatusAvailable},
		{v16.StatusCharging, core.StatusCharging},
		// Suspensions are pauses inside a session, not the end of one.
		{v16.StatusSuspendedEV, core.StatusCharging},
		{v16.StatusSuspendedEVSE, core.StatusCharging},
		{v16.StatusReserved, core.StatusReserved},
		{v16.StatusUnavailable, core.StatusInoperative},
		{v16.StatusFaulted, core.StatusOutOfOrder},
		{"SomethingNew", core.StatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.ocpp, func(t *testing.T) {
			t.Parallel()

			if got := v16.MapStatus(tt.ocpp); got != tt.want {
				t.Errorf("MapStatus(%q) = %s, want %s", tt.ocpp, got, tt.want)
			}
		})
	}
}

func TestBootNotification(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)

	conf := call[v16.BootNotificationConf](t, h, v16.ActionBootNotification, v16.BootNotificationReq{
		ChargePointVendor: "Alpitronic",
		ChargePointModel:  "HYC300",
		FirmwareVersion:   "1.2.3",
	})

	if conf.Status != "Accepted" {
		t.Errorf("status = %q, want Accepted", conf.Status)
	}
	if conf.Interval != 60 {
		t.Errorf("interval = %d, want the configured 60", conf.Interval)
	}
	// OCPP wants ISO 8601 in UTC; a local-time stamp is a classic interop bug.
	if conf.CurrentTime != "2026-08-14T12:30:00Z" {
		t.Errorf("currentTime = %q, want an RFC3339 UTC stamp", conf.CurrentTime)
	}

	snap, _ := svc.Snapshot().ChargePoint(testCP)
	if snap.Boot.Vendor != "Alpitronic" || snap.Boot.Model != "HYC300" || snap.Boot.FirmwareVersion != "1.2.3" {
		t.Errorf("boot info = %+v", snap.Boot)
	}
}

func TestBootNotificationRequiresVendorAndModel(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	for _, req := range []v16.BootNotificationReq{
		{ChargePointModel: "HYC300"},
		{ChargePointVendor: "Alpitronic"},
	} {
		rpcErr := callExpectingError(t, h, v16.ActionBootNotification, req)
		// A missing required field is an occurrence-constraint violation, as
		// distinct from a field that is present but wrong.
		if rpcErr.Code != ocpp.ErrOccurrenceConstraintViolation {
			t.Errorf("code = %s, want %s", rpcErr.Code, ocpp.ErrOccurrenceConstraintViolation)
		}
	}
}

func TestHeartbeat(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	conf := call[v16.HeartbeatConf](t, h, v16.ActionHeartbeat, struct{}{})
	if conf.CurrentTime != "2026-08-14T12:30:00Z" {
		t.Errorf("currentTime = %q", conf.CurrentTime)
	}
}

func TestStatusNotification(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)
	svc.Connected(testCP)

	call[struct{}](t, h, v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 1, ErrorCode: "NoError", Status: v16.StatusCharging,
	})

	if got, _ := svc.EVSEStatus("EVSE-1"); got != core.StatusCharging {
		t.Errorf("EVSE-1 = %s, want CHARGING", got)
	}
}

func TestStatusNotificationRejectsBadValues(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	tests := []struct {
		name string
		req  v16.StatusNotificationReq
		want ocpp.ErrorCode
	}{{
		name: "unknown status",
		req:  v16.StatusNotificationReq{ConnectorID: 1, Status: "Melting"},
		want: ocpp.ErrPropertyConstraintViolation,
	}, {
		name: "negative connector",
		req:  v16.StatusNotificationReq{ConnectorID: -1, Status: v16.StatusAvailable},
		want: ocpp.ErrPropertyConstraintViolation,
	}, {
		name: "missing status",
		req:  v16.StatusNotificationReq{ConnectorID: 1},
		want: ocpp.ErrOccurrenceConstraintViolation,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := callExpectingError(t, h, v16.ActionStatusNotification, tt.req).Code; got != tt.want {
				t.Errorf("code = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	t.Run("configured tag", func(t *testing.T) {
		conf := call[v16.AuthorizeConf](t, h, v16.ActionAuthorize, v16.AuthorizeReq{IDTag: testTag})
		if conf.IDTagInfo.Status != v16.AuthAccepted {
			t.Errorf("status = %q, want Accepted", conf.IDTagInfo.Status)
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		// OCPP idTags are case-insensitive, and readers differ on casing.
		conf := call[v16.AuthorizeConf](t, h, v16.ActionAuthorize, v16.AuthorizeReq{IDTag: "04a1b2c3d4"})
		if conf.IDTagInfo.Status != v16.AuthAccepted {
			t.Errorf("status = %q, want Accepted for a differently-cased tag", conf.IDTagInfo.Status)
		}
	})

	t.Run("unknown tag", func(t *testing.T) {
		conf := call[v16.AuthorizeConf](t, h, v16.ActionAuthorize, v16.AuthorizeReq{IDTag: "DEADBEEF"})
		if conf.IDTagInfo.Status != v16.AuthInvalid {
			t.Errorf("status = %q, want Invalid", conf.IDTagInfo.Status)
		}
	})

	t.Run("missing tag", func(t *testing.T) {
		if got := callExpectingError(t, h, v16.ActionAuthorize, v16.AuthorizeReq{}).Code; got != ocpp.ErrOccurrenceConstraintViolation {
			t.Errorf("code = %s", got)
		}
	})
}

func TestStartAndStopTransaction(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)
	svc.Connected(testCP)

	start := call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: testTag, MeterStart: 1000,
		Timestamp: "2026-08-14T12:00:00Z",
	})
	if start.IDTagInfo.Status != v16.AuthAccepted || start.TransactionID < 1 {
		t.Fatalf("start = %+v, want an accepted transaction with a positive id", start)
	}

	snap, _ := svc.Snapshot().ChargePoint(testCP)
	if len(snap.Transactions) != 1 {
		t.Fatalf("expected one transaction, got %d", len(snap.Transactions))
	}
	tx := snap.Transactions[0]
	if tx.EVSEUID != "EVSE-1" || tx.MeterStart != 1000 {
		t.Errorf("transaction = %+v", tx)
	}
	// The charger's own timestamp is authoritative when it sends one.
	if !tx.StartedAt.Equal(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("startedAt = %s, want the charger's timestamp", tx.StartedAt)
	}

	call[v16.StopTransactionConf](t, h, v16.ActionStopTransaction, v16.StopTransactionReq{
		TransactionID: start.TransactionID, MeterStop: 4200, Reason: "Local",
	})

	snap, _ = svc.Snapshot().ChargePoint(testCP)
	if snap.Transactions[0].Active() {
		t.Error("transaction should be closed")
	}
}

func TestStartTransactionRefusesUnknownTag(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)

	// A well-formed request with a tag we do not accept gets a refusal in the
	// payload, not a CALLERROR: the message was fine, the answer is just no.
	conf := call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: "DEADBEEF",
	})
	if conf.IDTagInfo.Status != v16.AuthInvalid {
		t.Errorf("status = %q, want Invalid", conf.IDTagInfo.Status)
	}

	snap, _ := svc.Snapshot().ChargePoint(testCP)
	if len(snap.Transactions) != 0 {
		t.Error("a refused transaction must not be recorded")
	}
}

func TestStartTransactionRejectsConnectorZero(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	// Connector 0 addresses the station, so it cannot host a transaction.
	rpcErr := callExpectingError(t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 0, IDTag: testTag,
	})
	if rpcErr.Code != ocpp.ErrPropertyConstraintViolation {
		t.Errorf("code = %s", rpcErr.Code)
	}
}

func TestStopUnknownTransactionIsAccepted(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	// The spec gives us no way to say "no such transaction", and refusing
	// would make the charger retry forever.
	call[v16.StopTransactionConf](t, h, v16.ActionStopTransaction, v16.StopTransactionReq{
		TransactionID: 4242, MeterStop: 1,
	})
}

func TestMeterValues(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)
	events, cancel := svc.Subscribe("test")
	defer cancel()

	call[struct{}](t, h, v16.ActionMeterValues, v16.MeterValuesReq{
		ConnectorID: 1,
		MeterValue: []v16.MeterValue{{
			Timestamp: "2026-08-14T12:00:00Z",
			SampledValue: []v16.SampledValue{
				{Value: "1234", Measurand: "Energy.Active.Import.Register", Unit: "Wh"},
			},
		}},
	})

	select {
	case e := <-events:
		if e.Kind != core.EventMeterValues || e.EVSEUID != "EVSE-1" {
			t.Fatalf("event = %+v", e)
		}
		if e.Detail != "Energy.Active.Import.Register=1234Wh" {
			t.Errorf("summary = %q", e.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("no meter values event")
	}
}

func TestDataTransferIsRejected(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	// We implement no vendor extensions, and saying so is the correct answer.
	conf := call[v16.DataTransferConf](t, h, v16.ActionDataTransfer, v16.DataTransferReq{
		VendorID: "com.alpitronic", MessageID: "Something",
	})
	if conf.Status != "Rejected" {
		t.Errorf("status = %q, want Rejected", conf.Status)
	}
}

func TestFirmwareAndDiagnosticsStatusAreAccepted(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	for _, action := range []string{
		v16.ActionFirmwareStatusNotification,
		v16.ActionDiagnosticsStatusNotification,
	} {
		call[struct{}](t, h, action, v16.StatusOnlyReq{Status: "Idle"})
	}
}

func TestUnknownActionIsNotImplemented(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	if got := callExpectingError(t, h, "LogStatusNotification", struct{}{}).Code; got != ocpp.ErrNotImplemented {
		t.Errorf("code = %s, want %s", got, ocpp.ErrNotImplemented)
	}
}

func TestMalformedPayloadIsFormationViolation(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	// A valid frame whose payload does not fit the action — as opposed to a
	// frame that is not valid RPC at all, which the transport rejects.
	_, rpcErr := h.HandleCall(context.Background(), testCP, v16.ActionStatusNotification,
		json.RawMessage(`{"connectorId":"not a number"}`))
	if rpcErr == nil {
		t.Fatal("expected an error")
	}
	if rpcErr.Code != ocpp.ErrFormationViolation {
		t.Errorf("code = %s, want %s", rpcErr.Code, ocpp.ErrFormationViolation)
	}
}

func TestUnknownFieldsAreTolerated(t *testing.T) {
	t.Parallel()

	h, _ := newHandler(t)

	// Chargers add vendor extensions; rejecting a BootNotification over one
	// extra key would be a spectacular own goal.
	_, rpcErr := h.HandleCall(context.Background(), testCP, v16.ActionBootNotification,
		json.RawMessage(`{"chargePointVendor":"Alpitronic","chargePointModel":"HYC","hypercharger":{"x":1}}`))
	if rpcErr != nil {
		t.Errorf("unexpected %s: %s", rpcErr.Code, rpcErr.Description)
	}
}
