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
//   - *RefusedError: the station answered, and the answer was no;
//   - ocpp.ErrTimeout: the station did not answer in time;
//   - ocpp.ErrNotConnected: the station is not connected;
//   - ocpp.ErrUnknownEVSE: the EVSE is not in config;
//   - *ocpp.RPCError: the station answered with a CALLERROR.
//
// All of them are matched with errors.Is or errors.As.
var (
	// ErrAlreadyReserved means the EVSE already holds a reservation.
	ErrAlreadyReserved = core.ErrAlreadyReserved
	// ErrNoReservation means there is no active reservation to cancel.
	ErrNoReservation = errors.New("no active reservation")
	// ErrNoActiveTransaction means there is no running session to stop.
	ErrNoActiveTransaction = errors.New("no active transaction")
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
)

// RefusedError means the station answered the command and said no: Rejected,
// Occupied, Faulted, Unavailable, UnlockFailed, NotSupported, or a status the
// spec does not define.
type RefusedError struct {
	Op      Op
	EVSEUID string
	// Status is the answer in version-agnostic terms.
	Status ocpp.CommandStatus
	// Raw is what the station literally sent.
	Raw string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("%s %s: the station refused (%s)", e.Op, e.EVSEUID, e.Raw)
}

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
