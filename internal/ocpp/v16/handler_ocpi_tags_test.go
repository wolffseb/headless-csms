package v16_test

import (
	"io"
	"log/slog"
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

// A tag that cpms vouches for only for a while carries that end in the
// idTagInfo, in Authorize.conf and in StartTransaction.conf: the station keeps
// every idTagInfo it receives in its Authorization Cache, and with no
// expiryDate the entry has no end (OCPP 1.6 §7.27). A configured tag has no
// end, so it carries none.
func TestTemporaryTagsCarryTheirEndInTheIDTagInfo(t *testing.T) {
	t.Parallel()
	const reservedTag, startTag = "NL-ABC-RESERVED", "NL-ABC-START"
	reservationEnd := testNow.Add(40 * time.Minute)
	grantEnd := testNow.Add(core.TagGrantWindow)

	cfg := testConfig()
	svc := core.New(cfg, core.WithClock(func() time.Time { return testNow }))
	t.Cleanup(svc.Close)
	h := v16.NewHandler(cfg, svc, slog.New(slog.NewTextHandler(io.Discard, nil)),
		v16.WithClock(func() time.Time { return testNow }))
	svc.Connected(testCP)

	pending, err := svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: reservedTag, ExpiresAt: reservationEnd, Source: "ocpi",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.ActivateReservation(pending.ID)
	svc.AuthoriseTag(startTag, core.TagGrantWindow)

	tests := []struct {
		name      string
		tag       string
		connector int
		want      string // the expiryDate, empty for none
	}{
		{"a configured tag has no end", testTag, 2, ""},
		{"a reservation's tag ends with the reservation", reservedTag, 1, reservationEnd.Format(time.RFC3339)},
		{"a START_SESSION's tag ends with the grant window", startTag, 2, grantEnd.Format(time.RFC3339)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth := call[v16.AuthorizeConf](t, h, v16.ActionAuthorize, v16.AuthorizeReq{IDTag: tc.tag})
			if auth.IDTagInfo.Status != v16.AuthAccepted || auth.IDTagInfo.ExpiryDate != tc.want {
				t.Errorf("Authorize.conf idTagInfo = %+v, want Accepted with expiryDate %q", auth.IDTagInfo, tc.want)
			}
			start := call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
				ConnectorID: tc.connector, IDTag: tc.tag, ReservationID: reservationIDFor(tc.tag == reservedTag, pending.ID),
			})
			if start.IDTagInfo.Status != v16.AuthAccepted || start.IDTagInfo.ExpiryDate != tc.want {
				t.Errorf("StartTransaction.conf idTagInfo = %+v, want Accepted with expiryDate %q", start.IDTagInfo, tc.want)
			}
		})
	}
}

func reservationIDFor(redeems bool, id int) *int {
	if !redeems {
		return nil
	}
	return &id
}
