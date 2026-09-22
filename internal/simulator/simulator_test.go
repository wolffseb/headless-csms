package simulator_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

const (
	testCP  = "ALP-HYC-001"
	testTag = "04A1B2C3D4"
)

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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// rig is a real CSMS with a real 1.6 handler, and a simulator connected to it.
type rig struct {
	server *csms.Server
	core   *core.Service
	sim    *simulator.Simulator
}

func newRig(t *testing.T, mutate ...func(*simulator.Options)) *rig {
	t.Helper()

	cfg := testConfig()
	svc := core.New(cfg)
	handler := v16.NewHandler(cfg, svc, discard())

	server, err := csms.New(csms.Options{
		Bind:        "127.0.0.1:0",
		Core:        svc,
		Handlers:    map[ocpp.Version]ocpp.Handler{ocpp.Version16: handler},
		CallTimeout: 2 * time.Second,
		IdleTimeout: 30 * time.Second,
		Log:         discard(),
	})
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

	opts := simulator.Options{
		URL:        "ws://" + server.Addr(),
		ID:         testCP,
		Connectors: 2,
		IDTag:      testTag,
		Log:        discard(),
	}
	for _, m := range mutate {
		m(&opts)
	}

	sim, err := simulator.New(opts)
	if err != nil {
		t.Fatalf("simulator.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sim.Close()
		sim.Wait()
	})
	if err := sim.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	return &rig{server: server, core: svc, sim: sim}
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

// conn is the CSMS-side connection, which is how tests send commands.
func (r *rig) conn(t *testing.T) interface {
	Call(context.Context, string, any) (json.RawMessage, error)
} {
	t.Helper()

	waitFor(t, "the connection to register", func() bool {
		_, ok := r.server.ChargePoint(testCP)
		return ok
	})
	c, _ := r.server.ChargePoint(testCP)
	return c
}

func (r *rig) call(t *testing.T, action string, payload, out any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	raw, err := r.conn(t).Call(ctx, action, payload)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s response: %v", action, err)
		}
	}
}

// TestSimulatorAppearsAsAConnectedStation is the headline criterion: the
// simulator alone is enough to make a station show up in core.
func TestSimulatorAppearsAsAConnectedStation(t *testing.T) {
	t.Parallel()

	r := newRig(t)

	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })
	waitFor(t, "both EVSEs to report available", func() bool {
		a, _ := r.core.EVSEStatus("EVSE-1")
		b, _ := r.core.EVSEStatus("EVSE-2")
		return a == core.StatusAvailable && b == core.StatusAvailable
	})

	snap, ok := r.core.Snapshot().ChargePoint(testCP)
	if !ok {
		t.Fatal("charge point missing from the snapshot")
	}
	if snap.Boot.Vendor == "" || snap.Boot.Model == "" {
		t.Errorf("boot info not recorded: %+v", snap.Boot)
	}
}

func TestPluggingInIsObservable(t *testing.T) {
	t.Parallel()

	// Standing in for someone walking up and plugging a car in.
	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	if err := r.sim.SetConnectorStatus(context.Background(), 1, v16.StatusCharging); err != nil {
		t.Fatalf("set status: %v", err)
	}

	waitFor(t, "EVSE-1 to report charging", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusCharging
	})
	// The other connector must not have moved.
	if st, _ := r.core.EVSEStatus("EVSE-2"); st != core.StatusAvailable {
		t.Errorf("EVSE-2 = %s, want AVAILABLE", st)
	}
}

func TestReserveNowIsAccepted(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	var conf v16.ReserveNowConf
	r.call(t, v16.ActionReserveNow, v16.ReserveNowReq{
		ConnectorID:   1,
		ExpiryDate:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		IDTag:         testTag,
		ReservationID: 42,
	}, &conf)

	if conf.Status != v16.ReservationAccepted {
		t.Fatalf("status = %q, want Accepted", conf.Status)
	}
	if got := r.sim.ReservationID(1); got != 42 {
		t.Errorf("simulator holds reservation %d, want 42", got)
	}
	waitFor(t, "EVSE-1 to report reserved", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusReserved
	})
}

func TestReserveNowScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		scenario simulator.Scenario
		want     string
	}{
		{simulator.ScenarioRejectReserve, v16.ReservationRejected},
		{simulator.ScenarioOccupied, v16.ReservationOccupied},
	}

	for _, tt := range tests {
		t.Run(string(tt.scenario), func(t *testing.T) {
			t.Parallel()

			r := newRig(t, func(o *simulator.Options) { o.Scenario = tt.scenario })
			waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

			var conf v16.ReserveNowConf
			r.call(t, v16.ActionReserveNow, v16.ReserveNowReq{
				ConnectorID: 1, IDTag: testTag, ReservationID: 1,
			}, &conf)

			if conf.Status != tt.want {
				t.Errorf("status = %q, want %q", conf.Status, tt.want)
			}
			if got := r.sim.ReservationID(1); got != 0 {
				t.Errorf("a refused reservation was still recorded as %d", got)
			}
		})
	}
}

func TestReserveNowOnABusyConnector(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}

	var conf v16.ReserveNowConf
	r.call(t, v16.ActionReserveNow, v16.ReserveNowReq{
		ConnectorID: 1, IDTag: testTag, ReservationID: 7,
	}, &conf)

	if conf.Status != v16.ReservationOccupied {
		t.Errorf("status = %q, want Occupied for a connector mid-session", conf.Status)
	}
}

func TestCancelReservation(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	var reserve v16.ReserveNowConf
	r.call(t, v16.ActionReserveNow, v16.ReserveNowReq{
		ConnectorID: 2, IDTag: testTag, ReservationID: 99,
	}, &reserve)
	if reserve.Status != v16.ReservationAccepted {
		t.Fatalf("reserve = %q", reserve.Status)
	}

	var cancel v16.CancelReservationConf
	r.call(t, v16.ActionCancelReservation, v16.CancelReservationReq{ReservationID: 99}, &cancel)
	if cancel.Status != v16.CmdAccepted {
		t.Errorf("cancel = %q, want Accepted", cancel.Status)
	}
	waitFor(t, "EVSE-2 to be free again", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-2")
		return st == core.StatusAvailable
	})

	// Cancelling a reservation that is not held must be refused.
	var again v16.CancelReservationConf
	r.call(t, v16.ActionCancelReservation, v16.CancelReservationReq{ReservationID: 99}, &again)
	if again.Status != v16.CmdRejected {
		t.Errorf("second cancel = %q, want Rejected", again.Status)
	}
}

// TestRemoteStartRedeemsTheReservation is the flow the whole tool exists for:
// reserve, then redeem with the configured tag.
func TestRemoteStartRedeemsTheReservation(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	var reserve v16.ReserveNowConf
	r.call(t, v16.ActionReserveNow, v16.ReserveNowReq{
		ConnectorID: 1, IDTag: testTag, ReservationID: 5,
	}, &reserve)
	if reserve.Status != v16.ReservationAccepted {
		t.Fatalf("reserve = %q", reserve.Status)
	}
	waitFor(t, "EVSE-1 to report reserved", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusReserved
	})

	one := 1
	var start v16.RemoteStartTransactionConf
	r.call(t, v16.ActionRemoteStartTransaction,
		v16.RemoteStartTransactionReq{ConnectorID: &one, IDTag: testTag}, &start)
	if start.Status != v16.CmdAccepted {
		t.Fatalf("remote start = %q, want Accepted", start.Status)
	}

	waitFor(t, "the session to start", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusCharging
	})
	if got := r.sim.ReservationID(1); got != 0 {
		t.Errorf("reservation %d survived the session starting", got)
	}

	txID := r.sim.TransactionID(1)
	if txID == 0 {
		t.Fatal("simulator has no transaction id")
	}
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	if len(snap.Transactions) != 1 || snap.Transactions[0].ID != txID {
		t.Fatalf("csms transactions = %+v, want one with id %d", snap.Transactions, txID)
	}

	var stop v16.RemoteStopTransactionConf
	r.call(t, v16.ActionRemoteStopTransaction,
		v16.RemoteStopTransactionReq{TransactionID: txID}, &stop)
	if stop.Status != v16.CmdAccepted {
		t.Fatalf("remote stop = %q", stop.Status)
	}

	waitFor(t, "the session to end", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusAvailable
	})
	waitFor(t, "the csms to close the transaction", func() bool {
		snap, _ := r.core.Snapshot().ChargePoint(testCP)
		return len(snap.Transactions) == 1 && !snap.Transactions[0].Active()
	})
}

func TestRemoteStartWithAnUnknownTagIsRefused(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	one := 1
	var start v16.RemoteStartTransactionConf
	r.call(t, v16.ActionRemoteStartTransaction,
		v16.RemoteStartTransactionReq{ConnectorID: &one, IDTag: "DEADBEEF"}, &start)

	// The station accepts the request; the CSMS refuses the token when the
	// StartTransaction arrives, so no session may result.
	if start.Status != v16.CmdAccepted {
		t.Fatalf("remote start = %q", start.Status)
	}
	waitFor(t, "the connector to settle back to available", func() bool {
		return r.sim.ConnectorStatus(1) == v16.StatusAvailable
	})
	if got := r.sim.TransactionID(1); got != 0 {
		t.Errorf("a refused token still opened transaction %d", got)
	}
}

func TestRemoteStopOfAnUnknownTransaction(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	var stop v16.RemoteStopTransactionConf
	r.call(t, v16.ActionRemoteStopTransaction,
		v16.RemoteStopTransactionReq{TransactionID: 4242}, &stop)
	if stop.Status != v16.CmdRejected {
		t.Errorf("status = %q, want Rejected", stop.Status)
	}
}

func TestUnlockConnector(t *testing.T) {
	t.Parallel()

	t.Run("normal", func(t *testing.T) {
		t.Parallel()

		r := newRig(t)
		waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

		var conf v16.UnlockConnectorConf
		r.call(t, v16.ActionUnlockConnector, v16.UnlockConnectorReq{ConnectorID: 1}, &conf)
		if conf.Status != v16.UnlockUnlocked {
			t.Errorf("status = %q, want Unlocked", conf.Status)
		}
	})

	t.Run("scenario unlock-fails", func(t *testing.T) {
		t.Parallel()

		r := newRig(t, func(o *simulator.Options) { o.Scenario = simulator.ScenarioUnlockFails })
		waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

		var conf v16.UnlockConnectorConf
		r.call(t, v16.ActionUnlockConnector, v16.UnlockConnectorReq{ConnectorID: 1}, &conf)
		if conf.Status != v16.UnlockFailed {
			t.Errorf("status = %q, want UnlockFailed", conf.Status)
		}
	})
}

// TestGetConfigurationReportsFeatureProfiles covers what `cpms probe` will use
// to find out whether the real station supports reservations at all.
func TestGetConfigurationReportsFeatureProfiles(t *testing.T) {
	t.Parallel()

	t.Run("reservation supported", func(t *testing.T) {
		t.Parallel()

		r := newRig(t)
		waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

		var conf v16.GetConfigurationConf
		r.call(t, v16.ActionGetConfiguration, v16.GetConfigurationReq{}, &conf)

		profiles := configValue(conf, v16.KeySupportedFeatureProfiles)
		if profiles == "" {
			t.Fatalf("no %s key in %+v", v16.KeySupportedFeatureProfiles, conf)
		}
		if !strings.Contains(profiles, "Reservation") {
			t.Errorf("profiles = %q, want Reservation among them", profiles)
		}
	})

	t.Run("reservation unsupported", func(t *testing.T) {
		t.Parallel()

		// A station that refuses reservations must not advertise the profile,
		// or probing it would give the wrong answer.
		r := newRig(t, func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectReserve })
		waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

		var conf v16.GetConfigurationConf
		r.call(t, v16.ActionGetConfiguration, v16.GetConfigurationReq{}, &conf)

		if profiles := configValue(conf, v16.KeySupportedFeatureProfiles); strings.Contains(profiles, "Reservation") {
			t.Errorf("profiles = %q, should not advertise Reservation", profiles)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		t.Parallel()

		r := newRig(t)
		waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

		var conf v16.GetConfigurationConf
		r.call(t, v16.ActionGetConfiguration,
			v16.GetConfigurationReq{Key: []string{v16.KeyNumberOfConnectors, "NoSuchKey"}}, &conf)

		if len(conf.ConfigurationKey) != 1 || conf.ConfigurationKey[0].Key != v16.KeyNumberOfConnectors {
			t.Errorf("keys = %+v", conf.ConfigurationKey)
		}
		if len(conf.UnknownKey) != 1 || conf.UnknownKey[0] != "NoSuchKey" {
			t.Errorf("unknown = %v", conf.UnknownKey)
		}
	})
}

func TestSlowScenarioOutlastsTheCallTimeout(t *testing.T) {
	t.Parallel()

	// The CSMS call timeout in the rig is 2s; a 3s stall must trip it.
	r := newRig(t, func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 3 * time.Second
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := r.conn(t).Call(ctx, v16.ActionUnlockConnector, v16.UnlockConnectorReq{ConnectorID: 1})
	if err == nil {
		t.Fatal("expected the call to time out against a slow station")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("call took %s; it should have given up after the 2s call timeout", elapsed)
	}
}

func TestTriggerMessage(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	var conf v16.TriggerMessageConf
	r.call(t, v16.ActionTriggerMessage,
		v16.TriggerMessageReq{RequestedMessage: v16.ActionHeartbeat}, &conf)
	if conf.Status != v16.TriggerAccepted {
		t.Fatalf("status = %q, want Accepted", conf.Status)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == core.EventHeartbeat {
				return
			}
		case <-deadline:
			t.Fatal("no heartbeat arrived after TriggerMessage")
		}
	}
}

func TestTriggerUnsupportedMessage(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	var conf v16.TriggerMessageConf
	r.call(t, v16.ActionTriggerMessage,
		v16.TriggerMessageReq{RequestedMessage: "DiagnosticsStatusNotification"}, &conf)
	if conf.Status != v16.TriggerNotImplemented {
		t.Errorf("status = %q, want NotImplemented", conf.Status)
	}
}

func TestChangeAvailability(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	var conf v16.ChangeAvailabilityConf
	r.call(t, v16.ActionChangeAvailability,
		v16.ChangeAvailabilityReq{ConnectorID: 1, Type: v16.AvailabilityInoperative}, &conf)
	if conf.Status != v16.CmdAccepted {
		t.Fatalf("status = %q", conf.Status)
	}

	waitFor(t, "EVSE-1 to go inoperative", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusInoperative
	})
}

func TestDisconnectIsObservable(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	r.sim.Close()

	waitFor(t, "the csms to notice", func() bool { return !r.core.IsOnline(testCP) })
	waitFor(t, "EVSE-1 to become unknown", func() bool {
		st, _ := r.core.EVSEStatus("EVSE-1")
		return st == core.StatusUnknown
	})
}

func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts simulator.Options
	}{
		{"no url", simulator.Options{ID: testCP}},
		{"no id", simulator.Options{URL: "ws://127.0.0.1:9000"}},
		{"unknown scenario", simulator.Options{URL: "ws://x:1", ID: "a", Scenario: "chaos"}},
		{"unimplemented version", simulator.Options{URL: "ws://x:1", ID: "a", Version: ocpp.Version201}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := simulator.New(tt.opts); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestConnectFailsAgainstNothing(t *testing.T) {
	t.Parallel()

	sim, err := simulator.New(simulator.Options{
		// Port 1 on loopback has nothing listening.
		URL: "ws://127.0.0.1:1", ID: testCP, Log: discard(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sim.Connect(ctx); err == nil {
		sim.Close()
		t.Fatal("expected dialling nothing to fail")
	}
}

func configValue(conf v16.GetConfigurationConf, key string) string {
	for _, k := range conf.ConfigurationKey {
		if k.Key == key {
			return k.Value
		}
	}
	return ""
}
