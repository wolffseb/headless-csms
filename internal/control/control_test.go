package control_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

const (
	testCP  = "ALP-HYC-001"
	testTag = "04A1B2C3D4"
)

var t0 = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

func testConfig() *config.Config {
	return &config.Config{
		Charger: config.Charger{ID: testCP},
		Auth:    config.Auth{DefaultIDTag: testTag},
		Location: config.Location{
			EVSEs: []config.EVSE{
				{UID: "EVSE-1", OCPPConnectorID: 1},
				{UID: "EVSE-2", OCPPConnectorID: 2},
			},
		},
	}
}

// fakeStation is an ocpp.ChargePoint that records what it was asked and
// answers what the test told it to.
type fakeStation struct {
	mu      sync.Mutex
	calls   []string
	reserve []ocpp.ReserveRequest
	starts  []ocpp.RemoteStartRequest
	stops   []string
	answer  map[string]ocpp.Result
	fail    map[string]error
}

func newFakeStation() *fakeStation {
	return &fakeStation{answer: map[string]ocpp.Result{}, fail: map[string]error{}}
}

func (f *fakeStation) respond(op string) (ocpp.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, op)
	if err := f.fail[op]; err != nil {
		return ocpp.Result{}, err
	}
	if res, ok := f.answer[op]; ok {
		return res, nil
	}
	if op == "unlock" {
		return ocpp.Result{Status: ocpp.CommandUnlocked, Raw: "Unlocked"}, nil
	}
	return ocpp.Result{Status: ocpp.CommandAccepted, Raw: "Accepted"}, nil
}

func (f *fakeStation) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeStation) ID() string            { return testCP }
func (f *fakeStation) Version() ocpp.Version { return ocpp.Version16 }

func (f *fakeStation) ReserveNow(_ context.Context, r ocpp.ReserveRequest) (ocpp.Result, error) {
	f.mu.Lock()
	f.reserve = append(f.reserve, r)
	f.mu.Unlock()
	return f.respond("reserve")
}

func (f *fakeStation) CancelReservation(context.Context, int) (ocpp.Result, error) {
	return f.respond("cancel")
}

func (f *fakeStation) RemoteStart(_ context.Context, r ocpp.RemoteStartRequest) (ocpp.Result, error) {
	f.mu.Lock()
	f.starts = append(f.starts, r)
	f.mu.Unlock()
	return f.respond("start")
}

func (f *fakeStation) RemoteStop(_ context.Context, id string) (ocpp.Result, error) {
	f.mu.Lock()
	f.stops = append(f.stops, id)
	f.mu.Unlock()
	return f.respond("stop")
}

func (f *fakeStation) UnlockConnector(context.Context, string) (ocpp.Result, error) {
	return f.respond("unlock")
}

func (f *fakeStation) TriggerStatus(context.Context, string) (ocpp.Result, error) {
	return f.respond("trigger")
}

func (f *fakeStation) Capabilities(context.Context) (ocpp.Capabilities, error) {
	return ocpp.Capabilities{Reservation: true}, nil
}

// commands is a control.Commander over the fake, or over nothing at all.
type commands struct{ station *fakeStation }

func (c commands) Command(string) (ocpp.ChargePoint, error) {
	if c.station == nil {
		return nil, ocpp.ErrNotConnected
	}
	return c.station, nil
}

type rig struct {
	clk     *clocktest.Clock
	store   *statetest.Store
	core    *core.Service
	station *fakeStation
	ctl     *control.Service
	events  <-chan core.Event
}

func newRig(t *testing.T) *rig {
	t.Helper()

	clk := clocktest.New(t0)
	store := statetest.New(t)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := testConfig()
	svc := core.New(cfg,
		core.WithClock(clk.Now),
		core.WithAfterFunc(func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) }),
		core.WithStore(store),
		core.WithLogger(discard),
	)
	t.Cleanup(svc.Close)
	events, cancel := svc.Subscribe("test")
	t.Cleanup(cancel)

	station := newFakeStation()
	ctl := control.New(control.Options{
		Core: svc, Commands: commands{station}, Config: cfg, Now: clk.Now, Log: discard,
	})
	return &rig{clk: clk, store: store, core: svc, station: station, ctl: ctl, events: events}
}

func (r *rig) reservations() []core.Reservation {
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	return snap.Reservations
}

// drain returns the events published so far. Core publishes synchronously
// from the calling goroutine, so after a control call returns they are all
// already buffered.
func drain(ch <-chan core.Event) []core.Event {
	var out []core.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func endedAs(events []core.Event) []string {
	var out []string
	for _, e := range events {
		if e.Kind == core.EventReservationEnded {
			out = append(out, e.Detail)
		}
	}
	return out
}

func TestReserveAccepted(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	res, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1", Source: "cli"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Defaults: the configured tag, fifteen minutes.
	sent := r.station.reserve[0]
	if sent.ReservationID != res.ID || sent.IDTag != testTag || !sent.ExpiresAt.Equal(t0.Add(15*time.Minute)) {
		t.Errorf("ReserveNow = %+v, want id %d, tag %s, expiry +15m", sent, res.ID, testTag)
	}
	if got := r.reservations(); len(got) != 1 || got[0].Status != core.ReservationActive {
		t.Fatalf("reservations = %+v, want one active", got)
	}
	if events := drain(r.events); len(events) != 1 || events[0].Kind != core.EventReservationCreated {
		t.Errorf("events = %+v, want one reservation_created", events)
	}
	if got := r.store.Get().Reservations; len(got) != 1 || got[0].ID != res.ID {
		t.Errorf("state.json reservations = %+v, want %d", got, res.ID)
	}
}

func TestReserveRefusedByTheStation(t *testing.T) {
	t.Parallel()

	for _, status := range []ocpp.CommandStatus{
		ocpp.CommandRejected, ocpp.CommandOccupied, ocpp.CommandFaulted, ocpp.CommandUnavailable,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.station.answer["reserve"] = ocpp.Result{Status: status, Raw: string(status)}

			_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})

			var refused *control.RefusedError
			if !errors.As(err, &refused) || refused.Status != status || refused.Op != control.OpReserve {
				t.Fatalf("err = %v, want a RefusedError with status %s", err, status)
			}
			if got := r.reservations(); len(got) != 0 {
				t.Errorf("core still holds %+v", got)
			}
			if got := r.store.Get().Reservations; len(got) != 0 {
				t.Errorf("state.json still holds %+v", got)
			}
			if got := endedAs(drain(r.events)); len(got) != 1 || got[0] != core.ReservationRejected {
				t.Errorf("ended = %v, want [rejected]", got)
			}
		})
	}
}

func TestReserveTimeoutLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["reserve"] = ocpp.ErrTimeout

	_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})
	if !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if got := r.reservations(); len(got) != 0 {
		t.Errorf("a timed-out reservation is dangling: %+v", got)
	}
	if got := endedAs(drain(r.events)); len(got) != 1 || got[0] != core.ReservationTimeout {
		t.Errorf("ended = %v, want [timeout]", got)
	}

	// And the EVSE is free for the next attempt.
	delete(r.station.fail, "reserve")
	if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); err != nil {
		t.Errorf("retry after a timeout: %v", err)
	}
}

func TestSecondReservationIsRefusedLocally(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); err != nil {
		t.Fatal(err)
	}

	_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1", IDTag: "OTHER"})
	if !errors.Is(err, control.ErrAlreadyReserved) {
		t.Fatalf("err = %v, want ErrAlreadyReserved", err)
	}
	if n := r.station.callCount(); n != 1 {
		t.Errorf("station saw %d calls, want only the first ReserveNow", n)
	}
}

func TestReserveSendsNothingIfItCannotBeRecorded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Fail(true)

	_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})
	if !errors.Is(err, statetest.ErrInjected) {
		t.Fatalf("err = %v, want the write failure", err)
	}
	if n := r.station.callCount(); n != 0 {
		t.Errorf("station saw %d calls, want none", n)
	}
	if got := r.reservations(); len(got) != 0 {
		t.Errorf("core holds %+v", got)
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); err != nil {
			t.Fatal(err)
		}
		drain(r.events)

		if err := r.ctl.Cancel(context.Background(), "EVSE-1"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got := r.reservations(); len(got) != 0 {
			t.Errorf("reservation survived: %+v", got)
		}
		if got := endedAs(drain(r.events)); len(got) != 1 || got[0] != core.ReservationCancelled {
			t.Errorf("ended = %v, want [cancelled]", got)
		}
		if got := r.store.Get().Reservations; len(got) != 0 {
			t.Errorf("state.json still holds %+v", got)
		}
	})

	t.Run("nothing to cancel", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if err := r.ctl.Cancel(context.Background(), "EVSE-1"); !errors.Is(err, control.ErrNoReservation) {
			t.Fatalf("err = %v, want ErrNoReservation", err)
		}
		if n := r.station.callCount(); n != 0 {
			t.Errorf("station saw %d calls, want none", n)
		}
	})

	t.Run("station rejects", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); err != nil {
			t.Fatal(err)
		}
		r.station.answer["cancel"] = ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}

		var refused *control.RefusedError
		if err := r.ctl.Cancel(context.Background(), "EVSE-1"); !errors.As(err, &refused) {
			t.Fatalf("err = %v, want a RefusedError", err)
		}
		// The station does not hold it, so our record is stale and goes too.
		if got := r.reservations(); len(got) != 0 {
			t.Errorf("stale reservation kept: %+v", got)
		}
	})

	t.Run("timeout keeps the reservation", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); err != nil {
			t.Fatal(err)
		}
		r.station.fail["cancel"] = ocpp.ErrTimeout

		if err := r.ctl.Cancel(context.Background(), "EVSE-1"); !errors.Is(err, ocpp.ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
		// No answer means the station may still hold it.
		if got := r.reservations(); len(got) != 1 {
			t.Errorf("reservations = %+v, want it kept", got)
		}
	})
}

func TestStart(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	if err := r.ctl.Start(context.Background(), "EVSE-2", ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.ctl.Start(context.Background(), "EVSE-1", "CARD-2"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first, second := r.station.starts[0], r.station.starts[1]
	if first.EVSEUID != "EVSE-2" || first.IDTag != testTag {
		t.Errorf("first RemoteStart = %+v, want EVSE-2 with the default tag", first)
	}
	if second.IDTag != "CARD-2" {
		t.Errorf("second RemoteStart tag = %q, want CARD-2", second.IDTag)
	}
	// Remote start ids are allocated from the persisted counter, for 1.6
	// too, so they never repeat.
	if first.RemoteStartID < 1 || second.RemoteStartID != first.RemoteStartID+1 {
		t.Errorf("remote start ids = %d, %d, want consecutive positive ids", first.RemoteStartID, second.RemoteStartID)
	}
	if got := r.store.Get().NextRemoteStartID; got != second.RemoteStartID+1 {
		t.Errorf("next_remote_start_id = %d, want %d", got, second.RemoteStartID+1)
	}

	r.station.answer["start"] = ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}
	var refused *control.RefusedError
	if err := r.ctl.Start(context.Background(), "EVSE-1", ""); !errors.As(err, &refused) || refused.Op != control.OpStart {
		t.Errorf("err = %v, want a RefusedError for start", err)
	}
}

func TestStop(t *testing.T) {
	t.Parallel()

	t.Run("no active transaction", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		if err := r.ctl.Stop(context.Background(), "EVSE-1"); !errors.Is(err, control.ErrNoActiveTransaction) {
			t.Fatalf("err = %v, want ErrNoActiveTransaction", err)
		}
		if n := r.station.callCount(); n != 0 {
			t.Errorf("station saw %d calls, want none", n)
		}
	})

	t.Run("stops the running one", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.core.StartTransaction(testCP, core.TransactionStart{ID: "41", ConnectorID: 1, IDTag: testTag})

		if err := r.ctl.Stop(context.Background(), "EVSE-1"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if len(r.station.stops) != 1 || r.station.stops[0] != "41" {
			t.Errorf("RemoteStop calls = %v, want [41]", r.station.stops)
		}
	})

	t.Run("refuses to guess between two", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.core.StartTransaction(testCP, core.TransactionStart{ID: "41", ConnectorID: 1, IDTag: testTag})
		r.core.StartTransaction(testCP, core.TransactionStart{ID: "42", ConnectorID: 1, IDTag: testTag})

		if err := r.ctl.Stop(context.Background(), "EVSE-1"); err == nil {
			t.Fatal("Stop picked one of two active transactions")
		}
		if n := r.station.callCount(); n != 0 {
			t.Errorf("station saw %d calls, want none", n)
		}
	})
}

func TestUnlockAndTrigger(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	if err := r.ctl.Unlock(context.Background(), "EVSE-1"); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	r.station.answer["unlock"] = ocpp.Result{Status: ocpp.CommandUnlockFailed, Raw: "UnlockFailed"}
	var refused *control.RefusedError
	if err := r.ctl.Unlock(context.Background(), "EVSE-1"); !errors.As(err, &refused) ||
		refused.Status != ocpp.CommandUnlockFailed {
		t.Errorf("err = %v, want a RefusedError with UnlockFailed", err)
	}

	if err := r.ctl.TriggerStatus(context.Background(), "EVSE-1"); err != nil {
		t.Errorf("TriggerStatus: %v", err)
	}
}

func TestLocalRefusalsSendNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	calls := map[string]func(*control.Service) error{
		"reserve": func(c *control.Service) error {
			_, err := c.Reserve(ctx, control.ReserveParams{EVSEUID: "NOPE"})
			return err
		},
		"cancel":  func(c *control.Service) error { return c.Cancel(ctx, "NOPE") },
		"start":   func(c *control.Service) error { return c.Start(ctx, "NOPE", "") },
		"stop":    func(c *control.Service) error { return c.Stop(ctx, "NOPE") },
		"unlock":  func(c *control.Service) error { return c.Unlock(ctx, "NOPE") },
		"trigger": func(c *control.Service) error { return c.TriggerStatus(ctx, "NOPE") },
	}
	for name, call := range calls {
		t.Run(name+" unknown EVSE", func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if err := call(r.ctl); !errors.Is(err, ocpp.ErrUnknownEVSE) {
				t.Fatalf("err = %v, want ErrUnknownEVSE", err)
			}
			if n := r.station.callCount(); n != 0 {
				t.Errorf("station saw %d calls, want none", n)
			}
		})
	}

	t.Run("not connected", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		ctl := control.New(control.Options{Core: r.core, Commands: commands{}, Config: testConfig()})
		_, err := ctl.Reserve(ctx, control.ReserveParams{EVSEUID: "EVSE-1"})
		if !errors.Is(err, ocpp.ErrNotConnected) {
			t.Fatalf("err = %v, want ErrNotConnected", err)
		}
		if got := r.reservations(); len(got) != 0 {
			t.Errorf("core holds %+v", got)
		}
	})
}
