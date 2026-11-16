package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/state"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

func profile(id int, evse, purpose string, stack int, limit float64) core.ChargingProfile {
	return core.ChargingProfile{
		ID: id, ChargePointID: testCP, EVSEUID: evse, Purpose: purpose, StackLevel: stack,
		Unit: "W", Periods: []core.ChargingPeriod{{Start: 0, Limit: limit}}, Source: "cli",
	}
}

func ids(ps []core.ChargingProfile) []int {
	out := []int{}
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRecordedProfilesReplaceLikeTheStation(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	r.svc.RecordChargingProfile(profile(1, "EVSE-1", core.PurposeTxDefault, 0, 20000))
	r.svc.RecordChargingProfile(profile(2, "EVSE-1", core.PurposeTxDefault, 1, 15000))
	r.svc.RecordChargingProfile(profile(3, "", core.PurposeStationMax, 0, 30000))
	// Same EVSE, purpose and stack level as 1: replaces it.
	r.svc.RecordChargingProfile(profile(4, "EVSE-1", core.PurposeTxDefault, 0, 10000))
	// Same id as 2, on another EVSE: replaces it too.
	r.svc.RecordChargingProfile(profile(2, "EVSE-2", core.PurposeTxDefault, 5, 5000))

	if got := ids(r.svc.ChargingProfiles("EVSE-1")); !sameInts(got, []int{4}) {
		t.Errorf("EVSE-1 profiles = %v, want [4]", got)
	}
	if got := ids(r.svc.ChargingProfiles("EVSE-2")); !sameInts(got, []int{2}) {
		t.Errorf("EVSE-2 profiles = %v, want [2]", got)
	}
	if got := ids(r.svc.ChargingProfiles("")); !sameInts(got, []int{3}) {
		t.Errorf("station profiles = %v, want [3]", got)
	}

	var set, cleared int
	for _, e := range collect(t, r.events) {
		switch e.Kind {
		case core.EventChargingProfileSet:
			set++
		case core.EventChargingProfileCleared:
			cleared++
			if e.Detail != core.ProfileReplaced {
				t.Errorf("cleared event detail %q, want %q", e.Detail, core.ProfileReplaced)
			}
		}
	}
	if set != 5 || cleared != 2 {
		t.Errorf("%d set and %d cleared events, want 5 and 2", set, cleared)
	}
}

func TestForgetChargingProfilesByFilter(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	r.svc.RecordChargingProfile(profile(1, "EVSE-1", core.PurposeTxDefault, 0, 20000))
	r.svc.RecordChargingProfile(profile(2, "EVSE-1", core.PurposeTxDefault, 1, 15000))
	r.svc.RecordChargingProfile(profile(3, "", core.PurposeStationMax, 0, 30000))
	r.svc.RecordChargingProfile(profile(4, "EVSE-2", core.PurposeTxDefault, 0, 30000))

	station := ""
	if got := ids(r.svc.ForgetChargingProfiles(testCP, core.ProfileFilter{EVSEUID: &station}, core.ProfileCleared)); !sameInts(got, []int{3}) {
		t.Errorf("forgetting the station's profiles removed %v, want [3]", got)
	}
	evse, stack := "EVSE-1", 1
	if got := ids(r.svc.ForgetChargingProfiles(testCP, core.ProfileFilter{EVSEUID: &evse, StackLevel: &stack}, core.ProfileCleared)); !sameInts(got, []int{2}) {
		t.Errorf("forgetting EVSE-1 stack 1 removed %v, want [2]", got)
	}
	if got := ids(r.svc.ForgetChargingProfiles("OTHER-CP", core.ProfileFilter{}, core.ProfileCleared)); len(got) != 0 {
		t.Errorf("forgetting another charge point's profiles removed %v", got)
	}
	if got := ids(r.svc.ForgetChargingProfiles(testCP, core.ProfileFilter{}, core.ProfileCleared)); !sameInts(got, []int{1, 4}) {
		t.Errorf("forgetting everything removed %v, want [1 4]", got)
	}
	if st := r.store.Get(); len(st.ChargingProfiles) != 0 {
		t.Errorf("state.json still holds %+v", st.ChargingProfiles)
	}
}

// The spec ends a TxProfile with its transaction; the TxDefault on the same
// EVSE stays.
func TestStoppingATransactionDropsItsTxProfile(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	r.svc.RecordChargingProfile(profile(1, "EVSE-1", core.PurposeTxDefault, 0, 20000))
	txID := r.startTx(1, "TAG", 0)
	tx := profile(2, "EVSE-1", core.PurposeTx, 0, 10000)
	tx.TransactionID = txID
	r.svc.RecordChargingProfile(tx)
	other := profile(3, "EVSE-2", core.PurposeTx, 0, 10000)
	other.TransactionID = "999"
	r.svc.RecordChargingProfile(other)
	collect(t, r.events)

	if _, ok := r.svc.StopTransaction(testCP, txID, 100, "Remote", time.Time{}); !ok {
		t.Fatal("StopTransaction found nothing")
	}
	if got := ids(r.svc.ChargingProfiles("EVSE-1")); !sameInts(got, []int{1}) {
		t.Errorf("EVSE-1 profiles after the stop = %v, want only the TxDefault [1]", got)
	}
	if got := ids(r.svc.ChargingProfiles("EVSE-2")); !sameInts(got, []int{3}) {
		t.Errorf("another transaction's TxProfile went too: EVSE-2 = %v", got)
	}
	var dropped []core.Event
	for _, e := range collect(t, r.events) {
		if e.Kind == core.EventChargingProfileCleared {
			dropped = append(dropped, e)
		}
	}
	if len(dropped) != 1 || dropped[0].ChargingProfileID != 2 || dropped[0].Detail != core.ProfileTransactionEnded {
		t.Errorf("cleared events = %+v, want profile 2, %q", dropped, core.ProfileTransactionEnded)
	}
	if st := r.store.Get(); len(st.ChargingProfiles) != 2 {
		t.Errorf("state.json holds %d profiles, want 2", len(st.ChargingProfiles))
	}
}

// A TxProfile sent with --force while no session ran has no transaction id.
// A station that kept it applies it to the next session on the EVSE and
// drops it when that session ends, so the record goes at the next stop on
// the same EVSE; without this it would be listed until a clear.
func TestStoppingATransactionDropsAnUnboundTxProfileOnItsEVSE(t *testing.T) {
	t.Parallel()
	r := newRig(t, clocktest.New(t0), statetest.New(t))

	r.svc.RecordChargingProfile(profile(1, "EVSE-1", core.PurposeTx, 0, 15000))
	r.svc.RecordChargingProfile(profile(2, "EVSE-2", core.PurposeTx, 0, 15000))
	txID := r.startTx(1, "TAG", 0)
	collect(t, r.events)

	if _, ok := r.svc.StopTransaction(testCP, txID, 100, "Remote", time.Time{}); !ok {
		t.Fatal("StopTransaction found nothing")
	}
	if got := ids(r.svc.ChargingProfiles("EVSE-1")); len(got) != 0 {
		t.Errorf("EVSE-1 profiles after the stop = %v, want none", got)
	}
	if got := ids(r.svc.ChargingProfiles("EVSE-2")); !sameInts(got, []int{2}) {
		t.Errorf("another EVSE's unbound TxProfile went too: EVSE-2 = %v", got)
	}
}

func TestChargingProfilesAndTheirCounterSurviveARestart(t *testing.T) {
	t.Parallel()
	store := statetest.New(t)
	clk := clocktest.New(t0)
	r := newRig(t, clk, store)

	id, err := r.svc.NextChargingProfileID()
	if err != nil || id != 1 {
		t.Fatalf("first profile id = %d, %v; want 1", id, err)
	}
	until := t0.Add(time.Hour)
	p := profile(id, "EVSE-1", core.PurposeTxDefault, 2, 20000)
	p.ValidTo = &until
	r.svc.RecordChargingProfile(p)
	// Ids handed out but never recorded (the station refused) are not
	// reused either: the counter itself survives, not only the records.
	for range 6 {
		if _, err := r.svc.NextChargingProfileID(); err != nil {
			t.Fatal(err)
		}
	}
	// A Tx profile whose transaction ended while cpms was down is dropped.
	txID := r.startTx(1, "TAG", 0)
	tx := profile(5, "EVSE-1", core.PurposeTx, 0, 10000)
	tx.TransactionID = txID
	r.svc.RecordChargingProfile(tx)
	if err := store.Update(func(st *state.State) error {
		st.Transactions = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	again := newRig(t, clk, statetest.Open(t, store.Path()))
	got := again.svc.ChargingProfiles("EVSE-1")
	if len(got) != 1 || got[0].ID != 1 || got[0].StackLevel != 2 || got[0].Periods[0].Limit != 20000 ||
		got[0].ValidTo == nil || !got[0].ValidTo.Equal(until) || got[0].Source != "cli" {
		t.Fatalf("after restart: %+v, want the TxDefault record only", got)
	}
	next, err := again.svc.NextChargingProfileID()
	if err != nil || next != 8 {
		t.Errorf("next profile id after restart = %d, %v; want 8 (the persisted counter)", next, err)
	}
}

// A file whose counter lags behind its records, edited by hand or written
// before the counter existed, must not lead to an id being handed out twice.
func TestProfileCounterNeverFallsBehindTheRecords(t *testing.T) {
	t.Parallel()
	store := statetest.New(t)
	if err := store.Update(func(st *state.State) error {
		st.NextChargingProfileID = 1
		st.ChargingProfiles = []state.ChargingProfile{{
			ID: 41, ChargePointID: testCP, EVSEUID: "EVSE-1", Purpose: core.PurposeTxDefault, Unit: "W",
			Periods: []state.ChargingPeriod{{Limit: 11000}},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, clocktest.New(t0), store)
	if id, err := r.svc.NextChargingProfileID(); err != nil || id != 42 {
		t.Errorf("next profile id = %d, %v; want 42", id, err)
	}
}

func TestNoProfileIDWhenTheCounterCannotBePersisted(t *testing.T) {
	t.Parallel()
	store := statetest.New(t)
	r := newRig(t, clocktest.New(t0), store)

	store.Fail(true)
	if _, err := r.svc.NextChargingProfileID(); !errors.Is(err, statetest.ErrInjected) {
		t.Errorf("NextChargingProfileID with a failing store = %v, want the write error", err)
	}
}
