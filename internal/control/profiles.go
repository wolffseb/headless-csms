package control

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// LimitParams describe a single-limit charging profile: one period, from now.
type LimitParams struct {
	// EVSEUID is the EVSE to limit; "" means the whole station, which only
	// PurposeStationMax may use.
	EVSEUID string
	// Exactly one of KW and Amps is set. KW is sent as W.
	KW   *float64
	Amps *float64
	// Purpose defaults to PurposeTxDefault.
	Purpose    ocpp.ProfilePurpose
	StackLevel int
	// For bounds the profile: it sets the schedule's duration and validTo.
	// Zero means open-ended.
	For time.Duration
	// TransactionID is the session a Tx profile is for, and must be active
	// on the EVSE. Empty picks the one active transaction there.
	TransactionID string
	// Force sends a Tx profile without a transactionId when no session is
	// running, instead of refusing it locally. It exists to see what the
	// station answers.
	Force bool
	// Source is who asked: "cli", "tui" or "ocpi".
	Source string
}

// ClearParams select the charging profiles to clear. Each nil field means
// "any", as in ocpp.ClearProfileRequest.
type ClearParams struct {
	ID *int
	// EVSEUID set to "" means the station's own profiles.
	EVSEUID    *string
	Purpose    *ocpp.ProfilePurpose
	StackLevel *int
}

// SetLimit installs a one-period Absolute profile that caps an EVSE, or the
// station, at a power or a current from now on.
func (s *Service) SetLimit(ctx context.Context, p LimitParams) (core.ChargingProfile, error) {
	if (p.KW == nil) == (p.Amps == nil) {
		return core.ChargingProfile{}, fmt.Errorf("%w: give exactly one of a power and a current", ocpp.ErrInvalidProfile)
	}
	if p.For < 0 {
		return core.ChargingProfile{}, fmt.Errorf("%w: duration %s is negative", ocpp.ErrInvalidProfile, p.For)
	}
	if p.Purpose == "" {
		p.Purpose = ocpp.PurposeTxDefault
	}
	if p.TransactionID != "" && p.Purpose == ocpp.PurposeTx {
		// Named on the command line, so checked like `cpms stop --tx`: a
		// session that is not running here is a typo, not a choice. Any
		// other purpose with a transaction id fails Validate.
		if err := s.checkEVSE(p.EVSEUID); err != nil {
			return core.ChargingProfile{}, err
		}
		if _, err := s.transactionOn(p.EVSEUID, p.TransactionID); err != nil {
			return core.ChargingProfile{}, err
		}
	}

	unit, limit := ocpp.UnitA, 0.0
	if p.KW != nil {
		// Rounded to the schema's 0.1, so 7.4 kW goes out as 7400, not
		// 7400.000000000001.
		unit, limit = ocpp.UnitW, math.Round(*p.KW*10000)/10
	} else {
		limit = *p.Amps
	}

	// Whole seconds: the wire carries no fractions.
	now := s.now().UTC().Truncate(time.Second)
	profile := ocpp.ChargingProfile{
		StackLevel:    p.StackLevel,
		Purpose:       p.Purpose,
		Kind:          ocpp.KindAbsolute,
		TransactionID: p.TransactionID,
		EVSEUID:       p.EVSEUID,
		Schedule: ocpp.ChargingSchedule{
			StartSchedule: &now,
			Unit:          unit,
			Periods:       []ocpp.SchedulePeriod{{Start: 0, Limit: limit}},
		},
	}
	if p.For > 0 {
		d := p.For.Round(time.Second)
		until := now.Add(d)
		profile.Schedule.Duration = &d
		profile.ValidTo = &until
	}
	return s.setProfile(ctx, profile, p.Source, p.Force)
}

// SetProfile installs a full charging profile on an EVSE ("" for the
// station). The caller's id is ignored: one is allocated, so a profile can
// never replace one cpms did not mean it to. A Tx profile without a
// transaction id is for the one session running on the EVSE.
func (s *Service) SetProfile(ctx context.Context, evseUID string, p ocpp.ChargingProfile, source string) (core.ChargingProfile, error) {
	p.EVSEUID = evseUID
	return s.setProfile(ctx, p, source, false)
}

// setProfile checks, allocates an id, sends, and records the profile once
// the station accepts it. Nothing is recorded otherwise: the record is what
// the station holds, as far as cpms knows.
func (s *Service) setProfile(ctx context.Context, p ocpp.ChargingProfile, source string, force bool) (core.ChargingProfile, error) {
	if err := s.checkEVSEOrStation(p.EVSEUID); err != nil {
		return core.ChargingProfile{}, err
	}
	if p.Purpose == ocpp.PurposeTx && p.TransactionID == "" && p.EVSEUID != "" {
		txID, err := s.activeTransaction(p.EVSEUID)
		switch {
		case err == nil:
			p.TransactionID = txID
		case !force:
			return core.ChargingProfile{}, err
		}
	}
	p.ID = 0
	if err := p.Validate(); err != nil {
		return core.ChargingProfile{}, err
	}
	cp, err := s.chargePoint()
	if err != nil {
		return core.ChargingProfile{}, err
	}
	id, err := s.core.NextChargingProfileID()
	if err != nil {
		return core.ChargingProfile{}, fmt.Errorf("recording the charging profile id: %w", err)
	}
	p.ID = id

	res, err := cp.SetChargingProfile(ctx, p)
	// ErrNotSent is neither: the profile never went out, so there is no id to
	// look for.
	if errors.Is(err, ocpp.ErrTimeout) || errors.Is(err, ocpp.ErrNotConnected) {
		// It may have arrived: say which id to look for, since nothing is
		// recorded for it.
		return core.ChargingProfile{}, &UnconfirmedProfileError{ID: p.ID, Target: target(p.EVSEUID), Err: err}
	}
	if err != nil {
		return core.ChargingProfile{}, err
	}
	if res.Status != ocpp.CommandAccepted {
		return core.ChargingProfile{}, refused(OpSetProfile, target(p.EVSEUID), res)
	}
	return s.core.RecordChargingProfile(toRecord(cp.ID(), p, source)), nil
}

// ClearProfiles asks the station to remove the profiles c selects. On
// Accepted, the matching records go. On "Unknown" the station holds none of
// them, so the matching records go too, since they no longer describe
// anything, and the answer is reported as a refusal, as Cancel does. It
// returns the records it dropped.
func (s *Service) ClearProfiles(ctx context.Context, c ClearParams) ([]core.ChargingProfile, error) {
	if c.EVSEUID != nil {
		if err := s.checkEVSEOrStation(*c.EVSEUID); err != nil {
			return nil, err
		}
	}
	cp, err := s.chargePoint()
	if err != nil {
		return nil, err
	}
	res, err := cp.ClearChargingProfile(ctx, ocpp.ClearProfileRequest{
		ID: c.ID, EVSEUID: c.EVSEUID, Purpose: c.Purpose, StackLevel: c.StackLevel,
	})
	if err != nil {
		return nil, err
	}

	filter := core.ProfileFilter{ID: c.ID, EVSEUID: c.EVSEUID, StackLevel: c.StackLevel}
	if c.Purpose != nil {
		purpose := string(*c.Purpose)
		filter.Purpose = &purpose
	}
	switch res.Status {
	case ocpp.CommandAccepted:
		return s.core.ForgetChargingProfiles(cp.ID(), filter, core.ProfileCleared), nil
	case ocpp.CommandUnknownProfile:
		dropped := s.core.ForgetChargingProfiles(cp.ID(), filter, core.ProfileUnknownAtStation)
		if len(dropped) > 0 {
			s.log.Warn("station holds none of the charging profiles to clear; dropping our records of them anyway",
				"profiles", len(dropped), "status", res.Raw)
		}
		return dropped, refused(OpClearProfiles, clearTarget(c), res)
	default:
		return nil, refused(OpClearProfiles, clearTarget(c), res)
	}
}

// Schedule asks the station for the limits it will apply to an EVSE ("" for
// the station) over the next d: all profiles combined, as only the station
// can work out. unit nil leaves the unit to the station.
func (s *Service) Schedule(ctx context.Context, evseUID string, d time.Duration, unit *ocpp.RateUnit) (ocpp.CompositeSchedule, error) {
	if err := s.checkEVSEOrStation(evseUID); err != nil {
		return ocpp.CompositeSchedule{}, err
	}
	if d <= 0 || d%time.Second != 0 {
		return ocpp.CompositeSchedule{}, fmt.Errorf("%w: duration %s is not a positive number of seconds", ocpp.ErrInvalidProfile, d)
	}
	cp, err := s.chargePoint()
	if err != nil {
		return ocpp.CompositeSchedule{}, err
	}
	sched, err := cp.GetCompositeSchedule(ctx, evseUID, d, unit)
	if err != nil {
		return ocpp.CompositeSchedule{}, err
	}
	if sched.Status != ocpp.CommandAccepted {
		return ocpp.CompositeSchedule{}, refused(OpSchedule, target(evseUID), ocpp.Result{Status: sched.Status, Raw: sched.Raw})
	}
	return sched, nil
}

// activeTransaction is the one session running on an EVSE.
func (s *Service) activeTransaction(evseUID string) (string, error) {
	txID, err := s.transactionOn(evseUID, "")
	if errors.Is(err, ErrNoActiveTransaction) {
		return "", fmt.Errorf("%s: %w for a Tx profile (use --force to send it anyway)", evseUID, ErrNoActiveTransaction)
	}
	return txID, err
}

// checkEVSEOrStation is checkEVSE that also accepts "" for the station.
func (s *Service) checkEVSEOrStation(evseUID string) error {
	if evseUID == "" {
		return nil
	}
	return s.checkEVSE(evseUID)
}

// target names an EVSE, or the station, in an error message.
func target(evseUID string) string {
	if evseUID == "" {
		return "station"
	}
	return evseUID
}

func clearTarget(c ClearParams) string {
	if c.EVSEUID == nil {
		return "station"
	}
	return target(*c.EVSEUID)
}

// toRecord turns a profile the station accepted into core's record of it.
func toRecord(cpID string, p ocpp.ChargingProfile, source string) core.ChargingProfile {
	periods := make([]core.ChargingPeriod, len(p.Schedule.Periods))
	for i, period := range p.Schedule.Periods {
		periods[i] = core.ChargingPeriod{Start: period.Start, Limit: period.Limit, Phases: period.Phases}
	}
	return core.ChargingProfile{
		ID: p.ID, ChargePointID: cpID, EVSEUID: p.EVSEUID,
		Purpose: string(p.Purpose), StackLevel: p.StackLevel, Unit: string(p.Schedule.Unit),
		Periods: periods, ValidFrom: p.ValidFrom, ValidTo: p.ValidTo,
		TransactionID: p.TransactionID, Source: source,
	}
}
