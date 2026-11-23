package v16_test

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// A tag the OCPI partner vouched for is accepted at the station, in
// Authorize and in StartTransaction, although it is not auth.default_id_tag:
// the tag of an active reservation, so that it can be redeemed, and the tag of
// a START_SESSION. Any other tag still is not.
func TestOCPITagsAreAccepted(t *testing.T) {
	t.Parallel()
	const reservedTag, startTag = "NL-ABC-RESERVED", "NL-ABC-START"

	h, svc := newHandler(t)
	svc.Connected(testCP)
	pending, err := svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: reservedTag,
		ExpiresAt: time.Now().Add(time.Hour), Source: "ocpi",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.AuthoriseTag(startTag, core.TagGrantWindow)

	authorize := func(tag string) string {
		return call[v16.AuthorizeConf](t, h, v16.ActionAuthorize, v16.AuthorizeReq{IDTag: tag}).IDTagInfo.Status
	}
	if got := authorize(reservedTag); got != v16.AuthInvalid {
		t.Errorf("Authorize of a reservation the station has not accepted yet = %s, want Invalid", got)
	}
	svc.ActivateReservation(pending.ID)

	for tag, want := range map[string]string{
		reservedTag: v16.AuthAccepted, "nl-abc-reserved": v16.AuthAccepted,
		startTag: v16.AuthAccepted, "nl-abc-start": v16.AuthAccepted,
		testTag: v16.AuthAccepted, "NL-ABC-OTHER": v16.AuthInvalid,
	} {
		if got := authorize(tag); got != want {
			t.Errorf("Authorize(%q) = %s, want %s", tag, got, want)
		}
	}

	// Redeeming the reservation with its tag opens the transaction.
	conf := call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: reservedTag, MeterStart: 0, ReservationID: &pending.ID,
	})
	if conf.IDTagInfo.Status != v16.AuthAccepted || conf.TransactionID < 1 {
		t.Fatalf("StartTransaction with the reserved tag = %+v, want accepted", conf)
	}
	if _, ok := svc.ActiveReservation("EVSE-1"); ok {
		t.Error("the reservation was not consumed")
	}

	// A session for a START_SESSION's tag.
	conf = call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 2, IDTag: startTag,
	})
	if conf.IDTagInfo.Status != v16.AuthAccepted {
		t.Errorf("StartTransaction with a START_SESSION tag = %+v, want accepted", conf)
	}

	// Once the reservation is gone, so is its tag.
	if got := authorize(reservedTag); got != v16.AuthInvalid {
		t.Errorf("Authorize of the tag of a consumed reservation = %s, want Invalid", got)
	}
}
