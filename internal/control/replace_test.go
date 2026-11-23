package control_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
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
// station already holds, so the cancel would drop the original as well. Our
// record of the original ends with it, since the station's state is unknown.
func TestFailedReplacementSendsNoCancelAndEndsTheOriginal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fail   func(r *rig)
		reason string
	}{
		{"silent station", func(r *rig) { r.station.fail["reserve"] = ocpp.ErrTimeout }, core.ReservationTimeout},
		{"dropped connection", func(r *rig) { r.station.fail["reserve"] = ocpp.ErrNotConnected }, core.ReservationFailed},
		{"busy station", func(r *rig) { r.station.fail["reserve"] = ocpp.ErrNotSent }, core.ReservationFailed},
		{"refusal", func(r *rig) {
			r.station.answer["reserve"] = ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}
		}, core.ReservationRejected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if _, err := ocpiReserve(r, "RES-1", t0.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			drain(r.events)

			tc.fail(r)
			if _, err := ocpiReserve(r, "RES-1", t0.Add(40*time.Minute)); err == nil {
				t.Fatal("the replacement succeeded")
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
			// The EVSE can be reserved again at once.
			r.station.fail, r.station.answer = map[string]error{}, map[string]ocpp.Result{}
			if _, err := ocpiReserve(r, "RES-2", t0.Add(40*time.Minute)); err != nil {
				t.Errorf("a new reservation after the failed replacement: %v", err)
			}
		})
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
