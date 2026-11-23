package commands_test

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	"github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// simStage is the stack with charger A being the simulator, dialled in over a
// real OCPP connection, so what the station is told and what it says back
// go through the 1.6 adapter and the handler. Charger B is not connected.
type simStage struct {
	*stage
	sim *simulator.Simulator
}

// newSimStage dials the simulator in with the given scenario, and with idTag
// as the tag a session started at the station uses.
func newSimStage(t *testing.T, scenario simulator.Scenario, idTag string) *simStage {
	t.Helper()

	// Short call timeouts, so a silent station is quick to give up on.
	cfg := testConfig()
	for i := range cfg.Chargers {
		cfg.Chargers[i].CallTimeout = config.NewDuration(400 * time.Millisecond)
	}
	var server *csms.Server
	s := newStage(t, func(o *stageOptions) {
		o.cfg = cfg
		o.commander = func(t *testing.T, cfg *config.Config, svc *core.Service, log *slog.Logger) control.Commander {
			var err error
			server, err = csms.New(csms.Options{
				Bind:     "127.0.0.1:0",
				Core:     svc,
				Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: v16.NewHandler(cfg, svc, log)},
				NewChargePoint: func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
					return v16.NewChargePoint(conn, cfg), nil
				},
				CallTimeoutFor: func(string) time.Duration { return 400 * time.Millisecond },
				IdleTimeout:    30 * time.Second,
				Log:            log,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := server.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = server.Shutdown(ctx)
			})
			return server
		}
	})

	sim, err := simulator.New(simulator.Options{
		URL: "ws://" + server.Addr(), ID: cpA, Connectors: 3, IDTag: idTag, Scenario: scenario,
		SlowDelay: 3 * time.Second, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sim.Close()
		sim.Wait()
	})
	if err := sim.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	ss := &simStage{stage: s, sim: sim}
	eventually(t, "the simulator's EVSEs to be AVAILABLE", func() bool {
		st, _ := s.core.EVSEStatus("EVSE-A1")
		return st == core.StatusAvailable
	})
	return ss
}

// The headline flow against the simulator: RESERVE_NOW is accepted, the
// connector goes Reserved with the partner's token, and redeeming the
// reservation at the station with that token (not auth.default_id_tag) opens a
// transaction and consumes the reservation.
func TestReserveRedeemWithTheOCPITokenOnTheSimulator(t *testing.T) {
	t.Parallel()
	s := newSimStage(t, simulator.ScenarioNormal, tag)

	if got := s.post("RESERVE_NOW", s.reserveBody("r1", nil)); got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("r1"); res != "ACCEPTED" {
		t.Fatalf("result = %s", res)
	}
	held, ok := s.core.ActiveReservation("EVSE-A1")
	if !ok || s.sim.ReservationID(1) != held.ID || s.sim.ConnectorStatus(1) != "Reserved" {
		t.Fatalf("the simulator holds reservation %d (%s), core %+v", s.sim.ReservationID(1), s.sim.ConnectorStatus(1), held)
	}

	if err := s.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("starting a session with the reserved token: %v", err)
	}
	eventually(t, "a transaction on EVSE-A1", func() bool { return len(s.core.ActiveTransactions("EVSE-A1")) == 1 })
	tx := s.core.ActiveTransactions("EVSE-A1")[0]
	if tx.IDTag != tag || tx.ReservationID != held.ID {
		t.Errorf("transaction = %+v, want tag %s redeeming reservation %d", tx, tag, held.ID)
	}
	if _, ok := s.core.ActiveReservation("EVSE-A1"); ok {
		t.Error("the reservation was not consumed")
	}
}

func TestReserveScenariosOnTheSimulator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		scenario simulator.Scenario
		want     string
	}{
		{simulator.ScenarioRejectReserve, "REJECTED"},
		{simulator.ScenarioOccupied, "EVSE_OCCUPIED"},
		{simulator.ScenarioSlow, "TIMEOUT"},
	}
	for _, tc := range tests {
		t.Run(string(tc.scenario), func(t *testing.T) {
			t.Parallel()
			s := newSimStage(t, tc.scenario, tag)
			got := s.post("RESERVE_NOW", s.reserveBody("r1", nil))
			if got.result != "ACCEPTED" {
				t.Fatalf("answer = %+v: the command is accepted, and fails later", got)
			}
			if res, _ := s.result("r1"); res != tc.want {
				t.Fatalf("result = %s, want %s", res, tc.want)
			}
			// A silent station is asked to cancel, and that gets no answer
			// either; the reservation goes once it has given up on it.
			eventually(t, "no reservation to be left in core", func() bool {
				cp, _ := s.core.Snapshot().ChargePoint(cpA)
				return len(cp.Reservations) == 0
			})
			if _, ok := s.core.ReservationByOCPIID("RES-1"); ok {
				t.Error("the reservation can still be found")
			}
			if id := s.sim.ReservationID(1); id != 0 && tc.scenario != simulator.ScenarioSlow {
				t.Errorf("the simulator holds reservation %d", id)
			}
		})
	}
}

func TestUnlockOnTheSimulator(t *testing.T) {
	t.Parallel()

	for scenario, want := range map[simulator.Scenario]string{
		simulator.ScenarioNormal:      "ACCEPTED",
		simulator.ScenarioUnlockFails: "FAILED",
	} {
		t.Run(string(scenario), func(t *testing.T) {
			t.Parallel()
			s := newSimStage(t, scenario, tag)
			if got := s.post("UNLOCK_CONNECTOR", s.unlockBody("u1", nil)); got.result != "ACCEPTED" {
				t.Fatalf("answer = %+v", got)
			}
			if res, _ := s.result("u1"); res != want {
				t.Errorf("result = %s, want %s", res, want)
			}
		})
	}
}

// START_SESSION with a token that is not auth.default_id_tag: the station is
// sent RemoteStartTransaction with that tag, and its StartTransaction for it
// is accepted.
func TestStartSessionWithAnOCPITokenOnTheSimulator(t *testing.T) {
	t.Parallel()
	s := newSimStage(t, simulator.ScenarioNormal, "04A1B2C3D4")

	if got := s.post("START_SESSION", s.startBody("s1", nil)); got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("s1"); res != "ACCEPTED" {
		t.Fatalf("result = %s", res)
	}
	eventually(t, "a transaction for the partner's token", func() bool {
		return len(s.core.ActiveTransactions("EVSE-A1")) == 1
	})
	if got := s.core.ActiveTransactions("EVSE-A1")[0].IDTag; got != tag {
		t.Errorf("transaction tag = %q, want %q", got, tag)
	}
	// The handler records the session in core before it returns the conf, so
	// the simulator may not have read the conf yet.
	eventually(t, "the simulator's session to be confirmed by the CSMS", func() bool {
		return s.sim.TransactionID(1) != 0
	})
}

// STOP_SESSION takes our transaction id, the one core and `cpms status` show.
func TestStopSessionOnTheSimulator(t *testing.T) {
	t.Parallel()
	s := newSimStage(t, simulator.ScenarioNormal, "04A1B2C3D4")

	if err := s.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a transaction", func() bool { return len(s.core.ActiveTransactions("EVSE-A1")) == 1 })
	id := s.core.ActiveTransactions("EVSE-A1")[0].ID

	body := map[string]any{"response_url": s.emsp.url("p1"), "session_id": id}
	if got := s.post("STOP_SESSION", body); got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("p1"); res != "ACCEPTED" {
		t.Fatalf("result = %s", res)
	}
	eventually(t, "the transaction to end", func() bool { return len(s.core.ActiveTransactions("EVSE-A1")) == 0 })

	body["session_id"] = strconv.Itoa(999)
	if got := s.post("STOP_SESSION", body); got.result != "UNKNOWN_SESSION" {
		t.Errorf("an unknown session: %+v", got)
	}
}
