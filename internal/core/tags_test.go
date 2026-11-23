package core_test

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

func tagRig(t *testing.T) (*core.Service, *clocktest.Clock) {
	t.Helper()

	cfg := testConfig()
	cfg.Auth = config.Auth{DefaultIDTag: "DEFAULT-TAG", IDTags: []string{"extra-tag"}}
	clk := clocktest.New(t0)
	svc := core.New(cfg,
		core.WithClock(clk.Now),
		core.WithAfterFunc(func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) }),
		core.WithStore(statetest.New(t)),
	)
	t.Cleanup(svc.Close)
	return svc, clk
}

// The configured tags are accepted whatever their case; nothing else is.
func TestTagAuthorisedAcceptsTheConfiguredTags(t *testing.T) {
	t.Parallel()
	svc, _ := tagRig(t)

	for tag, want := range map[string]bool{
		"DEFAULT-TAG": true, "default-tag": true, "EXTRA-TAG": true, "NL-ABC-C0001": false, "": false,
	} {
		if got := svc.TagAuthorised(tag); got != want {
			t.Errorf("TagAuthorised(%q) = %v, want %v", tag, got, want)
		}
	}
}

// The tag of an active reservation is accepted so that it can be redeemed,
// but not before the station accepted the reservation, and not after it ended.
func TestTagAuthorisedAcceptsAnActiveReservationsTag(t *testing.T) {
	t.Parallel()
	svc, clk := tagRig(t)

	pending, err := svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "NL-ABC-C0001", ExpiresAt: t0.Add(time.Hour), Source: "ocpi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.TagAuthorised("NL-ABC-C0001") {
		t.Error("the tag of a reservation the station has not accepted is authorised")
	}
	svc.ActivateReservation(pending.ID)
	if !svc.TagAuthorised("NL-ABC-C0001") || !svc.TagAuthorised("nl-abc-c0001") {
		t.Error("the tag of an active reservation is not authorised")
	}
	svc.EndReservation(pending.ID, core.ReservationCancelled)
	if svc.TagAuthorised("NL-ABC-C0001") {
		t.Error("the tag of a cancelled reservation is still authorised")
	}

	// Expiry ends it too.
	again, _ := svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "NL-ABC-C0002", ExpiresAt: t0.Add(time.Minute), Source: "ocpi",
	})
	svc.ActivateReservation(again.ID)
	clk.Advance(2 * time.Minute)
	if svc.TagAuthorised("NL-ABC-C0002") {
		t.Error("the tag of an expired reservation is still authorised")
	}
}

// A START_SESSION's tag is accepted for the 15 minutes after the command.
func TestAuthoriseTagLasts15Minutes(t *testing.T) {
	t.Parallel()
	svc, clk := tagRig(t)

	svc.AuthoriseTag("NL-ABC-C0003", core.TagGrantWindow)
	if !svc.TagAuthorised("nl-abc-c0003") {
		t.Fatal("the tag is not authorised right after the command")
	}
	clk.Advance(14*time.Minute + 59*time.Second)
	if !svc.TagAuthorised("NL-ABC-C0003") {
		t.Error("the tag expired before 15 minutes")
	}
	clk.Advance(2 * time.Second)
	if svc.TagAuthorised("NL-ABC-C0003") {
		t.Error("the tag is still authorised after 15 minutes")
	}

	// A second command extends the window, and never shortens it.
	svc.AuthoriseTag("NL-ABC-C0004", core.TagGrantWindow) // until +15m
	clk.Advance(10 * time.Minute)
	svc.AuthoriseTag("NL-ABC-C0004", time.Minute) // would be +11m
	clk.Advance(3 * time.Minute)
	if !svc.TagAuthorised("NL-ABC-C0004") {
		t.Error("a second command with a shorter window shortened the first")
	}
	svc.AuthoriseTag("NL-ABC-C0004", core.TagGrantWindow) // now until +28m
	clk.Advance(10 * time.Minute)
	if !svc.TagAuthorised("NL-ABC-C0004") {
		t.Error("a second command did not extend the window")
	}
	if svc.TagAuthorised("NL-ABC-C0003") {
		t.Error("the grant of another tag came back")
	}
}
