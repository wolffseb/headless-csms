package control_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
)

func f64(v float64) *float64 { return &v }

func TestSetLimitBuildsOneAbsolutePeriodAndRecordsIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	rec, err := r.ctl.SetLimit(context.Background(), control.LimitParams{
		EVSEUID: "EVSE-1", KW: f64(7.4), StackLevel: 2, For: time.Hour, Source: "cli",
	})
	if err != nil {
		t.Fatalf("SetLimit: %v", err)
	}

	sent := r.station.profiles[0]
	if sent.ID != rec.ID || sent.ID != 1 || sent.Purpose != ocpp.PurposeTxDefault || sent.Kind != ocpp.KindAbsolute ||
		sent.StackLevel != 2 || sent.EVSEUID != "EVSE-1" {
		t.Errorf("sent %+v, want TxDefault Absolute id 1 stack 2 on EVSE-1", sent)
	}
	sched := sent.Schedule
	if sched.Unit != ocpp.UnitW || len(sched.Periods) != 1 || sched.Periods[0].Limit != 7400 || sched.Periods[0].Start != 0 {
		t.Errorf("schedule %+v, want one period of exactly 7400 W from 0", sched)
	}
	if sched.StartSchedule == nil || !sched.StartSchedule.Equal(t0) ||
		sched.Duration == nil || *sched.Duration != time.Hour ||
		sent.ValidTo == nil || !sent.ValidTo.Equal(t0.Add(time.Hour)) {
		t.Errorf("start %v, duration %v, validTo %v; want now, 1h, now+1h", sched.StartSchedule, sched.Duration, sent.ValidTo)
	}

	got := r.core.ChargingProfiles("EVSE-1")
	if len(got) != 1 || got[0].ID != 1 || got[0].Unit != "W" || got[0].Periods[0].Limit != 7400 || got[0].Source != "cli" {
		t.Errorf("records = %+v, want the accepted profile", got)
	}
	if st := r.store.Get(); len(st.ChargingProfiles) != 1 || st.NextChargingProfileID != 2 {
		t.Errorf("state.json: %d profiles, next id %d; want 1 and 2", len(st.ChargingProfiles), st.NextChargingProfileID)
	}
}

func TestSetLimitInAmpsOnTheStation(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	if _, err := r.ctl.SetLimit(context.Background(), control.LimitParams{
		Amps: f64(32), Purpose: ocpp.PurposeStationMax,
	}); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	sent := r.station.profiles[0]
	if sent.EVSEUID != "" || sent.Schedule.Unit != ocpp.UnitA || sent.Schedule.Periods[0].Limit != 32 ||
		sent.Schedule.Duration != nil || sent.ValidTo != nil {
		t.Errorf("sent %+v, want 32 A on the station, open-ended", sent)
	}
	if got := r.core.ChargingProfiles(""); len(got) != 1 {
		t.Errorf("station records = %+v", got)
	}
}

// A profile the station does not accept is not recorded, and cpms does not
// claim a limit it does not know is applied.
func TestRefusedProfileIsNotRecorded(t *testing.T) {
	t.Parallel()

	for _, status := range []ocpp.CommandStatus{ocpp.CommandRejected, ocpp.CommandNotSupported} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.station.answer["set-profile"] = ocpp.Result{Status: status, Raw: string(status)}

			_, err := r.ctl.SetLimit(context.Background(), control.LimitParams{EVSEUID: "EVSE-1", KW: f64(20)})
			var refused *control.RefusedError
			if !errors.As(err, &refused) || refused.Status != status || refused.Op != control.OpSetProfile {
				t.Fatalf("err = %v, want a %s refusal", err, status)
			}
			if got := r.core.ChargingProfiles("EVSE-1"); len(got) != 0 {
				t.Errorf("recorded %+v", got)
			}
		})
	}
}

func TestSetLimitRefusesBadParamsBeforeSending(t *testing.T) {
	t.Parallel()

	tests := map[string]control.LimitParams{
		"both units":            {EVSEUID: "EVSE-1", KW: f64(1), Amps: f64(1)},
		"no unit":               {EVSEUID: "EVSE-1"},
		"negative for":          {EVSEUID: "EVSE-1", KW: f64(1), For: -time.Minute},
		"tx default on station": {KW: f64(1)},
		"station max on EVSE":   {EVSEUID: "EVSE-1", KW: f64(1), Purpose: ocpp.PurposeStationMax},
		"two decimals of A":     {EVSEUID: "EVSE-1", Amps: f64(6.25)},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if _, err := r.ctl.SetLimit(context.Background(), p); !errors.Is(err, ocpp.ErrInvalidProfile) {
				t.Errorf("err = %v, want ErrInvalidProfile", err)
			}
			if n := r.station.callCount(); n != 0 {
				t.Errorf("%d calls sent", n)
			}
			if st := r.store.Get(); st.NextChargingProfileID > 1 {
				t.Errorf("an id was allocated: next is %d", st.NextChargingProfileID)
			}
		})
	}
}

func TestTxLimitPicksTheRunningTransaction(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	// No session: refused locally, nothing sent, no id spent.
	_, err := r.ctl.SetLimit(context.Background(), control.LimitParams{EVSEUID: "EVSE-1", KW: f64(10), Purpose: ocpp.PurposeTx})
	if !errors.Is(err, control.ErrNoActiveTransaction) {
		t.Fatalf("err = %v, want ErrNoActiveTransaction", err)
	}
	if n := r.station.callCount(); n != 0 {
		t.Fatalf("%d calls sent", n)
	}

	// Forced: sent without a transaction id.
	if _, err := r.ctl.SetLimit(context.Background(), control.LimitParams{
		EVSEUID: "EVSE-1", KW: f64(10), Purpose: ocpp.PurposeTx, Force: true,
	}); err != nil {
		t.Fatalf("forced SetLimit: %v", err)
	}
	if sent := r.station.profiles[0]; sent.TransactionID != "" || sent.Purpose != ocpp.PurposeTx {
		t.Errorf("forced profile %+v, want a Tx profile without a transaction", sent)
	}

	txID := strconv.Itoa(r.core.NextTransactionID())
	r.core.StartTransaction(testCP, core.TransactionStart{ID: txID, ConnectorID: 1, IDTag: testTag})
	rec, err := r.ctl.SetLimit(context.Background(), control.LimitParams{EVSEUID: "EVSE-1", KW: f64(10), Purpose: ocpp.PurposeTx})
	if err != nil {
		t.Fatalf("SetLimit during a session: %v", err)
	}
	if sent := r.station.profiles[1]; sent.TransactionID != txID || rec.TransactionID != txID {
		t.Errorf("sent transaction %q, recorded %q; want %s", sent.TransactionID, rec.TransactionID, txID)
	}
}

// A state.json from before #73 can hold two sessions on one EVSE; --tx
// chooses. A named session must be running on the EVSE, and goes only with a
// tx limit.
func TestTxLimitForANamedTransaction(t *testing.T) {
	t.Parallel()
	r := newRigOn(t, twoActiveOnEVSE1(t))
	tx := func(id string) control.LimitParams {
		return control.LimitParams{EVSEUID: "EVSE-1", KW: f64(10), Purpose: ocpp.PurposeTx, TransactionID: id}
	}

	if _, err := r.ctl.SetLimit(context.Background(), tx("")); err == nil || !strings.Contains(err.Error(), "--tx") {
		t.Fatalf("err = %v, want a refusal to guess that points at --tx", err)
	}
	if _, err := r.ctl.SetLimit(context.Background(), tx("99")); !errors.Is(err, control.ErrNoActiveTransaction) {
		t.Errorf("unknown transaction: err = %v, want ErrNoActiveTransaction", err)
	}
	notTx := tx("41")
	notTx.Purpose = ocpp.PurposeTxDefault
	if _, err := r.ctl.SetLimit(context.Background(), notTx); !errors.Is(err, ocpp.ErrInvalidProfile) {
		t.Errorf("tx-default with a transaction: err = %v, want ErrInvalidProfile", err)
	}
	if n := r.station.callCount(); n != 0 {
		t.Fatalf("%d calls sent before a valid request", n)
	}

	rec, err := r.ctl.SetLimit(context.Background(), tx("42"))
	if err != nil {
		t.Fatalf("SetLimit for 42: %v", err)
	}
	if sent := r.station.profiles[0]; sent.TransactionID != "42" || rec.TransactionID != "42" {
		t.Errorf("sent transaction %q, recorded %q; want 42", sent.TransactionID, rec.TransactionID)
	}
}

func TestSetProfileAllocatesItsOwnID(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	p := ocpp.ChargingProfile{
		ID: 999, Purpose: ocpp.PurposeTxDefault, Kind: ocpp.KindRelative, EVSEUID: "ignored",
		Schedule: ocpp.ChargingSchedule{Unit: ocpp.UnitA, Periods: []ocpp.SchedulePeriod{{Limit: 16}, {Start: time.Minute, Limit: 10}}},
	}
	rec, err := r.ctl.SetProfile(context.Background(), "EVSE-2", p, "cli")
	if err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if sent := r.station.profiles[0]; sent.ID != 1 || sent.EVSEUID != "EVSE-2" || rec.ID != 1 || len(rec.Periods) != 2 {
		t.Errorf("sent %+v, recorded %+v; want id 1 on EVSE-2 with both periods", sent, rec)
	}
}

func TestClearProfiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	setTwo := func(r *rig) {
		for _, evse := range []string{"EVSE-1", "EVSE-2"} {
			if _, err := r.ctl.SetLimit(ctx, control.LimitParams{EVSEUID: evse, KW: f64(20)}); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("accepted drops the matching records", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		setTwo(r)
		evse := "EVSE-1"
		dropped, err := r.ctl.ClearProfiles(ctx, control.ClearParams{EVSEUID: &evse})
		if err != nil || len(dropped) != 1 || dropped[0].EVSEUID != "EVSE-1" {
			t.Fatalf("ClearProfiles = %+v, %v; want EVSE-1's record", dropped, err)
		}
		if c := r.station.clears[0]; c.EVSEUID == nil || *c.EVSEUID != "EVSE-1" || c.ID != nil || c.Purpose != nil {
			t.Errorf("sent %+v, want EVSE-1 only", c)
		}
		if got := r.core.ChargingProfiles("EVSE-2"); len(got) != 1 {
			t.Errorf("EVSE-2's record went too")
		}
	})

	t.Run("unknown drops them anyway and reports it", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		setTwo(r)
		r.station.answer["clear-profile"] = ocpp.Result{Status: ocpp.CommandUnknownProfile, Raw: "Unknown"}
		dropped, err := r.ctl.ClearProfiles(ctx, control.ClearParams{})
		var refused *control.RefusedError
		if !errors.As(err, &refused) || refused.Status != ocpp.CommandUnknownProfile {
			t.Fatalf("err = %v, want an Unknown refusal", err)
		}
		if len(dropped) != 2 || len(r.core.ChargingProfiles("EVSE-1"))+len(r.core.ChargingProfiles("EVSE-2")) != 0 {
			t.Errorf("dropped %+v; records should be gone", dropped)
		}
	})

	t.Run("a failed call keeps them", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		setTwo(r)
		r.station.fail["clear-profile"] = ocpp.ErrTimeout
		if _, err := r.ctl.ClearProfiles(ctx, control.ClearParams{}); !errors.Is(err, ocpp.ErrTimeout) {
			t.Fatalf("err = %v", err)
		}
		if got := r.core.ChargingProfiles("EVSE-1"); len(got) != 1 {
			t.Errorf("records dropped on a timeout")
		}
	})
}

func TestScheduleRefusedIsAnError(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.answer["composite-schedule"] = ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}

	_, err := r.ctl.Schedule(context.Background(), "", time.Hour, nil)
	var refused *control.RefusedError
	if !errors.As(err, &refused) || refused.Op != control.OpSchedule || refused.EVSEUID != "station" {
		t.Errorf("err = %v, want a refused composite schedule for the station", err)
	}
	if _, err := r.ctl.Schedule(context.Background(), "EVSE-1", 0, nil); !errors.Is(err, ocpp.ErrInvalidProfile) {
		t.Errorf("zero duration: err = %v", err)
	}
}

// A profile that never went out (the station stayed busy for the whole queue
// timeout) is not "unconfirmed": there is no id to look for, and telling the
// operator to check for one would send them after a profile that does not
// exist.
func TestUnsentProfileIsNotUnconfirmed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["set-profile"] = ocpp.ErrNotSent

	_, err := r.ctl.SetLimit(context.Background(), control.LimitParams{EVSEUID: "EVSE-1", KW: f64(20)})
	if !errors.Is(err, ocpp.ErrNotSent) {
		t.Fatalf("err = %v, want ErrNotSent", err)
	}
	var unconfirmed *control.UnconfirmedProfileError
	if errors.As(err, &unconfirmed) {
		t.Errorf("err = %v, want no UnconfirmedProfileError for a profile that was not sent", err)
	}
}

// A profile still queued when the connection closed was not sent either: it
// is a dropped connection, but there is no id to look for.
func TestProfileQueuedWhenTheConnectionClosedIsNotUnconfirmed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["set-profile"] = fmt.Errorf("SetChargingProfile: %w", ocppj.ErrClosedUnsent)

	_, err := r.ctl.SetLimit(context.Background(), control.LimitParams{EVSEUID: "EVSE-1", KW: f64(20)})
	if !errors.Is(err, ocpp.ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	var unconfirmed *control.UnconfirmedProfileError
	if errors.As(err, &unconfirmed) || strings.Contains(err.Error(), "#1") {
		t.Errorf("err = %v, want no claim that the profile was sent", err)
	}
}

// A profile that went out but got no answer may be held by the station
// although cpms records nothing. The error names the id it was sent with, so
// the operator can check for it and clear exactly that one.
func TestUnansweredProfileNamesItsID(t *testing.T) {
	t.Parallel()

	for _, sendErr := range []error{ocpp.ErrTimeout, ocpp.ErrNotConnected} {
		t.Run(sendErr.Error(), func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.station.fail["set-profile"] = sendErr

			_, err := r.ctl.SetLimit(context.Background(), control.LimitParams{EVSEUID: "EVSE-1", KW: f64(20)})
			if !errors.Is(err, sendErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, sendErr)
			}
			var unconfirmed *control.UnconfirmedProfileError
			if !errors.As(err, &unconfirmed) || unconfirmed.ID != 1 {
				t.Fatalf("err = %v, want an UnconfirmedProfileError for profile 1", err)
			}
			for _, want := range []string{"#1", "cpms limit show", "cpms limit clear --id 1"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to mention %q", err, want)
				}
			}
			if got := r.core.ChargingProfiles("EVSE-1"); len(got) != 0 {
				t.Errorf("records = %+v, want none: the station never confirmed it", got)
			}
		})
	}
}
