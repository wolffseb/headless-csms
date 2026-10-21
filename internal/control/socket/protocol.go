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
	// CodeNotConnected means the station is not connected to cpms.
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
	// CodeUnlockFailed means the station could not release the cable, or does
	// not support the command.
	CodeUnlockFailed Code = "unlock_failed"
	// CodeRefused means cpms refused locally, before sending anything:
	// already reserved, nothing to cancel, nothing to stop.
	CodeRefused Code = "refused"
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

// Status is the result of OpStatus. Its JSON is what `cpms status --json`
// prints, so its field names are a public interface: other tools parse it.
type Status struct {
	ChargePoint ChargePointStatus `json:"charge_point"`
	EVSEs       []EVSEStatus      `json:"evses"`
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
}

// ReservationStatus is a reservation held on an EVSE.
type ReservationStatus struct {
	ID        int       `json:"id"`
	IDTag     string    `json:"id_tag"`
	ExpiresAt time.Time `json:"expires_at"`
	// State is "pending" while the station has not answered, then "active".
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
