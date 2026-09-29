package ocpp

import (
	"context"
	"errors"
	"time"
)

// ChargePoint is a connected station that can be sent commands, whatever OCPP
// version it speaks.
//
// It is the seam between callers that want something done (the CLI, OCPI
// Commands, the TUI) and the version adapters that know how to say it on the
// wire. Nothing on the caller's side of it names an OCPP message, a connector
// id or a wire status string.
//
// EVSEs are addressed by their OCPI uid from config. Each adapter translates
// that to its own addressing: a connectorId in 1.6, an evseId/connectorId pair
// in 2.0.1.
//
// A station saying no (Rejected, Occupied, ...) is not an error: the call
// worked, and the answer is in Result. Errors are reserved for the call not
// working at all: ErrTimeout, ErrNotConnected, ErrUnknownEVSE, or a CALLERROR
// from the station as *RPCError.
type ChargePoint interface {
	// ID is the charge point identity.
	ID() string
	// Version is the OCPP version the station speaks.
	Version() Version

	// ReserveNow holds an EVSE for a token until r.ExpiresAt.
	ReserveNow(ctx context.Context, r ReserveRequest) (Result, error)
	// CancelReservation releases a reservation made with ReserveNow.
	CancelReservation(ctx context.Context, reservationID int) (Result, error)
	// RemoteStart asks the station to start a session on an EVSE.
	RemoteStart(ctx context.Context, r RemoteStartRequest) (Result, error)
	// RemoteStop asks the station to end a running transaction.
	RemoteStop(ctx context.Context, transactionID string) (Result, error)
	// UnlockConnector releases the cable lock on an EVSE.
	UnlockConnector(ctx context.Context, evseUID string) (Result, error)
	// TriggerStatus asks the station to report an EVSE's status now.
	TriggerStatus(ctx context.Context, evseUID string) (Result, error)
	// Capabilities reports which optional features the station advertises.
	Capabilities(ctx context.Context) (Capabilities, error)
}

// ReserveRequest asks for an EVSE to be held for a token.
type ReserveRequest struct {
	// ReservationID is allocated by the caller, not by the adapter.
	ReservationID int
	EVSEUID       string
	IDTag         string
	ExpiresAt     time.Time
}

// RemoteStartRequest asks for a session to be started on an EVSE.
type RemoteStartRequest struct {
	// RemoteStartID ties the resulting transaction back to this request.
	// It is allocated by the caller, like ReservationID. OCPP 2.0.1 sends it
	// and the station echoes it; 1.6 has nowhere to put it and ignores it.
	RemoteStartID int
	EVSEUID       string
	IDTag         string
}

// Result is a station's answer to a command.
type Result struct {
	// Status is the answer in version-agnostic terms.
	Status CommandStatus
	// Raw is the station's literal status, for logs and error messages. It is
	// the only record of what the station said when Status is CommandUnknown.
	Raw string
}

// Capabilities are the optional features a station advertises. A feature
// that is not advertised is false; "not advertised" is not an error.
type Capabilities struct {
	Reservation   bool
	RemoteTrigger bool
}

// CommandStatus is a station's answer to a command, covering every outcome a
// caller has to act on differently.
type CommandStatus string

// The command statuses.
const (
	CommandAccepted       CommandStatus = "Accepted"
	CommandRejected       CommandStatus = "Rejected"
	CommandOccupied       CommandStatus = "Occupied"
	CommandFaulted        CommandStatus = "Faulted"
	CommandUnavailable    CommandStatus = "Unavailable"
	CommandUnlocked       CommandStatus = "Unlocked"
	CommandUnlockFailed   CommandStatus = "UnlockFailed"
	CommandNotSupported   CommandStatus = "NotSupported"
	CommandNotImplemented CommandStatus = "NotImplemented"
	// CommandUnknown is a status the station sent that the spec does not
	// define for that command. Result.Raw holds what it was; it is never
	// guessed at.
	CommandUnknown CommandStatus = "Unknown"
)

// Errors a command can fail with, matched with errors.Is.
var (
	// ErrTimeout means the station did not answer within the call timeout.
	ErrTimeout = errors.New("ocpp: call timed out")
	// ErrNotConnected means the charge point is not connected, or its
	// connection closed while the call was in flight.
	ErrNotConnected = errors.New("ocpp: charge point not connected")
	// ErrUnknownEVSE means the EVSE uid is not in config. It is returned
	// before anything is sent.
	ErrUnknownEVSE = errors.New("ocpp: unknown EVSE")
)
