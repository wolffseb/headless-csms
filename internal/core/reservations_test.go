package core_test

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/state"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

var t0 = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rig is a core.Service on a fake clock with a real state file.
type rig struct {
	clk    *clocktest.Clock
	store  *statetest.Store
	svc    *core.Service
	events <-chan core.Event
	logs   *syncBuffer
}

func newRig(t *testing.T, clk *clocktest.Clock, store *statetest.Store) *rig {
	t.Helper()

	logs := &syncBuffer{}
	svc := core.New(testConfig(),
		core.WithClock(clk.Now),
		core.WithAfterFunc(func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) }),
		core.WithStore(store),
		core.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
	)
	t.Cleanup(svc.Close)
	events, cancel := svc.Subscribe("test")
	t.Cleanup(cancel)
	return &rig{clk: clk, store: store, svc: svc, events: events, logs: logs}
}

// reserve makes an active reservation the way control does: begin, then
// activate once the station has accepted.
func (r *rig) reserve(t *testing.T, evse, tag string, length time.Duration) core.Reservation {
	t.Helper()

	pending, err := r.svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: evse, IDTag: tag,
		ExpiresAt: r.clk.Now().Add(length), Source: "cli",
	})
	if err != nil {
		t.Fatalf("BeginReservation: %v", err)
	}
	active, ok := r.svc.ActivateReservation(pending.ID)
	if !ok {
		t.Fatalf("ActivateReservation(%d) found nothing pending", pending.ID)
	}
	return active
}

func (r *rig) reservations(t *testing.T) []core.Reservation {
	t.Helper()
	snap, _ := r.svc.Snapshot().ChargePoint(testCP)
	return snap.Reservations
}

// startTx starts a transaction the way the 1.6 handler does.
func (r *rig) startTx(evse int, tag string, reservationID int) string {
	id := strconv.Itoa(r.svc.NextTransactionID())
	r.svc.StartTransaction(testCP, core.TransactionStart{
		ID: id, ConnectorID: evse, IDTag: tag, ReservationID: reservationID,
	})
	return id
}

func ended(events []core.Event) []string {
	var out []string
	for _, e := range events {
		if e.Kind == core.EventReservationEnded {
			out = append(out, e.Detail)
		}
	}
	return out
}

func TestAcceptedReservationIsPublishedAndPersisted(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	res := r.reserve(t, "EVSE-1", "04A1B2C3D4", 15*time.Minute)

	got := r.reservations(t)
	if len(got) != 1 || got[0].ID != res.ID || got[0].Status != core.ReservationActive ||
		got[0].ConnectorID != 1 || !got[0].ExpiresAt.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("snapshot reservations = %+v, want reservation %d active on connector 1", got, res.ID)
	}

	events := collect(t, r.events)
	if len(events) != 1 || events[0].Kind != core.EventReservationCreated || events[0].ReservationID != res.ID {
		t.Fatalf("events = %v, want one %s for %d", kinds(events), core.EventReservationCreated, res.ID)
	}
	if line := events[0].String(); !strings.Contains(line, "EVSE-1") || !strings.Contains(line, "2026-10-06T10:15:00Z") {
		t.Errorf("event line %q should name the EVSE and the expiry", line)
	}

	persisted := r.store.Get().Reservations
	if len(persisted) != 1 || persisted[0].ID != res.ID || persisted[0].EVSEUID != "EVSE-1" {
		t.Fatalf("state.json reservations = %+v, want reservation %d", persisted, res.ID)
	}
}

func TestPendingReservationIsNotPersisted(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	pending, err := r.svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "T", ExpiresAt: t0.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	st := r.store.Get()
	if len(st.Reservations) != 0 {
		t.Errorf("a pending reservation reached state.json: %+v", st.Reservations)
	}
	// The id is spent, though: it is persisted before anything is sent, so a
	// crash now cannot lead to the same id going to the station twice.
	if st.NextReservationID != pending.ID+1 {
		t.Errorf("next_reservation_id = %d, want %d", st.NextReservationID, pending.ID+1)
	}
}

func TestReservationExpiresWithoutAStationMessage(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	r.reserve(t, "EVSE-1", "04A1B2C3D4", 15*time.Minute)
	collect(t, r.events)

	r.clk.Advance(15*time.Minute - time.Second)
	if len(r.reservations(t)) != 1 {
		t.Fatal("reservation ended before its expiry")
	}

	r.clk.Advance(time.Second)
	if got := r.reservations(t); len(got) != 0 {
		t.Fatalf("reservation still held after expiry: %+v", got)
	}
	if got := ended(collect(t, r.events)); len(got) != 1 || got[0] != core.ReservationExpired {
		t.Errorf("ended events = %v, want [%s]", got, core.ReservationExpired)
	}
	if got := r.store.Get().Reservations; len(got) != 0 {
		t.Errorf("state.json still holds %+v", got)
	}
}

func TestStartingASessionConsumesTheReservation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		tag           string
		reservationID func(core.Reservation) int
		consumed      bool
	}{
		{"with reservationId", "SOMEONE-ELSE", func(r core.Reservation) int { return r.ID }, true},
		// Some stations leave reservationId out; the same tag on the same
		// connector is enough, compared case-insensitively like every tag.
		{"without reservationId, same tag", "04a1b2c3d4", func(core.Reservation) int { return 0 }, true},
		{"without reservationId, other tag", "SOMEONE-ELSE", func(core.Reservation) int { return 0 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, clocktest.New(t0), statetest.New(t))
			res := r.reserve(t, "EVSE-1", "04A1B2C3D4", 15*time.Minute)
			collect(t, r.events)

			txID := r.startTx(1, tc.tag, tc.reservationID(res))

			snap, _ := r.svc.Snapshot().ChargePoint(testCP)
			events := ended(collect(t, r.events))
			if !tc.consumed {
				if len(snap.Reservations) != 1 || len(events) != 0 {
					t.Fatalf("reservation should survive another tag's session; reservations %+v, ended %v",
						snap.Reservations, events)
				}
				return
			}
			if len(snap.Reservations) != 0 {
				t.Fatalf("reservation still held: %+v", snap.Reservations)
			}
			if len(events) != 1 || events[0] != core.ReservationConsumed {
				t.Errorf("ended events = %v, want [%s]", events, core.ReservationConsumed)
			}
			if tx := snap.Transactions[0]; tx.ID != txID || tx.ReservationID != res.ID {
				t.Errorf("transaction = %+v, want %s redeeming reservation %d", tx, txID, res.ID)
			}
			if got := r.store.Get().Reservations; len(got) != 0 {
				t.Errorf("state.json still holds %+v", got)
			}

			// The expiry timer must not fire for a reservation that is gone.
			r.clk.Advance(time.Hour)
			if got := ended(collect(t, r.events)); len(got) != 0 {
				t.Errorf("a consumed reservation ended again: %v", got)
			}
		})
	}
}

func TestOneReservationPerEVSE(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	pending, err := r.svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "A", ExpiresAt: t0.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Refused while the first is still pending, and again once active.
	for _, phase := range []string{"pending", "active"} {
		_, err := r.svc.BeginReservation(core.Reservation{
			ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "B", ExpiresAt: t0.Add(time.Hour),
		})
		if !errors.Is(err, core.ErrAlreadyReserved) {
			t.Errorf("%s: second reservation err = %v, want ErrAlreadyReserved", phase, err)
		}
		r.svc.ActivateReservation(pending.ID)
	}
	// Another EVSE is unaffected.
	r.reserve(t, "EVSE-2", "B", time.Hour)
}

func TestReservationIsNotMadeWhenItCannotBeRecorded(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	r.store.Fail(true)

	_, err := r.svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "A", ExpiresAt: t0.Add(time.Hour),
	})
	if !errors.Is(err, statetest.ErrInjected) {
		t.Fatalf("err = %v, want the write failure", err)
	}
	if got := r.reservations(t); len(got) != 0 {
		t.Fatalf("an unrecorded reservation is held: %+v", got)
	}

	// The EVSE is free again once the disk is.
	r.store.Fail(false)
	r.reserve(t, "EVSE-1", "A", time.Hour)
}

func TestStationChangesSurviveAFailingStore(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	r.store.Fail(true)

	id := r.startTx(1, "04A1B2C3D4", 0)
	if _, ok := r.svc.StopTransaction(testCP, id, 100, "Local", time.Time{}); !ok {
		t.Fatal("stop failed after a failed write")
	}
	snap, _ := r.svc.Snapshot().ChargePoint(testCP)
	if len(snap.Transactions) != 1 {
		t.Fatalf("transactions = %+v, want the one the station reported", snap.Transactions)
	}
	if !strings.Contains(r.logs.String(), "could not persist") {
		t.Errorf("the write failure was not logged; logs:\n%s", r.logs.String())
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	clk := clocktest.New(t0)

	first := newRig(t, clk, statetest.Open(t, path))
	res := first.reserve(t, "EVSE-1", "04A1B2C3D4", 15*time.Minute)
	done := first.startTx(2, "04A1B2C3D4", 0)
	if _, ok := first.svc.StopTransaction(testCP, done, 10, "Local", time.Time{}); !ok {
		t.Fatal("stopping the first transaction failed")
	}
	running := first.startTx(2, "04A1B2C3D4", 0)
	first.svc.Close()

	// A new process: a fresh Service on the same file.
	second := newRig(t, clk, statetest.Open(t, path))

	got := second.reservations(t)
	if len(got) != 1 || got[0].ID != res.ID || !got[0].ExpiresAt.Equal(res.ExpiresAt) ||
		got[0].Status != core.ReservationActive || got[0].ConnectorID != 1 {
		t.Fatalf("reservations after restart = %+v, want %d expiring %s", got, res.ID, res.ExpiresAt)
	}
	active := second.svc.ActiveTransactions("EVSE-2")
	if len(active) != 1 || active[0].ID != running {
		t.Fatalf("active transactions after restart = %+v, want %s", active, running)
	}

	// Counters continue rather than restart.
	doneID, _ := strconv.Atoi(done)
	if next := second.svc.NextTransactionID(); next != doneID+2 {
		t.Errorf("next transaction id = %d, want %d", next, doneID+2)
	}
	if next := second.reserve(t, "EVSE-2", "X", time.Hour); next.ID != res.ID+1 {
		t.Errorf("next reservation id = %d, want %d", next.ID, res.ID+1)
	}

	// The station can close a session that began before the restart.
	if _, ok := second.svc.StopTransaction(testCP, running, 20, "Local", time.Time{}); !ok {
		t.Errorf("transaction %s from before the restart could not be stopped", running)
	}

	// And the reservation's timer was re-armed.
	collect(t, second.events)
	clk.Advance(15 * time.Minute)
	if got := ended(collect(t, second.events)); len(got) != 1 || got[0] != core.ReservationExpired {
		t.Errorf("ended events = %v, want the restored reservation to expire", got)
	}
}

func TestReservationThatExpiredWhileDownIsDropped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	clk := clocktest.New(t0)

	first := newRig(t, clk, statetest.Open(t, path))
	first.reserve(t, "EVSE-1", "04A1B2C3D4", 15*time.Minute)
	first.svc.Close()

	clk.Advance(time.Hour)
	second := newRig(t, clk, statetest.Open(t, path))

	if got := second.reservations(t); len(got) != 0 {
		t.Fatalf("an expired reservation came back: %+v", got)
	}
	if got := second.store.Get().Reservations; len(got) != 0 {
		t.Errorf("state.json still holds the expired reservation: %+v", got)
	}
}

// The counters must survive on their own, not only because an id is still in
// use: once every session and reservation has ended, nothing else in the file
// says which ids were spent.
func TestIDsAreNeverReusedAfterARestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	clk := clocktest.New(t0)

	first := newRig(t, clk, statetest.Open(t, path))
	res := first.reserve(t, "EVSE-1", "04A1B2C3D4", time.Hour)
	first.svc.EndReservation(res.ID, core.ReservationCancelled)
	tx := first.startTx(1, "04A1B2C3D4", 0)
	first.svc.StopTransaction(testCP, tx, 10, "Local", time.Time{})
	remote, err := first.svc.NextRemoteStartID()
	if err != nil {
		t.Fatal(err)
	}
	first.svc.Close()

	second := newRig(t, clk, statetest.Open(t, path))
	txID, _ := strconv.Atoi(tx)
	if next := second.svc.NextTransactionID(); next != txID+1 {
		t.Errorf("next transaction id = %d, want %d", next, txID+1)
	}
	if next := second.reserve(t, "EVSE-1", "X", time.Hour); next.ID != res.ID+1 {
		t.Errorf("next reservation id = %d, want %d", next.ID, res.ID+1)
	}
	if next, _ := second.svc.NextRemoteStartID(); next != remote+1 {
		t.Errorf("next remote start id = %d, want %d", next, remote+1)
	}
}

// A file edited by hand, or written before a counter existed, can hold ids
// the counters have not caught up with. They must not be handed out again.
func TestCountersNeverFallBehindTheIDsInTheFile(t *testing.T) {
	t.Parallel()
	store := statetest.New(t)
	if err := store.Update(func(st *state.State) error {
		st.Reservations = []state.Reservation{{
			ID: 7, ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "A", ExpiresAt: t0.Add(time.Hour),
		}}
		st.Transactions = []state.Transaction{{ID: "41", ChargePointID: testCP, EVSEUID: "EVSE-2", ConnectorID: 2}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	r := newRig(t, clocktest.New(t0), store)
	if next := r.svc.NextTransactionID(); next != 42 {
		t.Errorf("next transaction id = %d, want 42", next)
	}
	if next := r.reserve(t, "EVSE-2", "B", time.Hour); next.ID != 8 {
		t.Errorf("next reservation id = %d, want 8", next.ID)
	}
}
