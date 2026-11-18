package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// A station that reconnects can start a new session on a connector without
// ever stopping the old one (#73). One connector runs one session, so the
// new start ends the old one: it is stopped, its Tx profile goes with it, it
// leaves state.json, and another connector's session is left alone.
func TestSecondStartOnAConnectorEndsTheFirst(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	r.svc.StartTransaction(testCP, core.TransactionStart{ID: "1", ConnectorID: 1, IDTag: "TAG-A", MeterStart: 1000})
	r.svc.StartTransaction(testCP, core.TransactionStart{ID: "2", ConnectorID: 2, IDTag: "TAG-B", MeterStart: 500})
	bound := profile(1, "EVSE-1", core.PurposeTx, 0, 10000)
	bound.TransactionID = "1"
	r.svc.RecordChargingProfile(bound)
	// Sent without a transaction id, it may be the new session's at the
	// station, so superseding the old one leaves it.
	r.svc.RecordChargingProfile(profile(2, "EVSE-1", core.PurposeTx, 1, 12000))
	collect(t, r.events)

	r.clk.Advance(time.Hour)
	second := r.clk.Now().Add(-5 * time.Minute) // the station's timestamp, not our clock
	r.svc.StartTransaction(testCP, core.TransactionStart{ID: "3", ConnectorID: 1, IDTag: "TAG-A", MeterStart: 9000, At: second})

	active := r.svc.ActiveTransactions("EVSE-1")
	if len(active) != 1 || active[0].ID != "3" {
		t.Fatalf("active on EVSE-1 = %+v, want only 3", active)
	}
	if other := r.svc.ActiveTransactions("EVSE-2"); len(other) != 1 || other[0].ID != "2" {
		t.Errorf("active on EVSE-2 = %+v, want 2 untouched", other)
	}

	events := collect(t, r.events)
	var got []string
	for _, e := range events {
		got = append(got, string(e.Kind)+":"+e.TransactionID)
	}
	want := []string{"transaction_stopped:1", "charging_profile_cleared:1", "transaction_started:3"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	stop := events[0]
	// "Other": a reason from OCPP 1.6's list, so it can be passed on as is.
	if stop.Detail != "Other" || !stop.At.Equal(second) || stop.EVSEUID != "EVSE-1" {
		t.Errorf("stop event = %+v, want reason %q at the new start on EVSE-1", stop, core.ReasonSuperseded)
	}
	if events[1].ChargingProfileID != 1 {
		t.Errorf("cleared profile %d, want 1 (bound to the old session)", events[1].ChargingProfileID)
	}
	if left := ids(r.svc.ChargingProfiles("EVSE-1")); !sameInts(left, []int{2}) {
		t.Errorf("EVSE-1 profiles = %v, want the unbound one [2]", left)
	}

	var persisted []string
	for _, tx := range r.store.Get().Transactions {
		persisted = append(persisted, tx.ID)
	}
	if strings.Join(persisted, ",") != "2,3" {
		t.Errorf("state.json transactions = %v, want [2 3]", persisted)
	}
	if !strings.Contains(r.logs.String(), "superseded_by=3") {
		t.Errorf("no warning naming the new session; log:\n%s", r.logs.String())
	}

	// Its StopTransaction may still come, late; it finds nothing to stop
	// and must not touch the running session.
	if _, ok := r.svc.StopTransaction(testCP, "1", 1200, "EVDisconnected", time.Time{}); ok {
		t.Error("a late stop for the superseded session was taken as a stop")
	}
	if active := r.svc.ActiveTransactions("EVSE-1"); len(active) != 1 || active[0].ID != "3" {
		t.Errorf("after the late stop, active on EVSE-1 = %+v, want 3", active)
	}
}

// The sequence seen on a real station: cpms restarts, the station reconnects
// and starts again on the same connector. The old session comes from
// state.json, not from this process.
func TestStartAfterRestartEndsTheSessionFromStateFile(t *testing.T) {
	t.Parallel()
	store := statetest.New(t)
	clk := clocktest.New(t0)

	first := newRig(t, clk, store)
	first.svc.StartTransaction(testCP, core.TransactionStart{ID: "1", ConnectorID: 1, IDTag: "TAG-A"})
	first.svc.Close()

	again := newRig(t, clk, statetest.Open(t, store.Path()))
	again.svc.StartTransaction(testCP, core.TransactionStart{ID: "2", ConnectorID: 1, IDTag: "TAG-A"})

	if active := again.svc.ActiveTransactions("EVSE-1"); len(active) != 1 || active[0].ID != "2" {
		t.Fatalf("active on EVSE-1 = %+v, want only 2", active)
	}
	if txs := again.store.Get().Transactions; len(txs) != 1 || txs[0].ID != "2" {
		t.Errorf("state.json transactions = %+v, want only 2", txs)
	}
}

// A station that starts a session with an id it still has active is a
// replacement, not a second session: the id is the one the station knows, so
// the old entry is overwritten and nothing has ended. Ending it first would
// publish a stop the station never made and drop the Tx profile and live
// reading the session still uses.
func TestStartWithTheSameIDOnAConnectorReplacesItWithoutEndingIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	r.svc.StartTransaction(testCP, core.TransactionStart{ID: "7", ConnectorID: 1, IDTag: "TAG-A", MeterStart: 1000})
	bound := profile(1, "EVSE-1", core.PurposeTx, 0, 10000)
	bound.TransactionID = "7"
	r.svc.RecordChargingProfile(bound)
	r.svc.RecordMeterValues(testCP, 1, "", core.MeterReading{At: t0.Add(time.Minute), PowerW: watts(11000)})
	collect(t, r.events)

	r.svc.StartTransaction(testCP, core.TransactionStart{ID: "7", ConnectorID: 1, IDTag: "TAG-A", MeterStart: 1000})

	var got []string
	for _, e := range collect(t, r.events) {
		got = append(got, string(e.Kind)+":"+e.TransactionID)
	}
	if strings.Join(got, " ") != "transaction_started:7" {
		t.Errorf("events = %v, want only transaction_started:7", got)
	}
	if active := r.svc.ActiveTransactions("EVSE-1"); len(active) != 1 || active[0].ID != "7" {
		t.Errorf("active on EVSE-1 = %+v, want only 7", active)
	}
	if left := ids(r.svc.ChargingProfiles("EVSE-1")); !sameInts(left, []int{1}) {
		t.Errorf("EVSE-1 profiles = %v, want the one bound to 7 kept", left)
	}
	if rd := reading(t, r.svc, "EVSE-1"); rd == nil || rd.PowerW == nil || *rd.PowerW != 11000 {
		t.Errorf("EVSE-1 reading = %+v, want it kept", rd)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "reused an active transaction id") {
		t.Errorf("no reused-id warning; log:\n%s", logs)
	}
	if strings.Contains(logs, "superseded_by") {
		t.Errorf("the reused id was treated as superseded; log:\n%s", logs)
	}
}
