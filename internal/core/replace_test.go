package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// ocpiReserve makes an active reservation that came in over OCPI.
func (r *rig) ocpiReserve(t *testing.T, ocpiID, evse, tag, url string, length time.Duration) core.Reservation {
	t.Helper()

	pending, err := r.svc.BeginReservation(core.Reservation{
		OCPIReservationID: ocpiID, ChargePointID: testCP, EVSEUID: evse, IDTag: tag,
		ExpiresAt: r.clk.Now().Add(length), Source: "ocpi", ResponseURL: url,
	})
	if err != nil {
		t.Fatalf("BeginReservation: %v", err)
	}
	active, ok := r.svc.ActivateReservation(pending.ID)
	if !ok {
		t.Fatal("ActivateReservation found nothing pending")
	}
	return active
}

func replacement(ocpiID, evse, tag, url string, expires time.Time) core.Reservation {
	return core.Reservation{
		OCPIReservationID: ocpiID, ChargePointID: testCP, EVSEUID: evse, IDTag: tag,
		ExpiresAt: expires, Source: "ocpi", ResponseURL: url,
	}
}

// A reservation the partner already holds is replaced under its OCPP id: it
// goes back to pending with the new request's fields, the old expiry no
// longer applies, and activating it again arms the new one.
func TestBeginReplacementKeepsTheIDAndTakesTheNewRequest(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	orig := r.ocpiReserve(t, "RES-1", "EVSE-1", "TAG-A", "http://emsp/a", 10*time.Minute)
	collect(t, r.events)

	got, replaced, err := r.svc.BeginReplacement(replacement("res-1", "EVSE-1", "TAG-B", "http://emsp/b", t0.Add(30*time.Minute)))
	if err != nil || !replaced {
		t.Fatalf("BeginReplacement = %v, %v", replaced, err)
	}
	if got.ID != orig.ID || got.Status != core.ReservationPending || got.IDTag != "TAG-B" ||
		got.ResponseURL != "http://emsp/b" || !got.ExpiresAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("replacement = %+v, want id %d, pending, for TAG-B", got, orig.ID)
	}
	if _, ok := r.svc.ActiveReservation("EVSE-1"); ok {
		t.Error("a reservation that is being replaced still reads as active")
	}
	// The station still holds the original until it answers, and a crash now
	// must not forget it.
	if got := r.store.Get().Reservations; len(got) != 1 || got[0].IDTag != "TAG-A" || got[0].ResponseURL != "http://emsp/a" {
		t.Errorf("state.json lists %+v while the replacement is pending, want the original", got)
	}

	// The old expiry timer is gone, and must not end the pending replacement.
	if n := r.clk.Pending(); n != 0 {
		t.Errorf("%d timers pending while the replacement is, want none", n)
	}
	r.clk.Advance(11 * time.Minute)
	if rs := r.reservations(t); len(rs) != 1 {
		t.Fatalf("the old expiry ended the replacement: %+v", rs)
	}

	active, ok := r.svc.ActivateReservation(got.ID)
	if !ok || active.Status != core.ReservationActive {
		t.Fatalf("ActivateReservation = %+v, %v", active, ok)
	}
	if rs := r.store.Get().Reservations; len(rs) != 1 || !rs[0].ExpiresAt.Equal(t0.Add(30*time.Minute)) || rs[0].ResponseURL != "http://emsp/b" {
		t.Errorf("state.json = %+v, want the replacement", rs)
	}
	r.clk.Advance(20 * time.Minute) // 31 minutes in
	if _, ok := r.svc.ActiveReservation("EVSE-1"); ok {
		t.Error("the replacement did not expire at its own time")
	}
}

func TestBeginReplacementOfAnUnknownIDIsNotAReplacement(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	r.ocpiReserve(t, "RES-1", "EVSE-1", "TAG-A", "", time.Hour)

	if _, replaced, err := r.svc.BeginReplacement(replacement("RES-9", "EVSE-1", "TAG", "", t0.Add(time.Hour))); replaced || err != nil {
		t.Errorf("BeginReplacement of an unknown id = %v, %v, want false and no error", replaced, err)
	}
	if _, replaced, err := r.svc.BeginReplacement(replacement("", "EVSE-1", "TAG", "", t0.Add(time.Hour))); replaced || err != nil {
		t.Errorf("BeginReplacement of no id = %v, %v, want false and no error", replaced, err)
	}
}

// Moving a reservation to another EVSE, or replacing one that is not yet
// active, is refused.
func TestBeginReplacementRefusals(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	r.ocpiReserve(t, "RES-1", "EVSE-1", "TAG-A", "", time.Hour)
	pending, err := r.svc.BeginReservation(core.Reservation{
		OCPIReservationID: "RES-2", ChargePointID: testCP, EVSEUID: "EVSE-2", IDTag: "TAG", ExpiresAt: t0.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]core.Reservation{
		"another EVSE":           replacement("RES-1", "EVSE-2", "TAG", "", t0.Add(time.Hour)),
		"a reservation in setup": replacement("RES-2", "EVSE-2", "TAG", "", t0.Add(time.Hour)),
	} {
		_, replaced, err := r.svc.BeginReplacement(want)
		if replaced || !errors.Is(err, core.ErrAlreadyReserved) {
			t.Errorf("%s: BeginReplacement = %v, %v, want ErrAlreadyReserved", name, replaced, err)
		}
	}
	// Neither touched what was there.
	if rs := r.reservations(t); len(rs) != 2 || rs[0].Status != core.ReservationActive || rs[0].IDTag != "TAG-A" ||
		rs[1].ID != pending.ID || rs[1].Status != core.ReservationPending {
		t.Errorf("reservations = %+v", rs)
	}
}

// A replacement that cannot be recorded leaves the reservation as it was,
// including its expiry timer.
func TestBeginReplacementThatCannotBeRecordedChangesNothing(t *testing.T) {
	t.Parallel()
	store := statetest.New(t)
	r := newRig(t, clocktest.New(t0), store)
	orig := r.ocpiReserve(t, "RES-1", "EVSE-1", "TAG-A", "", 10*time.Minute)

	store.Fail(true)
	if _, replaced, err := r.svc.BeginReplacement(replacement("RES-1", "EVSE-1", "TAG-B", "", t0.Add(time.Hour))); replaced || err == nil {
		t.Fatalf("BeginReplacement = %v, %v, want an error", replaced, err)
	}
	store.Fail(false)

	got, ok := r.svc.ActiveReservation("EVSE-1")
	if !ok || got.IDTag != "TAG-A" || !got.ExpiresAt.Equal(orig.ExpiresAt) {
		t.Fatalf("reservation = %+v, %v, want the original", got, ok)
	}
	r.clk.Advance(11 * time.Minute)
	if _, ok := r.svc.ActiveReservation("EVSE-1"); ok {
		t.Error("the original's expiry timer was lost")
	}
}

// The end of a reservation carries its response_url only if it was active: the
// partner heard of it. A replacement that fails ends while pending.
func TestEndedEventsCarryTheResponseURLOfActiveReservations(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	orig := r.ocpiReserve(t, "RES-1", "EVSE-1", "TAG-A", "http://emsp/a", time.Hour)
	collect(t, r.events)
	r.svc.EndReservation(orig.ID, core.ReservationFailed)
	evs := collect(t, r.events)
	if len(evs) != 1 || evs[0].Kind != core.EventReservationEnded || evs[0].ResponseURL != "http://emsp/a" {
		t.Fatalf("events = %+v, want one ended with http://emsp/a", evs)
	}

	r.ocpiReserve(t, "RES-2", "EVSE-1", "TAG-A", "http://emsp/b", time.Hour)
	got, _, err := r.svc.BeginReplacement(replacement("RES-2", "EVSE-1", "TAG-A", "http://emsp/c", t0.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, r.events)
	r.svc.EndReservation(got.ID, core.ReservationTimeout)
	evs = collect(t, r.events)
	if len(evs) != 1 || evs[0].ResponseURL != "" {
		t.Fatalf("events = %+v, want one ended without a response URL: it never became active", evs)
	}

	// ReservationByOCPIID finds in any status, whatever the case.
	r.ocpiReserve(t, "Res-3", "EVSE-2", "TAG-A", "", time.Hour)
	if found, ok := r.svc.ReservationByOCPIID("RES-3"); !ok || found.EVSEUID != "EVSE-2" {
		t.Errorf("ReservationByOCPIID = %+v, %v", found, ok)
	}
	if _, ok := r.svc.ReservationByOCPIID(""); ok {
		t.Error("an empty id matched")
	}
}

// RevertReplacement puts the original back as it was: active, with its tag,
// expiry and response URL, its expiry re-armed, and nothing published.
func TestRevertReplacementRestoresTheOriginal(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))
	orig := r.ocpiReserve(t, "RES-1", "EVSE-1", "TAG-A", "http://emsp/a", 10*time.Minute)
	pending, _, err := r.svc.BeginReplacement(replacement("RES-1", "EVSE-1", "TAG-B", "http://emsp/b", t0.Add(30*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if prior, ok := r.svc.PriorReservation(pending.ID); !ok || prior.IDTag != "TAG-A" {
		t.Errorf("PriorReservation = %+v, %v, want the original", prior, ok)
	}
	collect(t, r.events)

	got, ok := r.svc.RevertReplacement(pending.ID)
	if !ok || got.ID != orig.ID || got.Status != core.ReservationActive || got.IDTag != "TAG-A" ||
		got.ResponseURL != "http://emsp/a" || !got.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("RevertReplacement = %+v, %v, want the original, active", got, ok)
	}
	if evs := collect(t, r.events); len(evs) != 0 {
		t.Errorf("events %+v, want none", evs)
	}
	if _, ok := r.svc.RevertReplacement(pending.ID); ok {
		t.Error("a reservation that is not a pending replacement was reverted")
	}
	r.clk.Advance(11 * time.Minute)
	if rs := r.reservations(t); len(rs) != 0 {
		t.Errorf("the original's expiry was not re-armed: %+v", rs)
	}
}
