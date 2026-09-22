package core_test

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
)

const testCP = "ALP-HYC-001"

func testConfig() *config.Config {
	return &config.Config{
		Charger: config.Charger{ID: testCP},
		Location: config.Location{
			ID: "OFFICE-01",
			EVSEs: []config.EVSE{
				{UID: "EVSE-1", OCPPConnectorID: 1},
				{UID: "EVSE-2", OCPPConnectorID: 2},
			},
		},
	}
}

// collect gathers every event that arrives within a short window. Tests assert
// on the whole batch, so an unexpected extra event fails rather than hiding.
func collect(t *testing.T, ch <-chan core.Event) []core.Event {
	t.Helper()

	var out []core.Event
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-ch:
			out = append(out, e)
			// Keep draining briefly so a second, unwanted event is seen too.
			deadline = time.After(50 * time.Millisecond)
		case <-deadline:
			return out
		}
	}
}

func kinds(events []core.Event) []core.EventKind {
	out := make([]core.EventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func statusOf(t *testing.T, s *core.Service, uid string) core.EVSEStatus {
	t.Helper()

	st, ok := s.EVSEStatus(uid)
	if !ok {
		t.Fatalf("EVSE %q is not known to the service", uid)
	}
	return st
}

func TestConfiguredEVSEsExistBeforeTheChargerConnects(t *testing.T) {
	t.Parallel()

	// The OCPI Locations module must be able to serve the station before the
	// charger has ever dialled in.
	s := core.New(testConfig())

	for _, uid := range []string{"EVSE-1", "EVSE-2"} {
		if got := statusOf(t, s, uid); got != core.StatusUnknown {
			t.Errorf("%s = %s, want %s before connection", uid, got, core.StatusUnknown)
		}
	}
	if s.IsOnline(testCP) {
		t.Error("charge point should not be online before it connects")
	}
}

func TestStatusNotificationUpdatesEVSE(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	ch, cancel := s.Subscribe("test")
	defer cancel()

	s.Connected(testCP)
	if got := kinds(collect(t, ch)); len(got) != 1 || got[0] != core.EventChargePointConnected {
		t.Fatalf("connecting emitted %v, want exactly [charge_point_connected]", got)
	}

	s.SetConnectorStatus(testCP, 1, core.StatusCharging, "NoError")

	events := collect(t, ch)
	if len(events) != 1 {
		t.Fatalf("expected exactly one event, got %v", kinds(events))
	}
	e := events[0]
	if e.Kind != core.EventEVSEStatusChanged || e.EVSEUID != "EVSE-1" ||
		e.From != core.StatusUnknown || e.To != core.StatusCharging {
		t.Errorf("got %+v, want EVSE-1 UNKNOWN→CHARGING", e)
	}

	if got := statusOf(t, s, "EVSE-1"); got != core.StatusCharging {
		t.Errorf("EVSE-1 = %s, want CHARGING", got)
	}
	// The other connector must not have moved.
	if got := statusOf(t, s, "EVSE-2"); got != core.StatusUnknown {
		t.Errorf("EVSE-2 = %s, want UNKNOWN", got)
	}
}

func TestRepeatedStatusEmitsNoEvent(t *testing.T) {
	t.Parallel()

	// Chargers re-send StatusNotification liberally. Without this, the OCPI
	// push client would PATCH the backend on every repeat.
	s := core.New(testConfig())
	s.Connected(testCP)
	s.SetConnectorStatus(testCP, 1, core.StatusCharging, "NoError")

	ch, cancel := s.Subscribe("test")
	defer cancel()

	s.SetConnectorStatus(testCP, 1, core.StatusCharging, "NoError")
	s.SetConnectorStatus(testCP, 1, core.StatusCharging, "NoError")

	if got := collect(t, ch); len(got) != 0 {
		t.Errorf("repeating a status emitted %v, want nothing", kinds(got))
	}
}

func TestConnectorZeroDarkensEveryEVSE(t *testing.T) {
	t.Parallel()

	// OCPP 1.6 addresses the station as a whole with connector 0. A faulted
	// station must not keep advertising usable connectors over OCPI.
	s := core.New(testConfig())
	s.Connected(testCP)
	s.SetConnectorStatus(testCP, 1, core.StatusAvailable, "NoError")
	s.SetConnectorStatus(testCP, 2, core.StatusCharging, "NoError")

	ch, cancel := s.Subscribe("test")
	defer cancel()

	s.SetConnectorStatus(testCP, 0, core.StatusOutOfOrder, "GroundFailure")

	events := collect(t, ch)
	if len(events) != 3 {
		t.Fatalf("expected a station change plus two EVSE changes, got %v", kinds(events))
	}
	if events[0].Kind != core.EventStationStatusChanged {
		t.Errorf("first event = %s, want station_status_changed", events[0].Kind)
	}
	for _, uid := range []string{"EVSE-1", "EVSE-2"} {
		if got := statusOf(t, s, uid); got != core.StatusOutOfOrder {
			t.Errorf("%s = %s, want OUTOFORDER while the station is faulted", uid, got)
		}
	}

	// Recovering the station restores each connector's own status.
	s.SetConnectorStatus(testCP, 0, core.StatusAvailable, "NoError")
	if got := statusOf(t, s, "EVSE-2"); got != core.StatusCharging {
		t.Errorf("EVSE-2 = %s, want its own CHARGING back after recovery", got)
	}
}

func TestDisconnectMakesEverythingUnknown(t *testing.T) {
	t.Parallel()

	// A station we cannot reach tells us nothing about its connectors, so the
	// last known status must not keep being served.
	s := core.New(testConfig())
	s.Connected(testCP)
	s.SetConnectorStatus(testCP, 1, core.StatusCharging, "NoError")

	ch, cancel := s.Subscribe("test")
	defer cancel()

	s.Disconnected(testCP, "heartbeat timeout")

	events := collect(t, ch)
	if len(events) != 2 {
		t.Fatalf("expected a disconnect plus one EVSE change, got %v", kinds(events))
	}
	if events[0].Detail != "heartbeat timeout" {
		t.Errorf("disconnect reason = %q, want %q", events[0].Detail, "heartbeat timeout")
	}
	if got := statusOf(t, s, "EVSE-1"); got != core.StatusUnknown {
		t.Errorf("EVSE-1 = %s, want UNKNOWN after disconnect", got)
	}

	// Disconnecting twice must not emit a second round.
	s.Disconnected(testCP, "again")
	if got := collect(t, ch); len(got) != 0 {
		t.Errorf("second disconnect emitted %v, want nothing", kinds(got))
	}
}

func TestTransactionLifecycle(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	s.Connected(testCP)

	ch, cancel := s.Subscribe("test")
	defer cancel()

	start := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	id := s.StartTransaction(testCP, 1, "04A1B2C3D4", 1000, start)
	if id < 1 {
		t.Fatalf("transaction id = %d, want a positive id", id)
	}

	second := s.StartTransaction(testCP, 2, "04A1B2C3D4", 0, start)
	if second == id {
		t.Errorf("two transactions share id %d", id)
	}

	tx, ok := s.StopTransaction(testCP, id, 4200, "Local", start.Add(time.Hour))
	if !ok {
		t.Fatal("stopping a live transaction should succeed")
	}
	if tx.MeterStop != 4200 || tx.Reason != "Local" || tx.Active() {
		t.Errorf("stopped transaction = %+v, want meterStop 4200, reason Local, not active", tx)
	}

	// Stopping it again, or stopping one that never existed, must be refused
	// rather than silently accepted.
	if _, ok := s.StopTransaction(testCP, id, 4200, "Local", start); ok {
		t.Error("stopping an already-stopped transaction should fail")
	}
	if _, ok := s.StopTransaction(testCP, 9999, 0, "Local", start); ok {
		t.Error("stopping an unknown transaction should fail")
	}

	got := kinds(collect(t, ch))
	want := []core.EventKind{
		core.EventTransactionStarted, core.EventTransactionStarted, core.EventTransactionStopped,
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestUnconfiguredConnectorGetsASyntheticUID(t *testing.T) {
	t.Parallel()

	// A charger reporting a connector we were not configured for must still be
	// observable, or the mismatch is invisible.
	s := core.New(testConfig())
	s.Connected(testCP)
	s.SetConnectorStatus(testCP, 7, core.StatusAvailable, "NoError")

	if got := statusOf(t, s, testCP+"-7"); got != core.StatusAvailable {
		t.Errorf("synthetic EVSE = %s, want AVAILABLE", got)
	}
}

func TestSnapshotIsAnIndependentCopy(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	s.Connected(testCP)
	s.SetConnectorStatus(testCP, 1, core.StatusCharging, "NoError")

	snap := s.Snapshot()
	cp, ok := snap.ChargePoint(testCP)
	if !ok {
		t.Fatal("snapshot is missing the configured charge point")
	}
	if !cp.Online || len(cp.EVSEs) != 2 {
		t.Fatalf("snapshot = %+v, want online with 2 EVSEs", cp)
	}
	// EVSE order follows config order, which is what the OCPI module and the
	// TUI both render.
	if cp.EVSEs[0].UID != "EVSE-1" || cp.EVSEs[1].UID != "EVSE-2" {
		t.Errorf("EVSE order = %s, %s; want EVSE-1, EVSE-2", cp.EVSEs[0].UID, cp.EVSEs[1].UID)
	}

	// Mutating the copy must not reach back into the service.
	cp.EVSEs[0].Status = core.StatusOutOfOrder
	if got := statusOf(t, s, "EVSE-1"); got != core.StatusCharging {
		t.Errorf("service state changed through a snapshot: EVSE-1 = %s", got)
	}
}

func TestSlowSubscriberDropsEventsInsteadOfBlocking(t *testing.T) {
	t.Parallel()

	// A stalled TUI must never be able to wedge the WebSocket reader that is
	// publishing these events.
	s := core.New(testConfig())
	_, cancel := s.Subscribe("stalled")
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			s.Heartbeat(testCP)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber that never reads")
	}

	if s.Drops()["stalled"] == 0 {
		t.Error("expected the stalled subscriber to have lost events")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	ch, cancel := s.Subscribe("test")

	cancel()
	cancel() // must be idempotent

	s.Heartbeat(testCP)

	if _, open := <-ch; open {
		t.Error("channel should be closed and empty after unsubscribing")
	}
}

func TestClockIsInjectable(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	s := core.New(testConfig(), core.WithClock(func() time.Time { return at }))

	ch, cancel := s.Subscribe("test")
	defer cancel()

	s.Connected(testCP)

	events := collect(t, ch)
	if len(events) == 0 {
		t.Fatal("expected an event")
	}
	if !events[0].At.Equal(at) {
		t.Errorf("event time = %s, want the injected %s", events[0].At, at)
	}
}
