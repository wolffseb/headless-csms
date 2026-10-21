package ocpp

import "time"

// Capability is something cpms asks a station to do, named independently of
// how any OCPP version advertises it.
type Capability string

// The capabilities a probe reports on.
const (
	CapRemoteStartStop Capability = "remote_start_stop"
	CapUnlock          Capability = "unlock"
	CapReservation     Capability = "reservation"
	CapTriggerStatus   Capability = "trigger_status"
)

// AllCapabilities lists every capability, in the order a report shows them.
func AllCapabilities() []Capability {
	return []Capability{CapRemoteStartStop, CapUnlock, CapReservation, CapTriggerStatus}
}

// Support is whether a station supports something.
type Support string

// The support levels. SupportUnknown means the station did not say, which is
// different from saying no.
const (
	SupportYes     Support = "yes"
	SupportNo      Support = "no"
	SupportUnknown Support = "unknown"
)

// ProbeReport is what a station says about itself when asked: which features
// it implements and the settings that change how it behaves towards cpms.
//
// It is version-agnostic. Each adapter fills it from its own version's
// mechanism (GetConfiguration in 1.6, GetVariables in 2.0.1), and puts the
// station's own names and the meaning of each setting into it, so a reader
// renders it without knowing any OCPP key name.
type ProbeReport struct {
	// Version is the OCPP version the station is speaking.
	Version Version

	// Features are the station's features in its own terms (1.6 feature
	// profiles): the ones cpms needs, present or not, then any other the
	// station advertises.
	Features []Feature

	// Settings are the values that change how the station behaves towards
	// cpms, in a fixed order, each with what it means.
	Settings []Setting

	// Connectors is how many connectors the station says it has, and
	// HeartbeatInterval how often it heartbeats. Each is nil when the station
	// did not report it or reported something unreadable. They are typed so
	// that they can be checked against config.
	Connectors        *int
	HeartbeatInterval *time.Duration

	// Raw is everything the station reported, as it reported it.
	Raw []RawSetting
}

// Feature is one feature as the station names it.
type Feature struct {
	Name    string
	Support Support
	// Grants are the capabilities this feature provides. A feature cpms has
	// no use for grants none.
	Grants []Capability
}

// Setting is one behaviour-relevant setting.
type Setting struct {
	// Name is the station's own name for it.
	Name string
	// Value is what the station reported; it is empty when Reported is false.
	Value    string
	Reported bool
	// Meaning says in one line why the setting matters to cpms.
	Meaning string
}

// RawSetting is one configuration entry exactly as the station reported it.
type RawSetting struct {
	Key      string
	Value    string
	Readonly bool
}

// Support reports whether the station supports a capability: yes if any
// feature granting it is present, no if every such feature is known to be
// absent, and unknown otherwise.
func (r ProbeReport) Support(c Capability) Support {
	granted, unknown := false, false
	for _, f := range r.Features {
		for _, g := range f.Grants {
			if g != c {
				continue
			}
			granted = true
			if f.Support == SupportYes {
				return SupportYes
			}
			if f.Support != SupportNo {
				unknown = true
			}
		}
	}
	if !granted || unknown {
		return SupportUnknown
	}
	return SupportNo
}
