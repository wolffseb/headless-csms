package v16_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

func ptr[T any](v T) *T { return &v }

// txDefaultProfile is an Absolute TxDefault in W with two periods: 20 kW,
// then 11 kW after an hour.
func txDefaultProfile(start time.Time) ocpp.ChargingProfile {
	return ocpp.ChargingProfile{
		ID: 7, StackLevel: 0, Purpose: ocpp.PurposeTxDefault, Kind: ocpp.KindAbsolute, EVSEUID: "EVSE-2",
		Schedule: ocpp.ChargingSchedule{
			StartSchedule: &start,
			Unit:          ocpp.UnitW,
			Periods: []ocpp.SchedulePeriod{
				{Start: 0, Limit: 20000},
				{Start: time.Hour, Limit: 11000},
			},
		},
	}
}

// TestProfileValidationRefusesBeforeSending covers each rule of Validate,
// and the adapter's own transaction id check, through SetChargingProfile:
// nothing may reach the station.
func TestProfileValidationRefusesBeforeSending(t *testing.T) {
	t.Parallel()

	base := func() ocpp.ChargingProfile { return txDefaultProfile(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)) }
	tx := func() ocpp.ChargingProfile {
		p := base()
		p.Purpose, p.TransactionID = ocpp.PurposeTx, "1001"
		return p
	}
	stationMax := func() ocpp.ChargingProfile {
		p := base()
		p.Purpose, p.EVSEUID = ocpp.PurposeStationMax, ""
		return p
	}

	tests := map[string]func() ocpp.ChargingProfile{
		"negative id":          func() ocpp.ChargingProfile { p := base(); p.ID = -1; return p },
		"negative stack level": func() ocpp.ChargingProfile { p := base(); p.StackLevel = -1; return p },
		"unknown purpose":      func() ocpp.ChargingProfile { p := base(); p.Purpose = "Sometimes"; return p },
		"unknown kind":         func() ocpp.ChargingProfile { p := base(); p.Kind = "Eventually"; return p },
		"unknown unit":         func() ocpp.ChargingProfile { p := base(); p.Schedule.Unit = "kW"; return p },
		"no periods":           func() ocpp.ChargingProfile { p := base(); p.Schedule.Periods = nil; return p },
		"first period not at 0": func() ocpp.ChargingProfile {
			p := base()
			p.Schedule.Periods[0].Start = time.Second
			return p
		},
		"periods not increasing": func() ocpp.ChargingProfile {
			p := base()
			p.Schedule.Periods[1].Start = 0
			return p
		},
		"period start not whole seconds": func() ocpp.ChargingProfile {
			p := base()
			p.Schedule.Periods[1].Start = 1500 * time.Millisecond
			return p
		},
		"negative limit": func() ocpp.ChargingProfile { p := base(); p.Schedule.Periods[0].Limit = -1; return p },
		"two decimals":   func() ocpp.ChargingProfile { p := base(); p.Schedule.Periods[0].Limit = 12.34; return p },
		"min rate with two decimals": func() ocpp.ChargingProfile {
			p := base()
			p.Schedule.MinRate = ptr(6.25)
			return p
		},
		"four phases": func() ocpp.ChargingProfile { p := base(); p.Schedule.Periods[0].Phases = ptr(4); return p },
		"duration not whole seconds": func() ocpp.ChargingProfile {
			p := base()
			p.Schedule.Duration = ptr(90 * time.Millisecond)
			return p
		},
		"tx without transaction": func() ocpp.ChargingProfile { p := tx(); p.TransactionID = ""; return p },
		"transaction on txdefault": func() ocpp.ChargingProfile {
			p := base()
			p.TransactionID = "1001"
			return p
		},
		"tx on the whole station":        func() ocpp.ChargingProfile { p := tx(); p.EVSEUID = ""; return p },
		"txdefault on the whole station": func() ocpp.ChargingProfile { p := base(); p.EVSEUID = ""; return p },
		"station max on an evse":         func() ocpp.ChargingProfile { p := stationMax(); p.EVSEUID = "EVSE-1"; return p },
		"recurring without recurrency": func() ocpp.ChargingProfile {
			p := base()
			p.Kind = ocpp.KindRecurring
			return p
		},
		"recurrency on absolute": func() ocpp.ChargingProfile {
			p := base()
			p.RecurrencyKind = ocpp.RecurrencyDaily
			return p
		},
		"recurring without start": func() ocpp.ChargingProfile {
			p := base()
			p.Kind, p.RecurrencyKind, p.Schedule.StartSchedule = ocpp.KindRecurring, ocpp.RecurrencyDaily, nil
			return p
		},
		"valid to before valid from": func() ocpp.ChargingProfile {
			p := base()
			from := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			to := from.Add(-time.Hour)
			p.ValidFrom, p.ValidTo = &from, &to
			return p
		},
		"transaction id not an integer": func() ocpp.ChargingProfile { p := tx(); p.TransactionID = "tx-abc"; return p },
	}

	// The fixtures themselves are fine; otherwise every case above passes
	// for the wrong reason.
	for name, ok := range map[string]func() ocpp.ChargingProfile{"txdefault": base, "tx": tx, "station max": stationMax} {
		cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
		if _, err := cp.SetChargingProfile(context.Background(), ok()); err != nil {
			t.Fatalf("valid %s profile refused: %v", name, err)
		}
		if n := len(fake.sent()); n != 1 {
			t.Fatalf("valid %s profile: %d calls sent, want 1", name, n)
		}
	}

	for name, profile := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
			_, err := cp.SetChargingProfile(context.Background(), profile())
			if !errors.Is(err, ocpp.ErrInvalidProfile) {
				t.Errorf("err = %v, want ErrInvalidProfile", err)
			}
			if n := len(fake.sent()); n != 0 {
				t.Errorf("%d calls sent, want 0", n)
			}
		})
	}
}

func TestChargingCommandsRefuseBadInputBeforeSending(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		send func(context.Context, *v16.ChargePoint) error
		want error
	}{
		"set on an unknown EVSE": {func(ctx context.Context, cp *v16.ChargePoint) error {
			p := txDefaultProfile(time.Now())
			p.EVSEUID = "NOPE"
			_, err := cp.SetChargingProfile(ctx, p)
			return err
		}, ocpp.ErrUnknownEVSE},
		"clear on an unknown EVSE": {func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.ClearChargingProfile(ctx, ocpp.ClearProfileRequest{EVSEUID: ptr("NOPE")})
			return err
		}, ocpp.ErrUnknownEVSE},
		"clear with an unknown purpose": {func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.ClearChargingProfile(ctx, ocpp.ClearProfileRequest{Purpose: ptr(ocpp.ProfilePurpose("Sometimes"))})
			return err
		}, ocpp.ErrInvalidProfile},
		"composite for an unknown EVSE": {func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.GetCompositeSchedule(ctx, "NOPE", time.Minute, nil)
			return err
		}, ocpp.ErrUnknownEVSE},
		"composite for a fraction of a second": {func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.GetCompositeSchedule(ctx, "EVSE-1", 1500*time.Millisecond, nil)
			return err
		}, nil},
		"composite for no time": {func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.GetCompositeSchedule(ctx, "EVSE-1", 0, nil)
			return err
		}, nil},
		"composite in kW": {func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.GetCompositeSchedule(ctx, "EVSE-1", time.Minute, ptr(ocpp.RateUnit("kW")))
			return err
		}, nil},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
			err := tt.send(context.Background(), cp)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if n := len(fake.sent()); n != 0 {
				t.Errorf("%d calls sent, want 0", n)
			}
		})
	}
}

// TestClearChargingProfileAddressing pins how ClearProfileRequest's nil and
// "" EVSE map onto connectorId.
func TestClearChargingProfileAddressing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		req  ocpp.ClearProfileRequest
		want string
	}{
		{ocpp.ClearProfileRequest{}, `{}`},
		{ocpp.ClearProfileRequest{EVSEUID: ptr("")}, `{"connectorId":0}`},
		{ocpp.ClearProfileRequest{EVSEUID: ptr("EVSE-2"), Purpose: ptr(ocpp.PurposeTxDefault), StackLevel: ptr(0)},
			`{"connectorId":2,"chargingProfilePurpose":"TxDefaultProfile","stackLevel":0}`},
		{ocpp.ClearProfileRequest{Purpose: ptr(ocpp.PurposeStationMax)}, `{"chargingProfilePurpose":"ChargePointMaxProfile"}`},
	}
	for _, tt := range tests {
		cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
		if _, err := cp.ClearChargingProfile(context.Background(), tt.req); err != nil {
			t.Fatalf("%+v: %v", tt.req, err)
		}
		if got := string(fake.sent()[0].payload); got != tt.want {
			t.Errorf("%+v sent %s, want %s", tt.req, got, tt.want)
		}
	}
}

// TestChargingStatusMapping covers every status the 1.6 spec defines for
// the three Smart Charging .confs, plus values it does not.
func TestChargingStatusMapping(t *testing.T) {
	t.Parallel()

	send := map[string]func(context.Context, *v16.ChargePoint) (ocpp.Result, error){
		v16.ActionSetChargingProfile: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.SetChargingProfile(ctx, txDefaultProfile(time.Now()))
		},
		v16.ActionClearChargingProfile: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.ClearChargingProfile(ctx, ocpp.ClearProfileRequest{ID: ptr(1)})
		},
		v16.ActionGetCompositeSchedule: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			s, err := cp.GetCompositeSchedule(ctx, "EVSE-1", time.Minute, nil)
			return ocpp.Result{Status: s.Status, Raw: s.Raw}, err
		},
	}

	tests := []struct {
		action string
		raw    string
		want   ocpp.CommandStatus
	}{
		// ChargingProfileStatus
		{v16.ActionSetChargingProfile, "Accepted", ocpp.CommandAccepted},
		{v16.ActionSetChargingProfile, "Rejected", ocpp.CommandRejected},
		{v16.ActionSetChargingProfile, "NotSupported", ocpp.CommandNotSupported},
		{v16.ActionSetChargingProfile, "Unknown", ocpp.CommandUnknown},
		// ClearChargingProfileStatus: its Unknown is a defined answer.
		{v16.ActionClearChargingProfile, "Accepted", ocpp.CommandAccepted},
		{v16.ActionClearChargingProfile, "Unknown", ocpp.CommandUnknownProfile},
		{v16.ActionClearChargingProfile, "Rejected", ocpp.CommandUnknown},
		// GetCompositeScheduleStatus
		{v16.ActionGetCompositeSchedule, "Accepted", ocpp.CommandAccepted},
		{v16.ActionGetCompositeSchedule, "Rejected", ocpp.CommandRejected},
		{v16.ActionGetCompositeSchedule, "NotSupported", ocpp.CommandUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.action+"/"+tt.raw, func(t *testing.T) {
			t.Parallel()

			resp, _ := json.Marshal(map[string]string{"status": tt.raw})
			cp, _ := newFakeChargePoint(string(resp))

			got, err := send[tt.action](context.Background(), cp)
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if got.Status != tt.want {
				t.Errorf("Status = %q, want %q", got.Status, tt.want)
			}
			if got.Raw != tt.raw {
				t.Errorf("Raw = %q, want %q preserved", got.Raw, tt.raw)
			}
		})
	}
}

// TestChargingResponses decodes a recorded .conf of each Smart Charging
// message, from testdata/golden/conf, through the adapter.
func TestChargingResponses(t *testing.T) {
	t.Parallel()

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata", "golden", "conf", name+".json"))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(b)
	}
	ctx := context.Background()

	cp, _ := newFakeChargePoint(read(v16.ActionSetChargingProfile))
	if res, err := cp.SetChargingProfile(ctx, txDefaultProfile(time.Now())); err != nil || res.Status != ocpp.CommandAccepted {
		t.Errorf("SetChargingProfile: %+v, %v", res, err)
	}

	cp, _ = newFakeChargePoint(read(v16.ActionClearChargingProfile))
	if res, err := cp.ClearChargingProfile(ctx, ocpp.ClearProfileRequest{ID: ptr(7)}); err != nil ||
		res.Status != ocpp.CommandUnknownProfile || res.Raw != "Unknown" {
		t.Errorf("ClearChargingProfile: %+v, %v", res, err)
	}

	cp, _ = newFakeChargePoint(read(v16.ActionGetCompositeSchedule))
	got, err := cp.GetCompositeSchedule(ctx, "EVSE-2", time.Hour, nil)
	if err != nil {
		t.Fatalf("GetCompositeSchedule: %v", err)
	}
	start := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	want := ocpp.CompositeSchedule{
		Status: ocpp.CommandAccepted, Raw: "Accepted", EVSEUID: "EVSE-2", Start: start,
		Schedule: &ocpp.ChargingSchedule{
			Duration: &hour,
			Unit:     ocpp.UnitW,
			Periods: []ocpp.SchedulePeriod{
				{Start: 0, Limit: 20000},
				{Start: 30 * time.Minute, Limit: 16.5, Phases: ptr(3)},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetCompositeSchedule:\n got %+v %+v\nwant %+v %+v", got, got.Schedule, want, want.Schedule)
	}

	cp, _ = newFakeChargePoint(`{"status":"Rejected"}`)
	got, err = cp.GetCompositeSchedule(ctx, "", time.Hour, nil)
	if err != nil || got.Status != ocpp.CommandRejected || got.Schedule != nil || !got.Start.IsZero() {
		t.Errorf("rejected composite: %+v, %v", got, err)
	}
}
