package ocpp

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// ChargingProfile limits how much power or current an EVSE, or the whole
// station, may draw over time. It is the version-agnostic form of OCPP 1.6's
// csChargingProfiles and 2.0.1's ChargingProfileType; the adapters translate
// it, and Validate checks it before either of them sends anything.
type ChargingProfile struct {
	// ID is allocated by the caller. A profile with the same ID replaces the
	// one installed before it.
	ID int
	// StackLevel orders profiles of the same purpose: the highest wins.
	StackLevel int
	Purpose    ProfilePurpose
	Kind       ProfileKind
	// RecurrencyKind is set for a Recurring profile only.
	RecurrencyKind RecurrencyKind
	// ValidFrom and ValidTo bound when the profile applies; nil is open.
	ValidFrom, ValidTo *time.Time
	// TransactionID is the running transaction a Tx profile is for, as core
	// knows it. It is set for PurposeTx only.
	TransactionID string
	Schedule      ChargingSchedule
	// EVSEUID is the EVSE the profile is for. Empty means the whole station,
	// which only PurposeStationMax may use: cpms always addresses one EVSE
	// with the other purposes.
	EVSEUID string
}

// ProfilePurpose says what a charging profile limits.
type ProfilePurpose string

// The profile purposes.
const (
	// PurposeStationMax caps the whole station (1.6 ChargePointMaxProfile,
	// 2.0.1 ChargingStationMaxProfile).
	PurposeStationMax ProfilePurpose = "StationMax"
	// PurposeTxDefault applies to transactions that start after it is set,
	// which makes it the one that can be installed while an EVSE is
	// reserved and has no transaction yet.
	PurposeTxDefault ProfilePurpose = "TxDefault"
	// PurposeTx applies to one running transaction and overrides TxDefault.
	PurposeTx ProfilePurpose = "Tx"
)

// ProfileKind says how a schedule is anchored in time.
type ProfileKind string

// The profile kinds.
const (
	// KindAbsolute starts at the schedule's StartSchedule.
	KindAbsolute ProfileKind = "Absolute"
	// KindRecurring repeats daily or weekly from StartSchedule.
	KindRecurring ProfileKind = "Recurring"
	// KindRelative starts when the transaction starts.
	KindRelative ProfileKind = "Relative"
)

// RecurrencyKind is how often a Recurring profile repeats.
type RecurrencyKind string

// The recurrency kinds. The zero value means "not recurring".
const (
	RecurrencyDaily  RecurrencyKind = "Daily"
	RecurrencyWeekly RecurrencyKind = "Weekly"
)

// RateUnit is the unit of a schedule's limits.
type RateUnit string

// The rate units.
const (
	// UnitW is watts, across all phases.
	UnitW RateUnit = "W"
	// UnitA is amperes per phase.
	UnitA RateUnit = "A"
)

// ChargingSchedule is a list of limits over time.
type ChargingSchedule struct {
	// Duration is how long the schedule lasts; nil means until the profile
	// ends. On the wire it is whole seconds.
	Duration *time.Duration
	// StartSchedule anchors an Absolute or Recurring schedule.
	StartSchedule *time.Time
	Unit          RateUnit
	// Periods start at 0 and increase strictly.
	Periods []SchedulePeriod
	// MinRate is the lowest rate the EV can usefully charge at, if known.
	MinRate *float64
}

// SchedulePeriod is one step of a schedule: Limit applies from Start (an
// offset from the schedule's start) until the next period's Start.
type SchedulePeriod struct {
	// Start is whole seconds on the wire.
	Start time.Duration
	// Limit is in the schedule's Unit, with at most one decimal place.
	Limit float64
	// Phases is the number of phases to use, if the profile says.
	Phases *int
}

// ClearProfileRequest selects the charging profiles to remove. Each nil
// field means "any"; the fields that are set must all match.
type ClearProfileRequest struct {
	ID *int
	// EVSEUID set to "" means the station itself (connector 0 in 1.6).
	EVSEUID    *string
	Purpose    *ProfilePurpose
	StackLevel *int
}

// CompositeSchedule is a station's answer to GetCompositeSchedule: the
// limits it will actually apply, all profiles combined.
type CompositeSchedule struct {
	// Status is CommandAccepted or CommandRejected, or CommandUnknown with
	// Raw holding what the station said.
	Status CommandStatus
	Raw    string
	// EVSEUID is the EVSE asked about; empty for the whole station.
	EVSEUID string
	// Start is when the schedule begins; Schedule's periods count from it.
	Start time.Time
	// Schedule is nil when the station did not send one, as when it
	// rejected the request.
	Schedule *ChargingSchedule
}

// ErrInvalidProfile means a charging profile, or a request about one, is
// refused before anything is sent. It is wrapped with the reason.
var ErrInvalidProfile = errors.New("ocpp: invalid charging profile")

// invalid wraps ErrInvalidProfile with a reason.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidProfile, fmt.Sprintf(format, args...))
}

// Validate checks a profile against the rules both OCPP versions share, so
// that a mistake is refused here rather than by the station, or silently
// rounded on the wire. It returns an error wrapping ErrInvalidProfile.
func (p ChargingProfile) Validate() error {
	if p.ID < 0 {
		return invalid("id %d is negative", p.ID)
	}
	if p.StackLevel < 0 {
		return invalid("stack level %d is negative", p.StackLevel)
	}

	switch p.Purpose {
	case PurposeStationMax:
		if p.EVSEUID != "" {
			return invalid("a %s profile is for the whole station, not EVSE %q", p.Purpose, p.EVSEUID)
		}
	case PurposeTxDefault, PurposeTx:
		// 1.6 allows TxDefault on connector 0 for every connector at once,
		// but cpms always addresses one EVSE.
		if p.EVSEUID == "" {
			return invalid("a %s profile needs an EVSE", p.Purpose)
		}
	default:
		return invalid("unknown purpose %q", p.Purpose)
	}
	if p.Purpose == PurposeTx && p.TransactionID == "" {
		return invalid("a %s profile needs a transaction id", p.Purpose)
	}
	if p.Purpose != PurposeTx && p.TransactionID != "" {
		return invalid("only a %s profile takes a transaction id", PurposeTx)
	}

	switch p.Kind {
	case KindAbsolute, KindRelative:
		if p.RecurrencyKind != "" {
			return invalid("a %s profile takes no recurrency kind", p.Kind)
		}
	case KindRecurring:
		if p.RecurrencyKind != RecurrencyDaily && p.RecurrencyKind != RecurrencyWeekly {
			return invalid("recurrency kind %q is neither %s nor %s", p.RecurrencyKind, RecurrencyDaily, RecurrencyWeekly)
		}
		if p.Schedule.StartSchedule == nil {
			return invalid("a %s profile needs a schedule start", p.Kind)
		}
	default:
		return invalid("unknown kind %q", p.Kind)
	}

	if p.ValidFrom != nil && p.ValidTo != nil && !p.ValidTo.After(*p.ValidFrom) {
		return invalid("valid to %s is not after valid from %s", p.ValidTo, p.ValidFrom)
	}
	return p.Schedule.validate()
}

func (s ChargingSchedule) validate() error {
	if s.Unit != UnitW && s.Unit != UnitA {
		return invalid("rate unit %q is neither %s nor %s", s.Unit, UnitW, UnitA)
	}
	if s.Duration != nil {
		if err := wholeSeconds("duration", *s.Duration); err != nil {
			return err
		}
	}
	if s.MinRate != nil {
		if err := oneDecimal("minimum rate", *s.MinRate); err != nil {
			return err
		}
	}
	if len(s.Periods) == 0 {
		return invalid("the schedule has no periods")
	}
	for i, period := range s.Periods {
		if err := wholeSeconds(fmt.Sprintf("period %d start", i), period.Start); err != nil {
			return err
		}
		switch {
		case i == 0 && period.Start != 0:
			return invalid("the first period starts at %s, not 0", period.Start)
		case i > 0 && period.Start <= s.Periods[i-1].Start:
			return invalid("period %d starts at %s, not after period %d", i, period.Start, i-1)
		}
		if err := oneDecimal(fmt.Sprintf("period %d limit", i), period.Limit); err != nil {
			return err
		}
		if period.Phases != nil && (*period.Phases < 1 || *period.Phases > 3) {
			return invalid("period %d has %d phases, not 1 to 3", i, *period.Phases)
		}
	}
	return nil
}

// wholeSeconds refuses a negative or fractional number of seconds: both
// versions carry durations as integer seconds.
func wholeSeconds(what string, d time.Duration) error {
	if d < 0 || d%time.Second != 0 {
		return invalid("%s %s is not a whole number of seconds", what, d)
	}
	return nil
}

// oneDecimal refuses a negative rate, or one with more than one decimal
// place: the schemas say multipleOf 0.1, and 12.34 is refused rather than
// rounded to something the caller did not ask for.
func oneDecimal(what string, v float64) error {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return invalid("%s %v is not a rate", what, v)
	}
	if tenths := v * 10; math.Abs(tenths-math.Round(tenths)) > 1e-6 {
		return invalid("%s %v has more than one decimal place", what, v)
	}
	return nil
}
