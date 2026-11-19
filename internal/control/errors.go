package control

import (
	"errors"
	"fmt"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// Errors refused locally, before anything is sent to the station. Besides
// these, a Service method can return:
//
//   - ocpp.ErrInvalidProfile: a charging profile or limit that cannot be sent;
//   - *RefusedError: the station answered, and the answer was no;
//   - ocpp.ErrTimeout: the station did not answer in time;
//   - ocpp.ErrNotConnected: the station is not connected, or the connection
//     dropped (the command may or may not have gone out, unless the error
//     also matches ocpp.ErrNotSent);
//   - ocpp.ErrNotSent: the station stayed busy with an earlier command, or
//     the connection closed while this one waited, so it was never sent (the
//     one thing every other error here may not promise);
//   - ocpp.ErrUnknownEVSE: the EVSE is not in config;
//   - *ocpp.RPCError: the station answered with a CALLERROR.
//
// All of them are matched with errors.Is or errors.As.
var (
	// ErrAlreadyReserved means the EVSE already holds a reservation.
	ErrAlreadyReserved = core.ErrAlreadyReserved
	// ErrNoReservation means there is no active reservation to cancel.
	ErrNoReservation = errors.New("no active reservation")
	// ErrShuttingDown means Close has been called: no new reservation is
	// accepted, because its cancel could not be waited for.
	ErrShuttingDown = errors.New("shutting down")
	// ErrNoActiveTransaction means there is no running session to stop.
	ErrNoActiveTransaction = errors.New("no active transaction")
	// ErrNoKeyValueConfig means the station's OCPP version has no flat
	// key/value configuration to change.
	ErrNoKeyValueConfig = errors.New("this OCPP version has no key/value configuration")
)

// Op names the action a RefusedError is about, in words a person would use.
type Op string

// The actions.
const (
	OpReserve       Op = "reserve"
	OpCancel        Op = "cancel"
	OpStart         Op = "start"
	OpStop          Op = "stop"
	OpUnlock        Op = "unlock"
	OpTriggerStatus Op = "trigger status"
	OpConfigSet     Op = "config set"
	OpSetProfile    Op = "set charging profile"
	OpClearProfiles Op = "clear charging profiles"
	OpSchedule      Op = "composite schedule"
)

// RefusedError means the station answered the command and said no: Rejected,
// Occupied, Faulted, Unavailable, UnlockFailed, NotSupported, or a status the
// spec does not define.
type RefusedError struct {
	Op Op
	// EVSEUID is what the command was about: the EVSE, or for OpConfigSet
	// the configuration key.
	EVSEUID string
	// Status is the answer in version-agnostic terms.
	Status ocpp.CommandStatus
	// Raw is what the station literally sent.
	Raw string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s %s: the station refused (%s)", e.Op, e.EVSEUID, e.Raw)
}

// UnconfirmedProfileError means SetChargingProfile went out but no answer
// came back (a timeout, or the connection dropped). The station may hold
// the profile although cpms records nothing, so the error names the id it
// was sent with. It wraps the send error: errors.Is(err, ocpp.ErrTimeout)
// still holds.
type UnconfirmedProfileError struct {
	ID int
	// Target is the EVSE uid, or "station".
	Target string
	Err    error
}

func (e *UnconfirmedProfileError) Error() string {
	return fmt.Sprintf("%s: charging profile #%d was sent but not confirmed (%v); the station may hold it. "+
		"Check with `cpms limit show`, remove it with `cpms limit clear --id %d`", e.Target, e.ID, e.Err, e.ID)
}

func (e *UnconfirmedProfileError) Unwrap() error { return e.Err }

func refused(op Op, evseUID string, res ocpp.Result) error {
	return &RefusedError{Op: op, EVSEUID: evseUID, Status: res.Status, Raw: res.Raw}
}

// endReason is why a pending reservation ends when sending it failed.
func endReason(err error) string {
	if errors.Is(err, ocpp.ErrTimeout) {
		return core.ReservationTimeout
	}
	return core.ReservationFailed
}
