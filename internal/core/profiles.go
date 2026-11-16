package core

import (
	"fmt"
	"sort"
	"time"

	"github.com/wolffseb/cli-cpms/internal/state"
)

// The charging profile purposes, as core records them. They are the
// version-agnostic names of ocpp.ProfilePurpose; core does not import the
// protocol package.
const (
	PurposeStationMax = "StationMax"
	PurposeTxDefault  = "TxDefault"
	PurposeTx         = "Tx"
)

// Why a charging profile record was dropped, as carried in
// EventChargingProfileCleared.Detail.
const (
	// ProfileCleared means the station accepted a ClearChargingProfile.
	ProfileCleared = "cleared"
	// ProfileUnknownAtStation means a clear was answered "Unknown": the
	// station does not hold the profile, so neither do we.
	ProfileUnknownAtStation = "unknown at the station"
	// ProfileReplaced means a newer profile took its id, or its EVSE,
	// purpose and stack level, which replaces it at the station too.
	ProfileReplaced = "replaced"
	// ProfileTransactionEnded means the transaction a Tx profile was for
	// stopped: the spec has a TxProfile end with its transaction.
	ProfileTransactionEnded = "transaction ended"
)

// ChargingProfile is a charging profile the station accepted, as cpms sent
// it. It is a record, not the truth: the station may have dropped it since,
// for instance on a reboot, and only it knows the limit it applies.
type ChargingProfile struct {
	ID            int
	ChargePointID string
	// EVSEUID is "" for a profile on the whole station.
	EVSEUID    string
	Purpose    string
	StackLevel int
	// Unit is "W" or "A".
	Unit    string
	Periods []ChargingPeriod
	// ValidFrom and ValidTo are nil when open.
	ValidFrom, ValidTo *time.Time
	// TransactionID is set for a Tx profile.
	TransactionID string
	SetAt         time.Time
	// Source is who set it: "cli", "tui" or "ocpi".
	Source string
}

// ChargingPeriod is one step of a recorded schedule: Limit applies from
// Start, an offset from the schedule's start, until the next period.
type ChargingPeriod struct {
	Start  time.Duration
	Limit  float64
	Phases *int
}

// ProfileFilter selects charging profile records. Each nil field means
// "any"; the fields that are set must all match, as in OCPP's
// ClearChargingProfile.
type ProfileFilter struct {
	ID *int
	// EVSEUID set to "" selects the station's own profiles.
	EVSEUID    *string
	Purpose    *string
	StackLevel *int
}

func (f ProfileFilter) matches(p ChargingProfile) bool {
	return (f.ID == nil || *f.ID == p.ID) &&
		(f.EVSEUID == nil || *f.EVSEUID == p.EVSEUID) &&
		(f.Purpose == nil || *f.Purpose == p.Purpose) &&
		(f.StackLevel == nil || *f.StackLevel == p.StackLevel)
}

// NextChargingProfileID hands out a chargingProfileId. Ids are never reused,
// including across restarts: a profile sent with the id of one the station
// still holds would silently replace it.
//
// Like NextRemoteStartID this is on the command side, so the counter is
// persisted before the id is returned, and a failed write is returned: the
// profile should not go out.
func (s *Service) NextChargingProfileID() (int, error) {
	s.mu.Lock()
	id := s.nextChargingProfileID
	s.nextChargingProfileID++
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		return 0, err
	}
	return id, nil
}

// RecordChargingProfile records a profile the station has accepted. It
// replaces any record with the same id, or the same charge point, EVSE,
// purpose and stack level, which is what the station does with the profiles
// themselves.
//
// The station already holds the profile, so a failed write is logged rather
// than returned.
func (s *Service) RecordChargingProfile(p ChargingProfile) ChargingProfile {
	if p.SetAt.IsZero() {
		p.SetAt = s.now()
	}
	p = p.clone()

	s.mu.Lock()
	var events []Event
	kept := s.profiles[:0:0]
	for _, old := range s.profiles {
		if old.ID == p.ID || (old.ChargePointID == p.ChargePointID && old.EVSEUID == p.EVSEUID &&
			old.Purpose == p.Purpose && old.StackLevel == p.StackLevel) {
			events = append(events, s.profileEvent(EventChargingProfileCleared, old, ProfileReplaced))
			continue
		}
		kept = append(kept, old)
	}
	s.profiles = append(kept, p)
	// A record made from a profile file may carry an id from elsewhere;
	// never hand it out again.
	s.nextChargingProfileID = max(s.nextChargingProfileID, p.ID+1)
	events = append(events, s.profileEvent(EventChargingProfileSet, p, describeProfile(p)))
	s.mu.Unlock()

	s.persistOrLog("charging profile", p.ID)
	s.publishAll(events)
	return p.clone()
}

// ForgetChargingProfiles drops the records f selects on a charge point and
// returns them, reporting each with EventChargingProfileCleared and reason.
func (s *Service) ForgetChargingProfiles(cpID string, f ProfileFilter, reason string) []ChargingProfile {
	s.mu.Lock()
	removed, events := s.forgetLocked(func(p ChargingProfile) bool {
		return p.ChargePointID == cpID && f.matches(p)
	}, reason)
	s.mu.Unlock()

	if len(removed) > 0 {
		s.persistOrLog("cleared charging profiles", removed[0].ID)
		s.publishAll(events)
	}
	return removed
}

// forgetLocked removes the records drop selects and returns them with the
// events to publish once the lock is released.
func (s *Service) forgetLocked(drop func(ChargingProfile) bool, reason string) ([]ChargingProfile, []Event) {
	var removed []ChargingProfile
	var events []Event
	kept := s.profiles[:0:0]
	for _, p := range s.profiles {
		if drop(p) {
			removed = append(removed, p)
			events = append(events, s.profileEvent(EventChargingProfileCleared, p, reason))
			continue
		}
		kept = append(kept, p)
	}
	s.profiles = kept
	return removed, events
}

// ChargingProfiles returns the records for one EVSE, or for the station
// itself when evseUID is "", by id.
func (s *Service) ChargingProfiles(evseUID string) []ChargingProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := []ChargingProfile{}
	for _, p := range s.profiles {
		if p.EVSEUID == evseUID {
			out = append(out, p.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// profilesLocked lists a charge point's records by id.
func (s *Service) profilesLocked(cpID string) []ChargingProfile {
	out := []ChargingProfile{}
	for _, p := range s.profiles {
		if p.ChargePointID == cpID {
			out = append(out, p.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Service) profileEvent(kind EventKind, p ChargingProfile, detail string) Event {
	return Event{
		Kind: kind, At: s.now(), ChargePointID: p.ChargePointID, EVSEUID: p.EVSEUID,
		TransactionID: p.TransactionID, ChargingProfileID: p.ID, Detail: detail,
	}
}

// describeProfile is a profile in a few words, for the event log.
func describeProfile(p ChargingProfile) string {
	s := fmt.Sprintf("%s stack %d", p.Purpose, p.StackLevel)
	if len(p.Periods) > 0 {
		s += fmt.Sprintf(", %g %s", p.Periods[0].Limit, p.Unit)
		if len(p.Periods) > 1 {
			s += fmt.Sprintf(" (+%d periods)", len(p.Periods)-1)
		}
	}
	return s
}

// clone deep-copies a record, so no caller shares its periods or times.
func (p ChargingProfile) clone() ChargingProfile {
	out := p
	out.Periods = make([]ChargingPeriod, len(p.Periods))
	for i, period := range p.Periods {
		out.Periods[i] = period
		if period.Phases != nil {
			n := *period.Phases
			out.Periods[i].Phases = &n
		}
	}
	out.ValidFrom = copyTime(p.ValidFrom)
	out.ValidTo = copyTime(p.ValidTo)
	return out
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// profileToState and profileFromState convert between core's record and
// the store's DTO.
func profileToState(p ChargingProfile) state.ChargingProfile {
	periods := make([]state.ChargingPeriod, len(p.Periods))
	for i, period := range p.Periods {
		periods[i] = state.ChargingPeriod{
			StartSeconds: int(period.Start / time.Second), Limit: period.Limit, Phases: period.Phases,
		}
	}
	return state.ChargingProfile{
		ID: p.ID, ChargePointID: p.ChargePointID, EVSEUID: p.EVSEUID,
		Purpose: p.Purpose, StackLevel: p.StackLevel, Unit: p.Unit, Periods: periods,
		ValidFrom: p.ValidFrom, ValidTo: p.ValidTo, TransactionID: p.TransactionID,
		SetAt: p.SetAt, Source: p.Source,
	}
}

func profileFromState(p state.ChargingProfile) ChargingProfile {
	periods := make([]ChargingPeriod, len(p.Periods))
	for i, period := range p.Periods {
		periods[i] = ChargingPeriod{
			Start: time.Duration(period.StartSeconds) * time.Second, Limit: period.Limit, Phases: period.Phases,
		}
	}
	return ChargingProfile{
		ID: p.ID, ChargePointID: p.ChargePointID, EVSEUID: p.EVSEUID,
		Purpose: p.Purpose, StackLevel: p.StackLevel, Unit: p.Unit, Periods: periods,
		ValidFrom: p.ValidFrom, ValidTo: p.ValidTo, TransactionID: p.TransactionID,
		SetAt: p.SetAt, Source: p.Source,
	}
}
