package control_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
)

func ocpiReserve(r *rig, ocpiID string, expires time.Time) (core.Reservation, error) {
	return r.ctl.Reserve(context.Background(), control.ReserveParams{
		EVSEUID: "EVSE-1", IDTag: "NL-ABC-1", ExpiresAt: expires, Source: "ocpi",
		OCPIReservationID: ocpiID, ResponseURL: "http://emsp/" + ocpiID,
	})
}

// A reserve under an OCPI reservation_id we hold replaces that reservation,
// under the same OCPP id, as a ReserveNow with an id the station holds does
// there.
func TestReserveWithAKnownOCPIIDReplaces(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	first, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ocpiReserve(r, "res-1", t0.Add(40*time.Minute))
	if err != nil {
		t.Fatalf("the replacement: %v", err)
	}
	if second.ID != first.ID || !second.ExpiresAt.Equal(t0.Add(40*time.Minute)) {
		t.Fatalf("replacement = %+v, want id %d and the new expiry", second, first.ID)
	}
	if got := r.reservations(); len(got) != 1 {
		t.Fatalf("core holds %+v, want one reservation", got)
	}
	if len(r.station.reserve) != 2 || r.station.reserve[0].ReservationID != first.ID || r.station.reserve[1].ReservationID != first.ID {
		t.Fatalf("the station was sent %+v, want two ReserveNow under id %d", r.station.reserve, first.ID)
	}
	if len(r.station.cancels) != 0 {
		t.Errorf("CancelReservation sent: %v", r.station.cancels)
	}
	// Without an OCPI id, a second reservation on the EVSE is still refused.
	if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); !errors.Is(err, control.ErrAlreadyReserved) {
		t.Errorf("a reservation without an OCPI id on a held EVSE: %v, want ErrAlreadyReserved", err)
	}
	if _, err := ocpiReserve(r, "RES-2", t0.Add(40*time.Minute)); !errors.Is(err, control.ErrAlreadyReserved) {
		t.Errorf("another OCPI id on a held EVSE: %v, want ErrAlreadyReserved", err)
	}
}

// A replacement that fails sends no CancelReservation: the id is the one the
// station already holds, so the cancel would drop the original as well.
//
// When the station never saw the replacement (busy, or it answered no), what it
// holds is known: the original. We keep it, with its tag and expiry, so the
// driver it was made for can still redeem it, and report the failure.
func TestFailedReplacementThatTheStationRefusedOrNeverSawKeepsTheOriginal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fail func(r *rig)
	}{
		{"busy station", func(r *rig) { r.station.fail["reserve"] = ocpp.ErrNotSent }},
		{"closed before it was sent", func(r *rig) { r.station.fail["reserve"] = fmt.Errorf("ReserveNow: %w", ocppj.ErrClosedUnsent) }},
		{"refusal", func(r *rig) {
			r.station.answer["reserve"] = ocpp.Result{Status: ocpp.CommandOccupied, Raw: "Occupied"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			original, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			drain(r.events)

			tc.fail(r)
			if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{
				EVSEUID: "EVSE-1", IDTag: "NL-NEW-TAG", ExpiresAt: t0.Add(40 * time.Minute), Source: "ocpi",
				OCPIReservationID: "RES-1", ResponseURL: "http://emsp/new",
			}); err == nil {
				t.Fatal("the replacement succeeded")
			}

			got := r.reservations()
			if len(got) != 1 || got[0].ID != original.ID || got[0].Status != core.ReservationActive ||
				got[0].IDTag != "NL-ABC-1" || !got[0].ExpiresAt.Equal(t0.Add(10*time.Minute)) ||
				got[0].ResponseURL != "http://emsp/RES-1" {
				t.Fatalf("core holds %+v, want the original, active, with its tag, expiry and response URL", got)
			}
			if ok, until := r.core.TagAuthorisation("NL-ABC-1"); !ok || !until.Equal(t0.Add(10*time.Minute)) {
				t.Errorf("the original's tag: authorised %v until %v, want until its expiry", ok, until)
			}
			if r.core.TagAuthorised("NL-NEW-TAG") {
				t.Error("the tag of the replacement that never happened is authorised")
			}
			if got := r.store.Get().Reservations; len(got) != 1 || got[0].ID != original.ID {
				t.Errorf("state.json holds %+v, want the original", got)
			}
			if got := endedAs(drain(r.events)); len(got) != 0 {
				t.Errorf("reservation_ended events %v, want none: the reservation lives on", got)
			}
			if len(r.station.cancels) != 0 {
				t.Errorf("CancelReservation sent for %v", r.station.cancels)
			}
			// It still expires on time.
			r.clk.Advance(11 * time.Minute)
			if got := r.reservations(); len(got) != 0 {
				t.Errorf("the kept reservation did not expire: %+v", got)
			}
		})
	}
}

// A replacement whose outcome is unknown (no answer, or the connection went
// while it was out) may or may not have reached the station, and what the
// station holds is unknown too. The original ends locally, no cancel is sent,
// and the tags of both the original and the replacement stay authorised until
// their own expiry, so whichever the station holds can be redeemed.
func TestFailedReplacementWithAnUnknownOutcomeEndsTheOriginalButKeepsTheTagsAuthorised(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fail   error
		reason string
	}{
		{"silent station", ocpp.ErrTimeout, core.ReservationTimeout},
		{"dropped connection", ocpp.ErrNotConnected, core.ReservationFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if _, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			drain(r.events)

			r.station.fail["reserve"] = tc.fail
			if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{
				EVSEUID: "EVSE-1", IDTag: "NL-NEW-TAG", ExpiresAt: t0.Add(40 * time.Minute), Source: "ocpi",
				OCPIReservationID: "RES-1",
			}); !errors.Is(err, tc.fail) {
				t.Fatalf("err = %v, want %v", err, tc.fail)
			}

			// No background cancel is in flight: the record is already gone.
			if got := r.reservations(); len(got) != 0 {
				t.Fatalf("core still holds %+v", got)
			}
			time.Sleep(50 * time.Millisecond)
			if len(r.station.cancels) != 0 {
				t.Errorf("CancelReservation sent for %v", r.station.cancels)
			}
			if got := endedAs(drain(r.events)); len(got) != 1 || got[0] != tc.reason {
				t.Errorf("ended = %v, want [%s]", got, tc.reason)
			}
			if got := r.store.Get().Reservations; len(got) != 0 {
				t.Errorf("state.json still holds %+v", got)
			}

			for tag, end := range map[string]time.Time{"NL-ABC-1": t0.Add(10 * time.Minute), "NL-NEW-TAG": t0.Add(40 * time.Minute)} {
				if ok, until := r.core.TagAuthorisation(tag); !ok || !until.Equal(end) {
					t.Errorf("%s: authorised %v until %v, want until %v", tag, ok, until, end)
				}
			}
			r.clk.Advance(11 * time.Minute)
			if r.core.TagAuthorised("NL-ABC-1") {
				t.Error("the original's tag is still authorised after the original's expiry")
			}
			if !r.core.TagAuthorised("NL-NEW-TAG") {
				t.Error("the replacement's tag stopped before the replacement's expiry")
			}

			// The EVSE can be reserved again at once.
			r.station.fail = map[string]error{}
			if _, err := ocpiReserve(r, "RES-2", t0.Add(40*time.Minute)); err != nil {
				t.Errorf("a new reservation after the failed replacement: %v", err)
			}
		})
	}
}

// While the replacing ReserveNow is out, the station still holds the original,
// so its driver must not be turned away, and a crash must not lose it.
func TestOriginalStaysRedeemableWhileItsReplacementIsInFlight(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	r.station.reserveEntered, r.station.reserveGate = make(chan struct{}), gate

	done := make(chan error, 1)
	go func() { _, err := ocpiReserve(r, "RES-1", t0.Add(40*time.Minute)); done <- err }()
	select {
	case <-r.station.reserveEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the replacing ReserveNow never went out")
	}

	if ok, until := r.core.TagAuthorisation("NL-ABC-1"); !ok || !until.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("while the replacement is out: authorised %v until %v, want the original's tag until its expiry", ok, until)
	}
	if got := r.store.Get().Reservations; len(got) != 1 || !got[0].ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("state.json while the replacement is out holds %+v, want the original", got)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A failed first reservation still gets its best-effort cancel: only a
// replacement must not.
func TestFailedFirstReservationStillSendsACancel(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["reserve"] = ocpp.ErrTimeout

	if _, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute)); !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	r.waitCancelled(t)
	if len(r.station.cancels) != 1 {
		t.Errorf("cancels = %v, want one", r.station.cancels)
	}
}

// CancelForPartner ends the reservation under a reason of its own.
func TestCancelForPartner(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	drain(r.events)

	if err := r.ctl.CancelForPartner(context.Background(), "EVSE-1"); err != nil {
		t.Fatal(err)
	}
	evs := drain(r.events)
	if got := endedAs(evs); len(got) != 1 || got[0] != "cancelled by partner" {
		t.Fatalf("ended = %v, want [cancelled by partner]", got)
	}
	if evs[0].ResponseURL != "http://emsp/RES-1" {
		t.Errorf("response URL = %q", evs[0].ResponseURL)
	}
}
