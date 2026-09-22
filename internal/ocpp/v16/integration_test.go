package v16_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

// stack is a real server, a real 1.6 handler and a real WebSocket client:
// everything except the charger itself.
type stack struct {
	server *csms.Server
	core   *core.Service
	client *ocpptest.Client
	events <-chan core.Event
}

func newStack(t *testing.T, mutate ...func(*csms.Options)) *stack {
	t.Helper()

	cfg := testConfig()
	svc := core.New(cfg)
	handler := v16.NewHandler(cfg, svc, slog.New(slog.NewTextHandler(io.Discard, nil)),
		v16.WithClock(func() time.Time { return testNow }))

	opts := csms.Options{
		Bind:        "127.0.0.1:0",
		Core:        svc,
		Handlers:    map[ocpp.Version]ocpp.Handler{ocpp.Version16: handler},
		Accept:      func(id string) bool { return id == testCP },
		CallTimeout: 2 * time.Second,
		IdleTimeout: 30 * time.Second,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range mutate {
		m(&opts)
	}

	server, err := csms.New(opts)
	if err != nil {
		t.Fatalf("csms.New: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	events, unsubscribe := svc.Subscribe("test")
	t.Cleanup(unsubscribe)

	client := ocpptest.MustDial(t, "ws://"+server.Addr()+"/ocpp/"+testCP)

	s := &stack{server: server, core: svc, client: client, events: events}
	// Synchronise on the connect event rather than on IsOnline: the flag is
	// set before the event is published, so polling it would race the drain
	// below and leave a stray event for the first assertion to trip over.
	s.waitForKind(t, core.EventChargePointConnected)
	s.drain()
	return s
}

// waitForKind consumes events until one of the given kind arrives.
func (s *stack) waitForKind(t *testing.T, kind core.EventKind) core.Event {
	t.Helper()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-s.events:
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s event", kind)
			return core.Event{}
		}
	}
}

// drain discards events queued so far, so a test can assert on just what it
// triggers next.
func (s *stack) drain() {
	for {
		select {
		case <-s.events:
		default:
			return
		}
	}
}

// next waits for the next event.
func (s *stack) next(t *testing.T) core.Event {
	t.Helper()

	select {
	case e := <-s.events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an event")
		return core.Event{}
	}
}

// expectNoMoreEvents asserts that nothing else arrives.
func (s *stack) expectNoMoreEvents(t *testing.T) {
	t.Helper()

	select {
	case e := <-s.events:
		t.Fatalf("unexpected extra event: %s", e)
	case <-time.After(150 * time.Millisecond):
	}
}

func (s *stack) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *stack) status(t *testing.T, uid string) core.EVSEStatus {
	t.Helper()

	st, ok := s.core.EVSEStatus(uid)
	if !ok {
		t.Fatalf("EVSE %q unknown", uid)
	}
	return st
}

func TestEndToEndBootNotification(t *testing.T) {
	t.Parallel()

	s := newStack(t)

	raw := s.client.MustCall(v16.ActionBootNotification, v16.BootNotificationReq{
		ChargePointVendor: "Alpitronic",
		ChargePointModel:  "HYC300",
	})

	var conf v16.BootNotificationConf
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if conf.Status != "Accepted" || conf.Interval != 60 {
		t.Errorf("boot response = %+v, want Accepted with a 60s interval", conf)
	}
	if _, err := time.Parse(time.RFC3339, conf.CurrentTime); err != nil {
		t.Errorf("currentTime %q is not RFC3339: %v", conf.CurrentTime, err)
	}

	if e := s.next(t); e.Kind != core.EventBootNotified {
		t.Errorf("event = %s, want boot_notified", e.Kind)
	}
}

func TestEndToEndStatusReachesCore(t *testing.T) {
	t.Parallel()

	// The headline path for this issue: a charger message becomes observable
	// domain state, exactly once.
	s := newStack(t)

	s.client.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 1, ErrorCode: "NoError", Status: v16.StatusCharging,
	})

	e := s.next(t)
	if e.Kind != core.EventEVSEStatusChanged || e.EVSEUID != "EVSE-1" || e.To != core.StatusCharging {
		t.Fatalf("event = %+v, want EVSE-1 → CHARGING", e)
	}
	if got := s.status(t, "EVSE-1"); got != core.StatusCharging {
		t.Errorf("EVSE-1 = %s, want CHARGING", got)
	}
	s.expectNoMoreEvents(t)

	// A repeat of the same status must be silent, or the OCPI push client
	// would PATCH the backend every time the charger repeats itself.
	s.client.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 1, ErrorCode: "NoError", Status: v16.StatusCharging,
	})
	s.expectNoMoreEvents(t)
}

func TestEndToEndStationFaultDarkensEveryEVSE(t *testing.T) {
	t.Parallel()

	s := newStack(t)

	s.client.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 1, ErrorCode: "NoError", Status: v16.StatusAvailable,
	})
	s.client.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 2, ErrorCode: "NoError", Status: v16.StatusAvailable,
	})
	s.drain()

	// Connector 0 is the station itself.
	s.client.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 0, ErrorCode: "GroundFailure", Status: v16.StatusFaulted,
	})

	s.waitFor(t, "both EVSEs to go out of order", func() bool {
		a, _ := s.core.EVSEStatus("EVSE-1")
		b, _ := s.core.EVSEStatus("EVSE-2")
		return a == core.StatusOutOfOrder && b == core.StatusOutOfOrder
	})
}

func TestEndToEndTransaction(t *testing.T) {
	t.Parallel()

	s := newStack(t)

	raw := s.client.MustCall(v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: testTag, MeterStart: 500,
		Timestamp: testNow.Format(time.RFC3339),
	})
	var start v16.StartTransactionConf
	if err := json.Unmarshal(raw, &start); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if start.IDTagInfo.Status != v16.AuthAccepted {
		t.Fatalf("start refused: %+v", start)
	}

	s.client.MustCall(v16.ActionStopTransaction, v16.StopTransactionReq{
		TransactionID: start.TransactionID, MeterStop: 9000, Reason: "Remote",
		Timestamp: testNow.Add(time.Hour).Format(time.RFC3339),
	})

	s.waitFor(t, "the transaction to close", func() bool {
		snap, ok := s.core.Snapshot().ChargePoint(testCP)
		return ok && len(snap.Transactions) == 1 && !snap.Transactions[0].Active()
	})

	snap, _ := s.core.Snapshot().ChargePoint(testCP)
	if got := snap.Transactions[0].MeterStop; got != 9000 {
		t.Errorf("meterStop = %d, want 9000", got)
	}
}

func TestEndToEndDisconnectBlanksStatus(t *testing.T) {
	t.Parallel()

	s := newStack(t)

	s.client.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 1, ErrorCode: "NoError", Status: v16.StatusCharging,
	})
	s.waitFor(t, "the status to land", func() bool {
		st, _ := s.core.EVSEStatus("EVSE-1")
		return st == core.StatusCharging
	})

	s.client.Close()

	s.waitFor(t, "the EVSE to become unknown", func() bool {
		st, _ := s.core.EVSEStatus("EVSE-1")
		return st == core.StatusUnknown
	})
	if s.core.IsOnline(testCP) {
		t.Error("charge point should be offline")
	}
}

func TestEndToEndHeartbeat(t *testing.T) {
	t.Parallel()

	s := newStack(t)

	raw := s.client.MustCall(v16.ActionHeartbeat, struct{}{})
	var conf v16.HeartbeatConf
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, conf.CurrentTime); err != nil {
		t.Errorf("currentTime %q is not RFC3339: %v", conf.CurrentTime, err)
	}
}

func TestEndToEndUnknownActionIsRejected(t *testing.T) {
	t.Parallel()

	s := newStack(t)

	resp := s.client.CallExpectingError("SignCertificate", struct{}{})
	if resp.ErrorCode != string(ocpp.ErrNotImplemented) {
		t.Errorf("code = %s, want %s", resp.ErrorCode, ocpp.ErrNotImplemented)
	}
}
