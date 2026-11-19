// Package socket lets one-shot commands drive the station through a running
// `cpms run`.
//
// cpms is one process by design, and the charger dials that process, so a
// second process cannot talk to the station itself. Instead `cpms run`
// listens on a Unix socket and the one-shot commands send it a request. The
// socket is a thin transport over internal/control: it adds no domain logic,
// it only carries a request in and a result or a typed error out.
//
// The socket doubles as the single-instance guard: a second `cpms run`
// pointed at the same socket finds the first one answering and refuses to
// start, which is what keeps two processes from writing the same state.json.
package socket

import (
	"encoding/json"
	"time"
)

// Version is the protocol version this build speaks. A request with any other
// version is refused, so a mismatched CLI and daemon fail loudly rather than
// misreading each other.
const Version = 1

// Op is the action a request asks for.
type Op string

// The operations.
const (
	OpReserve Op = "reserve"
	OpCancel  Op = "cancel"
	OpStart   Op = "start"
	OpStop    Op = "stop"
	OpUnlock  Op = "unlock"
	OpStatus  Op = "status"
	OpProbe   Op = "probe"
	// OpConfigSet changes one of the station's configuration keys.
	OpConfigSet Op = "config_set"
	// OpLimitSet installs a single-limit charging profile.
	OpLimitSet Op = "limit_set"
	// OpLimitClear clears charging profiles.
	OpLimitClear Op = "limit_clear"
	// OpLimitShow asks the station for its composite schedule.
	OpLimitShow Op = "limit_show"
	// OpProfileSet installs a full charging profile.
	OpProfileSet Op = "profile_set"
	// OpOCPIUnregister forgets the OCPI registration, so the counterparty
	// can register again with TOKEN_A.
	OpOCPIUnregister Op = "ocpi-unregister"
)

// Request is one line sent to the socket. Each connection carries exactly
// one request and one response.
type Request struct {
	V    int    `json:"v"`
	Op   Op     `json:"op"`
	EVSE string `json:"evse,omitempty"`
	// IDTag is the RFID tag for reserve and start. Empty means the
	// configured auth.default_id_tag.
	IDTag string `json:"id_tag,omitempty"`
	// ExpiresIn is a Go duration ("15m") for reserve. Empty means the
	// default reservation length.
	ExpiresIn string `json:"expires_in,omitempty"`
	// Key and Value are the station configuration key and its new value for
	// config_set.
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
	// TransactionID names the session for stop; empty means the one
	// active on the EVSE.
	TransactionID string `json:"transaction_id,omitempty"`
	// Station addresses the whole station (OCPP 1.6 connector 0) instead of
	// EVSE, for the limit and profile operations.
	Station bool `json:"station,omitempty"`
	// Limit is the limit for limit_set.
	Limit *LimitRequest `json:"limit,omitempty"`
	// Clear selects the profiles for limit_clear.
	Clear *ClearRequest `json:"clear,omitempty"`
	// Schedule is the window for limit_show.
	Schedule *ScheduleRequest `json:"schedule,omitempty"`
	// Profile is the profile for profile_set.
	Profile *Profile `json:"profile,omitempty"`
}

// LimitRequest is a single-limit profile. Exactly one of KW and Amps is set.
type LimitRequest struct {
	KW   *float64 `json:"kw,omitempty"`
	Amps *float64 `json:"amps,omitempty"`
	// Purpose is "tx-default" (the default), "station-max" or "tx".
	Purpose    string `json:"purpose,omitempty"`
	StackLevel int    `json:"stack_level,omitempty"`
	// For is a Go duration ("1h") bounding the profile; empty is open-ended.
	For string `json:"for,omitempty"`
	// Force sends a tx profile without a transaction when none is running.
	Force bool `json:"force,omitempty"`
	// TransactionID names the session a tx limit is for; empty means the
	// one active on the EVSE.
	TransactionID string `json:"transaction_id,omitempty"`
}

// ClearRequest selects charging profiles. Each unset field means "any"; the
// request's EVSE or Station narrows it to one EVSE, or the station, only
// when ByTarget is set.
type ClearRequest struct {
	ID       *int   `json:"id,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
	ByTarget bool   `json:"by_target,omitempty"`
}

// ScheduleRequest is the window for a composite schedule.
type ScheduleRequest struct {
	// Duration is a Go duration; empty means an hour.
	Duration string `json:"duration,omitempty"`
	// Unit is "W" or "A"; empty leaves it to the station.
	Unit string `json:"unit,omitempty"`
}

// Profile is a charging profile in cpms's neutral JSON shape: what `cpms
// profile set -f` reads, documented in the README. Its id is not part of
// it; cpms allocates one.
type Profile struct {
	// Purpose is "tx-default", "station-max" or "tx".
	Purpose    string `json:"purpose"`
	StackLevel int    `json:"stack_level"`
	// Kind is "absolute" (the default), "relative" or "recurring".
	Kind string `json:"kind,omitempty"`
	// Recurrency is "daily" or "weekly", for a recurring profile.
	Recurrency string     `json:"recurrency,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
	// TransactionID pins a tx profile to a session; empty means the one
	// running on the EVSE.
	TransactionID string          `json:"transaction_id,omitempty"`
	Schedule      ProfileSchedule `json:"schedule"`
}

// ProfileSchedule is a profile's schedule.
type ProfileSchedule struct {
	// Unit is "W" or "A".
	Unit string `json:"unit"`
	// Start anchors an absolute or recurring schedule.
	Start           *time.Time      `json:"start,omitempty"`
	DurationSeconds *int            `json:"duration_s,omitempty"`
	MinRate         *float64        `json:"min_rate,omitempty"`
	Periods         []ProfilePeriod `json:"periods"`
}

// ProfilePeriod is one step of a schedule: Limit, in the schedule's unit,
// from StartSeconds after the schedule's start until the next period.
type ProfilePeriod struct {
	StartSeconds int     `json:"start_s"`
	Limit        float64 `json:"limit"`
	Phases       *int    `json:"phases,omitempty"`
}

// Response is the one line the socket answers with.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a failure, as a closed set of codes the CLI turns into exit codes.
type Error struct {
	Code Code `json:"code"`
	// Status is the station's literal answer, when the station said no.
	Status string `json:"status,omitempty"`
	// CallError is the OCPP error code, such as "NotImplemented", when the
	// station answered with a CALLERROR instead of a status. Status is empty
	// then.
	CallError string `json:"call_error,omitempty"`
	// ProfileID is the id of a charging profile that was sent but not
	// answered: the station may hold it although cpms records nothing.
	ProfileID int    `json:"profile_id,omitempty"`
	Message   string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Code classifies a failure. Every code maps to exactly one exit code of the
// CLI, so adding one is a change to the CLI's documented interface.
type Code string

// The error codes.
const (
	// CodeUsage is a request the daemon cannot act on as given: an unknown
	// EVSE, a bad duration, an unsupported protocol version.
	CodeUsage Code = "usage"
	// CodeNotConnected means the station is not connected to cpms, or the
	// connection dropped while the command was waiting for its turn or for
	// its answer. The command may or may not have reached the station.
	CodeNotConnected Code = "not_connected"
	// CodeRejected means the station answered Rejected, or a status the spec
	// does not define.
	CodeRejected Code = "rejected"
	// CodeOccupied means the station answered Occupied.
	CodeOccupied Code = "occupied"
	// CodeFaulted means the station answered Faulted or Unavailable.
	CodeFaulted Code = "faulted"
	// CodeTimeout means the station did not answer within the call timeout.
	CodeTimeout Code = "timeout"
	// CodeBusy means the station stayed busy with an earlier command for the
	// whole time this one could wait, so it was not sent. Unlike a timeout,
	// the station has not seen the command and will not: asking again is safe.
	CodeBusy Code = "busy"
	// CodeUnlockFailed means the station could not release the cable, or does
	// not support the command.
	CodeUnlockFailed Code = "unlock_failed"
	// CodeRefused means cpms refused locally, before sending anything:
	// already reserved, nothing to cancel, nothing to stop, or cpms is
	// shutting down.
	CodeRefused Code = "refused"
	// CodeNotSupported means the station does not support the charging
	// profile or request: it answered NotSupported, or a NotImplemented or
	// NotSupported CALLERROR, to a limit or profile operation.
	CodeNotSupported Code = "not_supported"
	// CodeInternal is anything else.
	CodeInternal Code = "internal"
)

// ReserveResult is the result of OpReserve.
type ReserveResult struct {
	ReservationID int       `json:"reservation_id"`
	EVSE          string    `json:"evse"`
	IDTag         string    `json:"id_tag"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ConfigSetResult is the result of OpConfigSet. Status is the station's
// literal answer: Accepted, or RebootRequired when the value only applies
// after a restart.
type ConfigSetResult struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Status string `json:"status"`
}

// ChargingProfileStatus is a charging profile the station accepted, as cpms
// sent it. The station may have dropped it since. It is part of status
// --json, so a public interface.
type ChargingProfileStatus struct {
	ID int `json:"id"`
	// Purpose is "tx-default", "station-max" or "tx".
	Purpose    string          `json:"purpose"`
	StackLevel int             `json:"stack_level"`
	Unit       string          `json:"unit"`
	Periods    []ProfilePeriod `json:"periods"`
	ValidFrom  *time.Time      `json:"valid_from,omitempty"`
	ValidTo    *time.Time      `json:"valid_to,omitempty"`
	// TransactionID is set for a tx profile.
	TransactionID string    `json:"transaction_id,omitempty"`
	SetAt         time.Time `json:"set_at"`
	Source        string    `json:"source"`
}

// ProfileResult is the result of OpLimitSet and OpProfileSet: the profile
// the station accepted, on EVSE ("" for the station).
type ProfileResult struct {
	EVSE string `json:"evse"`
	ChargingProfileStatus
}

// ClearResult is the result of OpLimitClear: the ids of the records cpms
// dropped.
type ClearResult struct {
	Dropped []int `json:"dropped"`
}

// ScheduleResult is the result of OpLimitShow: the limits the station will
// apply, all profiles combined.
type ScheduleResult struct {
	EVSE            string          `json:"evse"`
	Start           time.Time       `json:"start"`
	DurationSeconds *int            `json:"duration_s,omitempty"`
	Unit            string          `json:"unit"`
	Periods         []ProfilePeriod `json:"periods"`
}

// Status is the result of OpStatus. Its JSON is what `cpms status --json`
// prints, so its field names are a public interface: other tools parse it.
type Status struct {
	ChargePoint ChargePointStatus `json:"charge_point"`
	EVSEs       []EVSEStatus      `json:"evses"`
	OCPI        OCPIStatus        `json:"ocpi"`
}

// OCPIStatus is the OCPI registration. It never carries a token.
type OCPIStatus struct {
	Registered bool `json:"registered"`
	// Party is the counterparty, e.g. "DE*ABC". Empty when not registered.
	Party string `json:"party,omitempty"`
	// Since is when it first registered; nil when not registered.
	Since *time.Time `json:"since,omitempty"`
	// PushEndpoint is its Locations receiver, which status changes are pushed
	// to. Empty when it lists none, or when not registered.
	PushEndpoint string `json:"push_endpoint,omitempty"`
	// Push is how the status push is doing. Nil when not registered.
	Push *PushStatus `json:"push,omitempty"`
}

// PushStatus is the health of the OCPI status push.
type PushStatus struct {
	// LastSuccess is when the partner last accepted a PATCH; nil if none yet.
	LastSuccess *time.Time `json:"last_success,omitempty"`
	// LastError is why the last failing attempt failed, and LastErrorAt when.
	// A later success does not clear them: compare the two times.
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// Pending is how many EVSEs have a change waiting to be sent or retried.
	Pending int `json:"pending"`
}

// UnregisterResult is the result of OpOCPIUnregister.
type UnregisterResult struct {
	// Party is the counterparty that was registered.
	Party string `json:"party"`
	// NotifyError is why the counterparty could not be told, or empty when it
	// was.
	NotifyError string `json:"notify_error,omitempty"`
}

// ChargePointStatus describes the station as a whole.
type ChargePointStatus struct {
	ID     string `json:"id"`
	Online bool   `json:"online"`
	// LastSeen is nil until the station has ever said anything.
	LastSeen *time.Time `json:"last_seen"`
	Vendor   string     `json:"vendor"`
	Model    string     `json:"model"`
	Firmware string     `json:"firmware"`
	// StationStatus is what the station reported for itself (OCPP 1.6
	// connector 0).
	StationStatus string `json:"station_status"`
	// LastDisconnect is nil unless the station has disconnected since
	// `cpms run` started.
	LastDisconnect *DisconnectStatus `json:"last_disconnect"`
	// ChargingProfiles are the station-wide profiles it accepted, as sent.
	ChargingProfiles []ChargingProfileStatus `json:"charging_profiles"`
	// Meter is the station's own meter reading (OCPP 1.6 connector 0), such
	// as clock-aligned Power.Offered. Omitted when there is none.
	Meter *MeterStatus `json:"meter,omitempty"`
}

// DisconnectStatus is when and why the station last went offline.
type DisconnectStatus struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// EVSEStatus describes one EVSE.
type EVSEStatus struct {
	UID         string             `json:"uid"`
	Status      string             `json:"status"`
	ErrorCode   string             `json:"error_code"`
	Reservation *ReservationStatus `json:"reservation"`
	Transaction *TransactionStatus `json:"transaction"`
	// MeterStatus is the EVSE's newest meter reading, its fields inline.
	MeterStatus
	// ChargingProfiles are the profiles the station accepted for this EVSE,
	// as sent.
	ChargingProfiles []ChargingProfileStatus `json:"charging_profiles"`
}

// MeterStatus is the newest meter reading: what the station draws (power in
// W, the highest phase current in A) and the most it offers the car, which is
// the limit it applies. Each is omitted when the reading lacks it, and all of
// them, with ReadingAt, when there is no reading: none yet, or the session or
// the connection ended since.
type MeterStatus struct {
	PowerW          *float64   `json:"power_w,omitempty"`
	CurrentA        *float64   `json:"current_a,omitempty"`
	PowerOfferedW   *float64   `json:"power_offered_w,omitempty"`
	CurrentOfferedA *float64   `json:"current_offered_a,omitempty"`
	ReadingAt       *time.Time `json:"reading_at,omitempty"`
	// PowerAt and the rest are when each value was taken. A MeterValues
	// that lacks a value keeps the one before, so values can differ in age;
	// ReadingAt is the newest message.
	PowerAt          *time.Time `json:"power_at,omitempty"`
	CurrentAt        *time.Time `json:"current_at,omitempty"`
	PowerOfferedAt   *time.Time `json:"power_offered_at,omitempty"`
	CurrentOfferedAt *time.Time `json:"current_offered_at,omitempty"`
}

// ReservationStatus is a reservation held on an EVSE.
type ReservationStatus struct {
	ID        int       `json:"id"`
	IDTag     string    `json:"id_tag"`
	ExpiresAt time.Time `json:"expires_at"`
	// State is "pending" while the station has not answered, then "active". A
	// reservation whose ReserveNow timed out or failed is "cancelling" until
	// the cancel sent after it has an answer.
	State string `json:"state"`
}

// TransactionStatus is a session running on an EVSE.
type TransactionStatus struct {
	ID        string    `json:"id"`
	IDTag     string    `json:"id_tag"`
	StartedAt time.Time `json:"started_at"`
}

// ProbeResult is the result of OpProbe: what the station says about itself.
// Its JSON is part of what `cpms probe --json` prints, so like Status its
// field names are a public interface.
type ProbeResult struct {
	ChargePoint ProbeChargePoint `json:"charge_point"`
	// Features are in the station's own terms, such as 1.6 feature profiles.
	Features []ProbeFeature `json:"features"`
	// Capabilities are what cpms asks of a station, whatever the version.
	Capabilities []ProbeCapability `json:"capabilities"`
	Settings     []ProbeSetting    `json:"settings"`
	// Connectors and HeartbeatIntervalSeconds are null when the station did
	// not report them, or reported something unreadable.
	Connectors               *int          `json:"connectors"`
	HeartbeatIntervalSeconds *int          `json:"heartbeat_interval_s"`
	Raw                      []ProbeRawKey `json:"raw"`
}

// ProbeChargePoint identifies the probed station.
type ProbeChargePoint struct {
	ID           string `json:"id"`
	OCPPVersion  string `json:"ocpp_version"`
	Vendor       string `json:"vendor"`
	Model        string `json:"model"`
	SerialNumber string `json:"serial_number"`
	Firmware     string `json:"firmware"`
}

// ProbeFeature is one feature as the station names it. Support is "yes",
// "no" or "unknown".
type ProbeFeature struct {
	Name    string   `json:"name"`
	Support string   `json:"support"`
	Grants  []string `json:"grants"`
	// Assumed is true when the support was inferred rather than reported:
	// OCPP 1.6 makes Core mandatory, so it is assumed when the station
	// does not list its profiles.
	Assumed bool `json:"assumed"`
	// Note explains the support when the station's answer alone would
	// mislead, such as a profile left out of a list that is full. It is
	// empty otherwise.
	Note string `json:"note"`
}

// ProbeCapability is whether the station supports one capability.
type ProbeCapability struct {
	Name    string `json:"name"`
	Support string `json:"support"`
}

// ProbeSetting is one behaviour-relevant setting. Value is empty when
// Reported is false.
type ProbeSetting struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Reported bool   `json:"reported"`
	Meaning  string `json:"meaning"`
}

// ProbeRawKey is one configuration entry exactly as the station reported it.
type ProbeRawKey struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Readonly bool   `json:"readonly"`
}
