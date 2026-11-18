package socket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// DefaultScheduleWindow is how far ahead limit_show looks when not told.
const DefaultScheduleWindow = time.Hour

// The purpose names the CLI and the JSON use, kebab-case like the flags.
var purposeNames = map[string]ocpp.ProfilePurpose{
	"tx-default":  ocpp.PurposeTxDefault,
	"station-max": ocpp.PurposeStationMax,
	"tx":          ocpp.PurposeTx,
}

// ParsePurpose reads a purpose name: "tx-default", "station-max" or "tx".
func ParsePurpose(name string) (ocpp.ProfilePurpose, error) {
	if p, ok := purposeNames[name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("unknown purpose %q: want tx-default, station-max or tx", name)
}

// PurposeName is the name of a purpose as core records it ("TxDefault").
func PurposeName(p string) string {
	for name, purpose := range purposeNames {
		if string(purpose) == p {
			return name
		}
	}
	return p
}

var kindNames = map[string]ocpp.ProfileKind{
	"": ocpp.KindAbsolute, "absolute": ocpp.KindAbsolute,
	"relative": ocpp.KindRelative, "recurring": ocpp.KindRecurring,
}

var recurrencyNames = map[string]ocpp.RecurrencyKind{
	"": "", "daily": ocpp.RecurrencyDaily, "weekly": ocpp.RecurrencyWeekly,
}

// ToOCPP converts a profile in the neutral JSON shape to the protocol's,
// for an EVSE ("" for the station). It checks the names; ocpp's Validate
// checks the rest. The id is left at 0 for control to allocate.
func (p Profile) ToOCPP(evseUID string) (ocpp.ChargingProfile, error) {
	purpose, err := ParsePurpose(p.Purpose)
	if err != nil {
		return ocpp.ChargingProfile{}, err
	}
	kind, ok := kindNames[p.Kind]
	if !ok {
		return ocpp.ChargingProfile{}, fmt.Errorf("unknown kind %q: want absolute, relative or recurring", p.Kind)
	}
	recurrency, ok := recurrencyNames[p.Recurrency]
	if !ok {
		return ocpp.ChargingProfile{}, fmt.Errorf("unknown recurrency %q: want daily or weekly", p.Recurrency)
	}
	sched := ocpp.ChargingSchedule{
		StartSchedule: p.Schedule.Start,
		Unit:          ocpp.RateUnit(p.Schedule.Unit),
		MinRate:       p.Schedule.MinRate,
	}
	if s := p.Schedule.DurationSeconds; s != nil {
		d := time.Duration(*s) * time.Second
		sched.Duration = &d
	}
	for _, period := range p.Schedule.Periods {
		sched.Periods = append(sched.Periods, ocpp.SchedulePeriod{
			Start: time.Duration(period.StartSeconds) * time.Second, Limit: period.Limit, Phases: period.Phases,
		})
	}
	return ocpp.ChargingProfile{
		StackLevel: p.StackLevel, Purpose: purpose, Kind: kind, RecurrencyKind: recurrency,
		ValidFrom: p.ValidFrom, ValidTo: p.ValidTo, TransactionID: p.TransactionID,
		Schedule: sched, EVSEUID: evseUID,
	}, nil
}

// profileStatus renders a record for status and results.
func profileStatus(p core.ChargingProfile) ChargingProfileStatus {
	periods := make([]ProfilePeriod, len(p.Periods))
	for i, period := range p.Periods {
		periods[i] = ProfilePeriod{StartSeconds: int(period.Start / time.Second), Limit: period.Limit, Phases: period.Phases}
	}
	return ChargingProfileStatus{
		ID: p.ID, Purpose: PurposeName(p.Purpose), StackLevel: p.StackLevel, Unit: p.Unit,
		Periods: periods, ValidFrom: utcPtr(p.ValidFrom), ValidTo: utcPtr(p.ValidTo),
		TransactionID: p.TransactionID, SetAt: p.SetAt.UTC(), Source: p.Source,
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return utcOrNil(*t)
}

// target is the EVSE a limit or profile request addresses: "" for the
// station.
func (r Request) target() string {
	if r.Station {
		return ""
	}
	return r.EVSE
}

// dispatchProfile serves the limit and profile operations.
func (s *Server) dispatchProfile(ctx context.Context, req Request) Response {
	ctl := s.opts.Control
	switch req.Op {
	case OpLimitSet:
		if req.Limit == nil {
			return fail(&Error{Code: CodeUsage, Message: "limit_set needs a limit"})
		}
		l := req.Limit
		params := control.LimitParams{
			EVSEUID: req.target(), KW: l.KW, Amps: l.Amps, StackLevel: l.StackLevel, Force: l.Force,
			TransactionID: l.TransactionID, Source: "cli",
		}
		if l.Purpose != "" {
			purpose, err := ParsePurpose(l.Purpose)
			if err != nil {
				return fail(&Error{Code: CodeUsage, Message: err.Error()})
			}
			params.Purpose = purpose
		}
		if l.For != "" {
			d, err := time.ParseDuration(l.For)
			if err != nil || d < 0 {
				return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf("invalid duration %q: want a positive duration such as 1h", l.For)})
			}
			params.For = d
		}
		rec, err := ctl.SetLimit(ctx, params)
		if err != nil {
			return fail(classifyProfile(err))
		}
		return ok(ProfileResult{EVSE: rec.EVSEUID, ChargingProfileStatus: profileStatus(rec)})

	case OpProfileSet:
		if req.Profile == nil {
			return fail(&Error{Code: CodeUsage, Message: "profile_set needs a profile"})
		}
		p, err := req.Profile.ToOCPP(req.target())
		if err != nil {
			return fail(&Error{Code: CodeUsage, Message: err.Error()})
		}
		rec, err := ctl.SetProfile(ctx, req.target(), p, "cli")
		if err != nil {
			return fail(classifyProfile(err))
		}
		return ok(ProfileResult{EVSE: rec.EVSEUID, ChargingProfileStatus: profileStatus(rec)})

	case OpLimitClear:
		c := ClearRequest{}
		if req.Clear != nil {
			c = *req.Clear
		}
		params := control.ClearParams{ID: c.ID}
		if c.Purpose != "" {
			purpose, err := ParsePurpose(c.Purpose)
			if err != nil {
				return fail(&Error{Code: CodeUsage, Message: err.Error()})
			}
			params.Purpose = &purpose
		}
		if c.ByTarget {
			t := req.target()
			params.EVSEUID = &t
		}
		dropped, err := ctl.ClearProfiles(ctx, params)
		if err != nil {
			e := classifyProfile(err)
			if n := len(dropped); n > 0 {
				e.Message += fmt.Sprintf("; cpms dropped its %d record(s) of them", n)
			}
			return fail(e)
		}
		ids := make([]int, 0, len(dropped))
		for _, p := range dropped {
			ids = append(ids, p.ID)
		}
		return ok(ClearResult{Dropped: ids})

	case OpLimitShow:
		window, unit := DefaultScheduleWindow, (*ocpp.RateUnit)(nil)
		if sr := req.Schedule; sr != nil {
			if sr.Duration != "" {
				d, err := time.ParseDuration(sr.Duration)
				if err != nil || d <= 0 {
					return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf("invalid duration %q: want a positive duration such as 1h", sr.Duration)})
				}
				window = d
			}
			if sr.Unit != "" {
				u := ocpp.RateUnit(sr.Unit)
				if u != ocpp.UnitW && u != ocpp.UnitA {
					return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf("unknown unit %q: want W or A", sr.Unit)})
				}
				unit = &u
			}
		}
		sched, err := ctl.Schedule(ctx, req.target(), window, unit)
		if err != nil {
			return fail(classifyProfile(err))
		}
		out := ScheduleResult{EVSE: req.target(), Start: sched.Start.UTC(), Periods: []ProfilePeriod{}}
		if sc := sched.Schedule; sc != nil {
			out.Unit = string(sc.Unit)
			if sc.Duration != nil {
				secs := int(*sc.Duration / time.Second)
				out.DurationSeconds = &secs
			}
			for _, period := range sc.Periods {
				out.Periods = append(out.Periods, ProfilePeriod{
					StartSeconds: int(period.Start / time.Second), Limit: period.Limit, Phases: period.Phases,
				})
			}
		}
		return ok(out)
	}
	return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf("unknown operation %q", req.Op)})
}

// classifyProfile is classify for the smart-charging operations, where "the
// station cannot do that" is its own code rather than unlock_failed, and a
// profile that cannot be sent is a usage error.
func classifyProfile(err error) *Error {
	e := classify(err)
	switch {
	case e.Code == CodeUnlockFailed:
		e.Code = CodeNotSupported
	case errors.Is(err, ocpp.ErrInvalidProfile):
		e.Code = CodeUsage
	}
	return e
}
