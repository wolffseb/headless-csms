package simulator

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// The limits the simulator advertises in its Smart Charging configuration
// keys, and enforces: a profile beyond any of them is Rejected.
const (
	maxStackLevel        = 8
	maxSchedulePeriods   = 24
	maxProfilesInstalled = 10
)

// The Smart Charging model is deliberately simple, and documented here so
// that a test failing against it can be told from a spec question:
//
//   - Profiles are kept per connector, 0 being the station, in memory: they
//     survive a reconnect and a Reset (errata 3.37), but not a restart of
//     the simulator process.
//   - A new profile replaces one with the same id anywhere on the station,
//     and one with the same purpose and stack level on the same connector.
//   - A TxProfile needs a running transaction on its connector (with the
//     same id, if it names one); it ends with that transaction.
//   - The limit at a moment is: within each purpose, the active profile with
//     the highest stack level; TxProfile beats TxDefaultProfile; the result
//     is the lower of that and the connector's share of
//     ChargePointMaxProfile. Profiles on connector 0 apply to every
//     connector.
//   - ChargePointMaxProfile caps the station's total (§3.13.3). The
//     simulator splits it evenly between the connectors with a running
//     transaction (all of it when there is none), and does not hand a
//     share one connector leaves unused to the others. GetCompositeSchedule
//     for connector 0 reports the station's total: the station's own
//     TxDefaultProfile (or the per-connector maximum) times the running
//     transactions, capped by ChargePointMaxProfile. Profiles set on
//     connectors 1..n do not enter that figure.
//   - Relative schedules, and Absolute ones without a startSchedule, start
//     "now": the simulator does not keep a transaction's start time, so they
//     are always in their first period. A Recurring schedule is evaluated in
//     its current recurrence only.
//   - Units are not converted for GetCompositeSchedule: asking in a unit
//     other than the profiles' is Rejected. The meter, which needs watts,
//     converts amperes at chargeVoltage.

func (s *Simulator) setChargingProfile(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.SetChargingProfileReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}
	rejected := v16.SetChargingProfileConf{Status: v16.ProfileRejected}

	if s.opts.Scenario == ScenarioRejectProfile {
		s.log.Info("refusing charging profile", "scenario", string(s.opts.Scenario))
		return rejected, nil
	}

	p := req.CSChargingProfiles
	if why := s.refuseProfile(req.ConnectorID, p); why != "" {
		s.log.Info("refusing charging profile", "connector", req.ConnectorID,
			"profile_id", p.ChargingProfileID, "reason", why)
		return rejected, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if why := s.refuseForTransactionLocked(req.ConnectorID, p); why != "" {
		s.log.Info("refusing charging profile", "connector", req.ConnectorID,
			"profile_id", p.ChargingProfileID, "reason", why)
		return rejected, nil
	}

	replaces := func(id int, old v16.ChargingProfile) bool {
		return old.ChargingProfileID == p.ChargingProfileID ||
			(id == req.ConnectorID && old.ChargingProfilePurpose == p.ChargingProfilePurpose &&
				old.StackLevel == p.StackLevel)
	}
	kept := 0
	for id, list := range s.profiles {
		for _, old := range list {
			if !replaces(id, old) {
				kept++
			}
		}
	}
	if kept >= maxProfilesInstalled {
		s.log.Info("refusing charging profile", "connector", req.ConnectorID,
			"profile_id", p.ChargingProfileID, "reason", "MaxChargingProfilesInstalled reached")
		return rejected, nil
	}
	for id, list := range s.profiles {
		s.profiles[id] = slices.DeleteFunc(list, func(old v16.ChargingProfile) bool { return replaces(id, old) })
	}
	s.profiles[req.ConnectorID] = append(s.profiles[req.ConnectorID], p)

	s.log.Info("charging profile set", "connector", req.ConnectorID, "profile_id", p.ChargingProfileID,
		"purpose", p.ChargingProfilePurpose, "stack_level", p.StackLevel)
	return v16.SetChargingProfileConf{Status: v16.ProfileAccepted}, nil
}

// refuseProfile checks what does not depend on the station's state: the
// connector exists, the purpose fits it, and the profile stays within the
// limits the configuration keys advertise. It returns why it refuses, or "".
func (s *Simulator) refuseProfile(connectorID int, p v16.ChargingProfile) string {
	switch {
	case connectorID < 0 || connectorID > s.opts.Connectors:
		return "no such connector"
	case p.ChargingProfilePurpose == v16.PurposeChargePointMax && connectorID != 0:
		return "ChargePointMaxProfile is for connector 0"
	case p.ChargingProfilePurpose == v16.PurposeTx && connectorID == 0:
		return "TxProfile needs a connector"
	case p.ChargingProfilePurpose != v16.PurposeChargePointMax &&
		p.ChargingProfilePurpose != v16.PurposeTxDefault && p.ChargingProfilePurpose != v16.PurposeTx:
		return "unknown purpose"
	case p.StackLevel < 0 || p.StackLevel > maxStackLevel:
		return "stack level beyond ChargeProfileMaxStackLevel"
	case len(p.ChargingSchedule.ChargingSchedulePeriod) == 0:
		return "no schedule periods"
	case len(p.ChargingSchedule.ChargingSchedulePeriod) > maxSchedulePeriods:
		return "more periods than ChargingScheduleMaxPeriods"
	case p.ChargingSchedule.ChargingRateUnit != v16.UnitW && p.ChargingSchedule.ChargingRateUnit != v16.UnitA:
		return "unknown charging rate unit"
	}
	return ""
}

// refuseForTransactionLocked refuses a TxProfile without a running
// transaction on its connector to apply to, as the spec expects of a
// station. That is the case on a connector that is only reserved. The
// caller holds s.mu.
func (s *Simulator) refuseForTransactionLocked(connectorID int, p v16.ChargingProfile) string {
	if p.ChargingProfilePurpose != v16.PurposeTx {
		return ""
	}
	c := s.connectors[connectorID]
	if c.transactionID == 0 {
		return "TxProfile without a running transaction"
	}
	if p.TransactionID != nil && *p.TransactionID != c.transactionID {
		return "TxProfile for another transaction"
	}
	return ""
}

func (s *Simulator) clearChargingProfile(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.ClearChargingProfileReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cleared := 0
	for id, list := range s.profiles {
		if req.ConnectorID != nil && *req.ConnectorID != id {
			continue
		}
		s.profiles[id] = slices.DeleteFunc(list, func(p v16.ChargingProfile) bool {
			match := (req.ID == nil || *req.ID == p.ChargingProfileID) &&
				(req.ChargingProfilePurpose == "" || req.ChargingProfilePurpose == p.ChargingProfilePurpose) &&
				(req.StackLevel == nil || *req.StackLevel == p.StackLevel)
			if match {
				cleared++
			}
			return match
		})
	}
	if cleared == 0 {
		return v16.ClearChargingProfileConf{Status: v16.ClearUnknown}, nil
	}
	s.log.Info("charging profiles cleared", "count", cleared)
	return v16.ClearChargingProfileConf{Status: v16.ClearAccepted}, nil
}

func (s *Simulator) getCompositeSchedule(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.GetCompositeScheduleReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}
	rejected := v16.GetCompositeScheduleConf{Status: v16.CompositeRejected}
	if req.ConnectorID < 0 || req.ConnectorID > s.opts.Connectors || req.Duration <= 0 {
		return rejected, nil
	}

	s.mu.Lock()
	profiles := s.applicableLocked(req.ConnectorID)
	sessions := s.sessionsLocked()
	s.mu.Unlock()

	unit := req.ChargingRateUnit
	if unit == "" {
		unit = v16.UnitW
		if len(profiles) > 0 {
			unit = profiles[0].ChargingSchedule.ChargingRateUnit
		}
	}
	for _, p := range profiles {
		if p.ChargingSchedule.ChargingRateUnit != unit {
			s.log.Info("refusing composite schedule", "reason", "units differ", "unit", unit)
			return rejected, nil
		}
	}
	stationMax := float64(chargePower)
	if unit == v16.UnitA {
		stationMax = float64(chargePower / chargeVoltage)
	}
	conv := shareOf(req.ConnectorID, sessions, func(_ v16.ChargingProfile, l float64) float64 { return l })
	if req.ConnectorID == 0 {
		stationMax *= float64(sessions)
	}

	now := s.opts.Now().UTC().Truncate(time.Second)
	end := now.Add(time.Duration(req.Duration) * time.Second)
	moments := []time.Time{now}
	for _, p := range profiles {
		for _, t := range boundaries(p, now) {
			if t.After(now) && t.Before(end) {
				moments = append(moments, t)
			}
		}
	}
	slices.SortFunc(moments, func(a, b time.Time) int { return a.Compare(b) })
	moments = slices.Compact(moments)

	var periods []v16.ChargingSchedulePeriod
	for _, t := range moments {
		limit, ok := compositeLimit(profiles, now, t, conv)
		if !ok {
			// No profile applies: the station's own maximum does.
			limit = stationMax
		}
		// The schema allows one decimal place.
		limit = math.Floor(limit*10) / 10
		if n := len(periods); n > 0 && periods[n-1].Limit == limit {
			continue
		}
		periods = append(periods, v16.ChargingSchedulePeriod{
			StartPeriod: int(t.Sub(now) / time.Second),
			Limit:       limit,
		})
	}

	connectorID, duration := req.ConnectorID, req.Duration
	return v16.GetCompositeScheduleConf{
		Status:        v16.CompositeAccepted,
		ConnectorID:   &connectorID,
		ScheduleStart: now.Format(time.RFC3339),
		// Errata 3.57: startSchedule is omitted here, scheduleStart says it.
		ChargingSchedule: &v16.ChargingSchedule{
			Duration:               &duration,
			ChargingRateUnit:       unit,
			ChargingSchedulePeriod: periods,
		},
	}, nil
}

// applicableLocked lists the profiles that bear on a connector: its own and
// the station's. A TxProfile counts only while its transaction runs, so one
// left behind by a session that ended some other way has no effect. The
// caller holds s.mu.
func (s *Simulator) applicableLocked(connectorID int) []v16.ChargingProfile {
	out := slices.Clone(s.profiles[0])
	if connectorID == 0 {
		return out
	}
	c := s.connectors[connectorID]
	for _, p := range s.profiles[connectorID] {
		if p.ChargingProfilePurpose == v16.PurposeTx &&
			(c.transactionID == 0 || (p.TransactionID != nil && *p.TransactionID != c.transactionID)) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// limitWLocked is the limit, in W, the profiles put on a connector at now,
// and false when none applies. The caller holds s.mu.
func (s *Simulator) limitWLocked(connectorID int, now time.Time) (int, bool) {
	toW := func(p v16.ChargingProfile, l float64) float64 {
		if p.ChargingSchedule.ChargingRateUnit == v16.UnitA {
			return l * chargeVoltage
		}
		return l
	}
	conv := shareOf(connectorID, s.sessionsLocked(), toW)
	limit, ok := compositeLimit(s.applicableLocked(connectorID), now, now, conv)
	return int(math.Floor(limit)), ok
}

// sessionsLocked counts the connectors with a running transaction, at least
// one. The caller holds s.mu.
func (s *Simulator) sessionsLocked() int {
	n := 0
	for id, c := range s.connectors {
		if id != 0 && c.transactionID != 0 {
			n++
		}
	}
	return max(n, 1)
}

// shareOf wraps conv to turn station and connector limits into one
// connector's (see the model above): on a connector, its share of
// ChargePointMaxProfile; on connector 0, the station's total of the
// per-connector limits.
func shareOf(connectorID, sessions int, conv func(v16.ChargingProfile, float64) float64) func(v16.ChargingProfile, float64) float64 {
	return func(p v16.ChargingProfile, l float64) float64 {
		l = conv(p, l)
		switch {
		case connectorID != 0 && p.ChargingProfilePurpose == v16.PurposeChargePointMax:
			return l / float64(sessions)
		case connectorID == 0 && p.ChargingProfilePurpose != v16.PurposeChargePointMax:
			return l * float64(sessions)
		}
		return l
	}
}

// clearTxProfilesLocked removes a connector's TxProfiles when its
// transaction ends. The caller holds s.mu.
func (s *Simulator) clearTxProfilesLocked(connectorID int) {
	s.profiles[connectorID] = slices.DeleteFunc(s.profiles[connectorID], func(p v16.ChargingProfile) bool {
		return p.ChargingProfilePurpose == v16.PurposeTx
	})
}

// reset answers Reset. Charging profiles persist across reboots (errata
// 3.37 on §5.16), so it keeps them; nothing else is modelled.
func (s *Simulator) reset() (any, *ocpp.RPCError) {
	return v16.ResetConf{Status: v16.CmdAccepted}, nil
}

// compositeLimit combines the profiles' limits at t (see the model above),
// each converted by conv. It returns false when no profile applies.
func compositeLimit(profiles []v16.ChargingProfile, now, t time.Time, conv func(v16.ChargingProfile, float64) float64) (float64, bool) {
	type winner struct {
		level int
		limit float64
		ok    bool
	}
	best := map[string]winner{}
	for _, p := range profiles {
		limit, ok := limitAt(p, anchor(p, now), t)
		if !ok {
			continue
		}
		if w := best[p.ChargingProfilePurpose]; !w.ok || p.StackLevel > w.level {
			best[p.ChargingProfilePurpose] = winner{p.StackLevel, conv(p, limit), true}
		}
	}

	tx := best[v16.PurposeTx]
	if !tx.ok {
		tx = best[v16.PurposeTxDefault]
	}
	station := best[v16.PurposeChargePointMax]
	switch {
	case tx.ok && station.ok:
		return min(tx.limit, station.limit), true
	case tx.ok:
		return tx.limit, true
	case station.ok:
		return station.limit, true
	}
	return 0, false
}

// anchor is when a profile's schedule starts, as seen at now.
func anchor(p v16.ChargingProfile, now time.Time) time.Time {
	start, ok := parseTime(p.ChargingSchedule.StartSchedule)
	if !ok || p.ChargingProfileKind == v16.KindRelative {
		return now
	}
	if p.ChargingProfileKind == v16.KindRecurring {
		every := 24 * time.Hour
		if p.RecurrencyKind == v16.RecurrencyWeekly {
			every *= 7
		}
		if now.After(start) {
			start = start.Add(now.Sub(start) / every * every)
		}
	}
	return start
}

// limitAt is the limit p sets at t, given its schedule starts at start, and
// false when p does not apply at t.
func limitAt(p v16.ChargingProfile, start, t time.Time) (float64, bool) {
	if from, ok := parseTime(p.ValidFrom); ok && t.Before(from) {
		return 0, false
	}
	if to, ok := parseTime(p.ValidTo); ok && !t.Before(to) {
		return 0, false
	}
	if t.Before(start) {
		return 0, false
	}
	offset := t.Sub(start)
	sched := p.ChargingSchedule
	if sched.Duration != nil && offset >= time.Duration(*sched.Duration)*time.Second {
		return 0, false
	}
	limit, ok := 0.0, false
	for _, period := range sched.ChargingSchedulePeriod {
		if time.Duration(period.StartPeriod)*time.Second > offset {
			break
		}
		limit, ok = period.Limit, true
	}
	return limit, ok
}

// boundaries are the moments p's limit may change, seen at now.
func boundaries(p v16.ChargingProfile, now time.Time) []time.Time {
	start := anchor(p, now)
	var out []time.Time
	for _, period := range p.ChargingSchedule.ChargingSchedulePeriod {
		out = append(out, start.Add(time.Duration(period.StartPeriod)*time.Second))
	}
	if d := p.ChargingSchedule.Duration; d != nil {
		out = append(out, start.Add(time.Duration(*d)*time.Second))
	}
	for _, edge := range []string{p.ValidFrom, p.ValidTo} {
		if t, ok := parseTime(edge); ok {
			out = append(out, t)
		}
	}
	return out
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
