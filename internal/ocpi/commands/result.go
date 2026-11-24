package commands

import (
	"context"
	"errors"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// Response is OCPI's CommandResponseType: the CPO's immediate answer to a
// command, before anything has reached the station.
type Response string

// The CommandResponseTypes.
const (
	ResponseNotSupported   Response = "NOT_SUPPORTED"
	ResponseRejected       Response = "REJECTED"
	ResponseAccepted       Response = "ACCEPTED"
	ResponseUnknownSession Response = "UNKNOWN_SESSION"
)

// Result is OCPI's CommandResultType: what came of the command at the station,
// posted to the command's response_url later.
type Result string

// The CommandResultTypes.
const (
	ResultAccepted            Result = "ACCEPTED"
	ResultCanceledReservation Result = "CANCELED_RESERVATION"
	ResultEVSEOccupied        Result = "EVSE_OCCUPIED"
	ResultEVSEInoperative     Result = "EVSE_INOPERATIVE"
	ResultFailed              Result = "FAILED"
	ResultNotSupported        Result = "NOT_SUPPORTED"
	ResultRejected            Result = "REJECTED"
	ResultTimeout             Result = "TIMEOUT"
	ResultUnknownReservation  Result = "UNKNOWN_RESERVATION"
)

// ResultFor turns what control did into the CommandResultType the partner is
// told, with the error's text as the message (empty on success).
//
// The station's answers map as the issue's table says. What the station did
// not answer is told apart by what we know: no answer in time is TIMEOUT,
// anything else that stops the command (a dropped connection, a busy station
// that never got it, a CALLERROR) is FAILED, which is OCPI's "execution failed
// at the Charge Point", except a CALLERROR that says the action is not
// implemented, which is NOT_SUPPORTED.
func ResultFor(err error) (Result, string) {
	if err == nil {
		return ResultAccepted, ""
	}
	msg := err.Error()

	var refused *control.RefusedError
	if errors.As(err, &refused) {
		switch refused.Status {
		case ocpp.CommandRejected:
			return ResultRejected, msg
		case ocpp.CommandOccupied:
			return ResultEVSEOccupied, msg
		case ocpp.CommandFaulted, ocpp.CommandUnavailable:
			return ResultEVSEInoperative, msg
		case ocpp.CommandUnlockFailed:
			return ResultFailed, msg
		case ocpp.CommandNotSupported, ocpp.CommandNotImplemented:
			return ResultNotSupported, msg
		}
		return ResultFailed, msg
	}

	var rpc *ocpp.RPCError
	switch {
	case errors.Is(err, ocpp.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return ResultTimeout, msg
	case errors.Is(err, control.ErrNoReservation):
		return ResultUnknownReservation, msg
	case errors.Is(err, control.ErrAlreadyReserved):
		return ResultEVSEOccupied, msg
	case errors.As(err, &rpc) && (rpc.Code == ocpp.ErrNotImplemented || rpc.Code == ocpp.ErrNotSupported):
		return ResultNotSupported, msg
	default:
		return ResultFailed, msg
	}
}
