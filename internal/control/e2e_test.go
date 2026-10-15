package control_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// simRig is the whole stack `cpms run` builds, with the simulator dialled in
// as the station: core on a real state file, the CSMS, the 1.6 handler and
// command adapter, and control on top.
type simRig struct {
	core  *core.Service
	ctl   *control.Service
	sim   *simulator.Simulator
	store *statetest.Store
}

func newSimRig(t *testing.T, callTimeout time.Duration, mutate ...func(*simulator.Options)) *simRig {
	t.Helper()

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	store := statetest.New(t)
	svc := core.New(cfg, core.WithStore(store), core.WithLogger(discard))
	t.Cleanup(svc.Close)

	server, err := csms.New(csms.Options{
		Bind:     "127.0.0.1:0",
		Core:     svc,
		Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: v16.NewHandler(cfg, svc, discard)},
		NewChargePoint: func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
			return v16.NewChargePoint(conn, cfg), nil
		},
		CallTimeout: callTimeout,
		IdleTimeout: 30 * time.Second,
		Log:         discard,
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
		URL: "ws://" + server.Addr(), ID: testCP, Connectors: 2, IDTag: testTag, Log: discard,
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

	r := &simRig{
		core:  svc,
		ctl:   control.New(control.Options{Core: svc, Commands: server, Config: cfg, Log: discard}),
		sim:   sim,
		store: store,
	}
	r.waitFor(t, "both EVSEs available", func() bool {
		return r.status("EVSE-1") == core.StatusAvailable && r.status("EVSE-2") == core.StatusAvailable
	})
	return r
}

func (r *simRig) status(uid string) core.EVSEStatus {
	st, _ := r.core.EVSEStatus(uid)
	return st
}

func (r *simRig) waitFor(t *testing.T, what string, cond func() bool) {
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

// TestReserveStartStopAgainstTheSimulator is the flow this tool exists for:
// reserve, redeem the reservation with the office tag, stop.
func TestReserveStartStopAgainstTheSimulator(t *testing.T) {
	t.Parallel()
	r := newSimRig(t, 5*time.Second)
	ctx := context.Background()

	res, err := r.ctl.Reserve(ctx, control.ReserveParams{EVSEUID: "EVSE-1", Source: "cli"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// RESERVED is what the station reported, not something core inferred.
	r.waitFor(t, "EVSE-1 to be reported RESERVED", func() bool { return r.status("EVSE-1") == core.StatusReserved })
	if got := r.sim.ReservationID(1); got != res.ID {
		t.Fatalf("simulator holds reservation %d, want %d", got, res.ID)
	}

	if err := r.ctl.Start(ctx, "EVSE-1", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.waitFor(t, "a transaction on EVSE-1", func() bool { return len(r.core.ActiveTransactions("EVSE-1")) == 1 })
	r.waitFor(t, "EVSE-1 to be CHARGING", func() bool { return r.status("EVSE-1") == core.StatusCharging })
	if _, held := r.core.ActiveReservation("EVSE-1"); held {
		t.Error("the reservation survived the session it was redeemed by")
	}
	tx := r.core.ActiveTransactions("EVSE-1")[0]
	if tx.IDTag != testTag || tx.ReservationID != res.ID {
		t.Errorf("transaction = %+v, want tag %s redeeming reservation %d", tx, testTag, res.ID)
	}
	if got := r.store.Get().Transactions; len(got) != 1 || got[0].ID != tx.ID {
		t.Errorf("state.json transactions = %+v, want %s", got, tx.ID)
	}

	if err := r.ctl.Stop(ctx, "EVSE-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	r.waitFor(t, "the transaction to end", func() bool { return len(r.core.ActiveTransactions("EVSE-1")) == 0 })
	if got := r.store.Get().Transactions; len(got) != 0 {
		t.Errorf("state.json still holds %+v", got)
	}
}

func TestReserveRefusedByTheSimulator(t *testing.T) {
	t.Parallel()

	for scenario, want := range map[simulator.Scenario]ocpp.CommandStatus{
		simulator.ScenarioRejectReserve: ocpp.CommandRejected,
		simulator.ScenarioOccupied:      ocpp.CommandOccupied,
	} {
		t.Run(string(scenario), func(t *testing.T) {
			t.Parallel()
			r := newSimRig(t, 5*time.Second, func(o *simulator.Options) { o.Scenario = scenario })

			_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})
			var refused *control.RefusedError
			if !errors.As(err, &refused) || refused.Status != want {
				t.Fatalf("err = %v, want a RefusedError with %s", err, want)
			}
			if _, held := r.core.ActiveReservation("EVSE-1"); held {
				t.Error("a refused reservation is held")
			}
		})
	}
}

// TestReserveTimeoutAgainstTheSimulator is not parallel: goleak compares
// against the goroutines alive when it starts, and a parallel sibling would
// add its own.
func TestReserveTimeoutAgainstTheSimulator(t *testing.T) {
	// Cleanups run last-in first-out, so this runs after the rig is torn down.
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	r := newSimRig(t, 200*time.Millisecond, func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 2 * time.Second
	})

	_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})
	if !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	if len(snap.Reservations) != 0 {
		t.Errorf("a timed-out reservation is dangling: %+v", snap.Reservations)
	}
}
