package v16_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// newChargePointFactory is the csms.Options.NewChargePoint that `cpms run`
// wires, reduced to the one version that exists.
func newChargePointFactory(cfg *config.Config) func(*ocppj.Conn) (ocpp.ChargePoint, error) {
	return func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
		return v16.NewChargePoint(conn, cfg), nil
	}
}

// simRig is a real CSMS, a real 1.6 handler and command adapter, and the
// simulator dialled in: the whole outbound path with only the charger faked.
type simRig struct {
	server *csms.Server
	sim    *simulator.Simulator
	cp     ocpp.ChargePoint
}

func newSimRig(t *testing.T, callTimeout time.Duration, mutate ...func(*simulator.Options)) *simRig {
	t.Helper()

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	svc := core.New(cfg)

	server, err := csms.New(csms.Options{
		Bind:           "127.0.0.1:0",
		Core:           svc,
		Handlers:       map[ocpp.Version]ocpp.Handler{ocpp.Version16: v16.NewHandler(cfg, svc, discard)},
		NewChargePoint: newChargePointFactory(cfg),
		CallTimeout:    callTimeout,
		IdleTimeout:    30 * time.Second,
		Log:            discard,
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
		Log:        discard,
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

	var cp ocpp.ChargePoint
	pollUntil(t, "the charge point to be commandable", func() bool {
		cp, err = server.Command(testCP)
		return err == nil
	})
	return &simRig{server: server, sim: sim, cp: cp}
}

func pollUntil(t *testing.T, what string, cond func() bool) {
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

func expectStatus(t *testing.T, what string, res ocpp.Result, err error, want ocpp.CommandStatus) {
	t.Helper()

	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if res.Status != want {
		t.Fatalf("%s = %s (raw %q), want %s", what, res.Status, res.Raw, want)
	}
}

func ctx3s(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestChargePointAgainstSimulator(t *testing.T) {
	t.Parallel()

	t.Run("reserve and cancel", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second)
		ctx := ctx3s(t)

		res, err := r.cp.ReserveNow(ctx, ocpp.ReserveRequest{
			ReservationID: 42, EVSEUID: "EVSE-1", IDTag: testTag, ExpiresAt: time.Now().Add(15 * time.Minute),
		})
		expectStatus(t, "ReserveNow", res, err, ocpp.CommandAccepted)
		if got := r.sim.ReservationID(1); got != 42 {
			t.Errorf("simulator holds reservation %d, want 42", got)
		}

		res, err = r.cp.CancelReservation(ctx, 42)
		expectStatus(t, "CancelReservation", res, err, ocpp.CommandAccepted)
		if got := r.sim.ReservationID(1); got != 0 {
			t.Errorf("simulator still holds reservation %d after cancelling", got)
		}
	})

	for _, tt := range []struct {
		scenario simulator.Scenario
		want     ocpp.CommandStatus
	}{
		{simulator.ScenarioRejectReserve, ocpp.CommandRejected},
		{simulator.ScenarioOccupied, ocpp.CommandOccupied},
	} {
		t.Run("reserve under "+string(tt.scenario), func(t *testing.T) {
			t.Parallel()

			r := newSimRig(t, 2*time.Second, func(o *simulator.Options) { o.Scenario = tt.scenario })
			res, err := r.cp.ReserveNow(ctx3s(t), ocpp.ReserveRequest{
				ReservationID: 1, EVSEUID: "EVSE-1", IDTag: testTag, ExpiresAt: time.Now().Add(time.Minute),
			})
			expectStatus(t, "ReserveNow", res, err, tt.want)
		})
	}

	t.Run("remote start and stop", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second)
		ctx := ctx3s(t)

		res, err := r.cp.RemoteStart(ctx, ocpp.RemoteStartRequest{RemoteStartID: 1, EVSEUID: "EVSE-2", IDTag: testTag})
		expectStatus(t, "RemoteStart", res, err, ocpp.CommandAccepted)

		// The answer comes before the StartTransaction it leads to.
		pollUntil(t, "the transaction to start", func() bool { return r.sim.TransactionID(2) != 0 })
		if got := r.sim.TransactionID(1); got != 0 {
			t.Errorf("a session started on connector 1 (%d) for a start on EVSE-2", got)
		}

		res, err = r.cp.RemoteStop(ctx, strconv.Itoa(r.sim.TransactionID(2)))
		expectStatus(t, "RemoteStop", res, err, ocpp.CommandAccepted)
		pollUntil(t, "the transaction to end", func() bool { return r.sim.TransactionID(2) == 0 })
	})

	t.Run("unlock", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second)
		res, err := r.cp.UnlockConnector(ctx3s(t), "EVSE-1")
		expectStatus(t, "UnlockConnector", res, err, ocpp.CommandUnlocked)
	})

	t.Run("unlock under unlock-fails", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second, func(o *simulator.Options) { o.Scenario = simulator.ScenarioUnlockFails })
		res, err := r.cp.UnlockConnector(ctx3s(t), "EVSE-1")
		expectStatus(t, "UnlockConnector", res, err, ocpp.CommandUnlockFailed)
	})

	t.Run("trigger status", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second)
		res, err := r.cp.TriggerStatus(ctx3s(t), "EVSE-1")
		expectStatus(t, "TriggerStatus", res, err, ocpp.CommandAccepted)
	})

	t.Run("capabilities", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second)
		caps, err := r.cp.Capabilities(ctx3s(t))
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		if !caps.Reservation || !caps.RemoteTrigger {
			t.Errorf("caps = %+v, want Reservation and RemoteTrigger", caps)
		}
	})

	t.Run("capabilities under reject-reserve", func(t *testing.T) {
		t.Parallel()

		r := newSimRig(t, 2*time.Second, func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectReserve })
		caps, err := r.cp.Capabilities(ctx3s(t))
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		if caps.Reservation {
			t.Errorf("caps = %+v, want Reservation false", caps)
		}
	})
}

func TestCommandForAnAbsentChargePoint(t *testing.T) {
	t.Parallel()

	r := newSimRig(t, 2*time.Second)

	if _, err := r.server.Command("NO-SUCH-CP"); !errors.Is(err, ocpp.ErrNotConnected) {
		t.Errorf("unknown id: err = %v, want ErrNotConnected", err)
	}

	cp := r.cp
	r.sim.Close()
	pollUntil(t, "the connection to deregister", func() bool {
		_, err := r.server.Command(testCP)
		return errors.Is(err, ocpp.ErrNotConnected)
	})

	// A handle obtained before the drop fails the same way.
	if _, err := cp.UnlockConnector(ctx3s(t), "EVSE-1"); !errors.Is(err, ocpp.ErrNotConnected) {
		t.Errorf("stale handle: err = %v, want ErrNotConnected", err)
	}
}

// TestChargePointTimeout is deliberately not parallel: goleak compares against
// the goroutines alive when it starts, and a parallel sibling would add its own.
func TestChargePointTimeout(t *testing.T) {
	// Cleanups run last-in first-out, so this runs after the rig is torn down.
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	r := newSimRig(t, 200*time.Millisecond, func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 2 * time.Second
	})

	start := time.Now()
	_, err := r.cp.UnlockConnector(context.Background(), "EVSE-1")
	if !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("call took %s, want it to give up after the 200ms timeout", elapsed)
	}
}

func TestStationCallErrorSurfacesAsRPCError(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	s := newStack(t, func(o *csms.Options) { o.NewChargePoint = newChargePointFactory(cfg) })
	s.client.OnCall(func(ocpptest.Call) (any, error) {
		return nil, errors.New("connector 1 is on fire")
	})

	cp, err := s.server.Command(testCP)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	_, err = cp.UnlockConnector(ctx3s(t), "EVSE-1")

	var rpcErr *ocpp.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v, want an *ocpp.RPCError", err)
	}
	if rpcErr.Code != ocpp.ErrGenericError || rpcErr.Description != "connector 1 is on fire" {
		t.Errorf("got %s / %q, want GenericError / %q", rpcErr.Code, rpcErr.Description, "connector 1 is on fire")
	}
}
