package v16

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// The Smart Charging profile's three CSMS-to-station messages.

var purposeToWire = map[ocpp.ProfilePurpose]string{
	ocpp.PurposeStationMax: PurposeChargePointMax,
	ocpp.PurposeTxDefault:  PurposeTxDefault,
	ocpp.PurposeTx:         PurposeTx,
}

// SetChargingProfile validates p and sends SetChargingProfile. A profile for
// the whole station goes to connector 0.
func (cp *ChargePoint) SetChargingProfile(ctx context.Context, p ocpp.ChargingProfile) (ocpp.Result, error) {
	if err := p.Validate(); err != nil {
		return ocpp.Result{}, err
	}
	connectorID, err := cp.connectorOrStation(p.EVSEUID)
	if err != nil {
		return ocpp.Result{}, err
	}

	wire := ChargingProfile{
		ChargingProfileID:      p.ID,
		StackLevel:             p.StackLevel,
		ChargingProfilePurpose: purposeToWire[p.Purpose],
		ChargingProfileKind:    string(p.Kind),
		RecurrencyKind:         string(p.RecurrencyKind),
		ValidFrom:              wireTime(p.ValidFrom),
		ValidTo:                wireTime(p.ValidTo),
		ChargingSchedule:       scheduleToWire(p.Schedule),
	}
	if p.TransactionID != "" {
		// core keeps transaction ids as strings; 1.6 numbers them.
		id, err := strconv.Atoi(p.TransactionID)
		if err != nil {
			return ocpp.Result{}, fmt.Errorf("%w: transaction id %q is not an integer", ocpp.ErrInvalidProfile, p.TransactionID)
		}
		wire.TransactionID = &id
	}
	return cp.command(ctx, ActionSetChargingProfile, SetChargingProfileReq{
		ConnectorID:        connectorID,
		CSChargingProfiles: wire,
	})
}

// ClearChargingProfile sends ClearChargingProfile with the fields r sets.
func (cp *ChargePoint) ClearChargingProfile(ctx context.Context, r ocpp.ClearProfileRequest) (ocpp.Result, error) {
	req := ClearChargingProfileReq{ID: r.ID, StackLevel: r.StackLevel}
	if r.EVSEUID != nil {
		connectorID, err := cp.connectorOrStation(*r.EVSEUID)
		if err != nil {
			return ocpp.Result{}, err
		}
		req.ConnectorID = &connectorID
	}
	if r.Purpose != nil {
		purpose, ok := purposeToWire[*r.Purpose]
		if !ok {
			return ocpp.Result{}, fmt.Errorf("%w: unknown purpose %q", ocpp.ErrInvalidProfile, *r.Purpose)
		}
		req.ChargingProfilePurpose = purpose
	}
	return cp.command(ctx, ActionClearChargingProfile, req)
}

// GetCompositeSchedule sends GetCompositeSchedule for the EVSE's connector,
// or connector 0 for "". d goes on the wire as whole seconds, so a fraction
// is refused rather than cut off.
func (cp *ChargePoint) GetCompositeSchedule(ctx context.Context, evseUID string, d time.Duration, unit *ocpp.RateUnit) (ocpp.CompositeSchedule, error) {
	if d <= 0 || d%time.Second != 0 {
		return ocpp.CompositeSchedule{}, fmt.Errorf("ocpp 1.6: composite schedule duration %s is not a positive whole number of seconds", d)
	}
	connectorID, err := cp.connectorOrStation(evseUID)
	if err != nil {
		return ocpp.CompositeSchedule{}, err
	}
	req := GetCompositeScheduleReq{ConnectorID: connectorID, Duration: int(d / time.Second)}
	if unit != nil {
		if *unit != ocpp.UnitW && *unit != ocpp.UnitA {
			return ocpp.CompositeSchedule{}, fmt.Errorf("ocpp 1.6: rate unit %q is neither W nor A", *unit)
		}
		req.ChargingRateUnit = string(*unit)
	}

	raw, err := cp.c.Call(ctx, ActionGetCompositeSchedule, req)
	if err != nil {
		return ocpp.CompositeSchedule{}, err
	}
	var conf GetCompositeScheduleConf
	if err := json.Unmarshal(raw, &conf); err != nil {
		return ocpp.CompositeSchedule{}, fmt.Errorf("%s response: %w", ActionGetCompositeSchedule, err)
	}

	out := ocpp.CompositeSchedule{
		Status:  MapCommandStatus(ActionGetCompositeSchedule, conf.Status),
		Raw:     conf.Status,
		EVSEUID: evseUID,
	}
	if conf.ScheduleStart != "" {
		if out.Start, err = time.Parse(time.RFC3339, conf.ScheduleStart); err != nil {
			return out, fmt.Errorf("%s response: scheduleStart: %w", ActionGetCompositeSchedule, err)
		}
	}
	if conf.ChargingSchedule != nil {
		s, err := scheduleFromWire(*conf.ChargingSchedule)
		if err != nil {
			return out, fmt.Errorf("%s response: %w", ActionGetCompositeSchedule, err)
		}
		out.Schedule = &s
	}
	return out, nil
}

// connectorOrStation maps an EVSE uid to its connector, and "" to connector
// 0, the station as a whole.
func (cp *ChargePoint) connectorOrStation(evseUID string) (int, error) {
	if evseUID == "" {
		return 0, nil
	}
	return cp.connectorID(evseUID)
}

// wireTime renders an optional time as 1.6 sends times: UTC, whole seconds.
func wireTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func scheduleToWire(s ocpp.ChargingSchedule) ChargingSchedule {
	wire := ChargingSchedule{
		StartSchedule:    wireTime(s.StartSchedule),
		ChargingRateUnit: string(s.Unit),
		MinChargingRate:  s.MinRate,
	}
	if s.Duration != nil {
		secs := int(*s.Duration / time.Second)
		wire.Duration = &secs
	}
	for _, p := range s.Periods {
		wire.ChargingSchedulePeriod = append(wire.ChargingSchedulePeriod, ChargingSchedulePeriod{
			StartPeriod:  int(p.Start / time.Second),
			Limit:        p.Limit,
			NumberPhases: p.Phases,
		})
	}
	return wire
}

func scheduleFromWire(w ChargingSchedule) (ocpp.ChargingSchedule, error) {
	s := ocpp.ChargingSchedule{Unit: ocpp.RateUnit(w.ChargingRateUnit), MinRate: w.MinChargingRate}
	if w.Duration != nil {
		d := time.Duration(*w.Duration) * time.Second
		s.Duration = &d
	}
	if w.StartSchedule != "" {
		t, err := time.Parse(time.RFC3339, w.StartSchedule)
		if err != nil {
			return s, fmt.Errorf("startSchedule: %w", err)
		}
		s.StartSchedule = &t
	}
	for _, p := range w.ChargingSchedulePeriod {
		s.Periods = append(s.Periods, ocpp.SchedulePeriod{
			Start:  time.Duration(p.StartPeriod) * time.Second,
			Limit:  p.Limit,
			Phases: p.NumberPhases,
		})
	}
	return s, nil
}
