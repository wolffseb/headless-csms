package cli

import (
	"errors"
	"fmt"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// Exit codes of the one-shot commands. They are part of cpms's interface:
// scripts branch on them, so a code's meaning never changes once released.
const (
	ExitOK           = 0
	ExitError        = 1 // usage, config, or anything unexpected
	ExitNotRunning   = 2 // cpms run is not reachable
	ExitNotConnected = 3 // the charge point is not connected
	ExitRejected     = 4 // the station answered Rejected
	ExitOccupied     = 5
	ExitFaulted      = 6 // Faulted or Unavailable
	ExitTimeout      = 7 // no answer within charger.call_timeout
	ExitUnlockFailed = 8 // unlock failed, or not supported by the station
	ExitRefused      = 9 // refused locally: already reserved, nothing to cancel or stop
	// ExitMissingCapability means `cpms probe` found the station lacks a
	// capability cpms requires. It is distinct so the on-site hardware
	// check can script it.
	ExitMissingCapability = 10
)

// ExitStatus is an error that carries the process exit code. main exits with
// Code instead of the default 1.
type ExitStatus struct {
	Code int
	Err  error
}

func (e *ExitStatus) Error() string { return e.Err.Error() }

func (e *ExitStatus) Unwrap() error { return e.Err }

// ExitCode is the exit code for an error returned by a command: 0 for nil,
// the carried code for an *ExitStatus, and 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var es *ExitStatus
	if errors.As(err, &es) {
		return es.Code
	}
	return ExitError
}

// exitFor turns a socket error into the exit code and the message a person
// sees. The message says what happened and, where there is one, what to do.
func exitFor(op socket.Op, evse string, err error) error {
	if errors.Is(err, socket.ErrNotRunning) {
		return &ExitStatus{Code: ExitNotRunning, Err: fmt.Errorf("%w. Is `cpms run` running, with the same --config or --socket?", err)}
	}

	var se *socket.Error
	if !errors.As(err, &se) {
		return &ExitStatus{Code: ExitError, Err: err}
	}

	subject := evse
	if subject == "" {
		subject = string(op)
	}
	msg := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{subject}, args...)...)
	}

	switch se.Code {
	case socket.CodeNotConnected:
		return &ExitStatus{Code: ExitNotConnected, Err: msg(
			"the charge point is not connected to cpms. Check that the station's CSMS URL points here (`cpms config validate` prints it)")}
	case socket.CodeRejected:
		hint := ""
		if op == socket.OpReserve {
			hint = ". If it does not support the Reservation profile, `cpms probe` will say so"
		}
		return &ExitStatus{Code: ExitRejected, Err: msg("the station rejected the %s (status %s)%s", noun(op), se.Status, hint)}
	case socket.CodeOccupied:
		return &ExitStatus{Code: ExitOccupied, Err: msg("the station says it is occupied (status %s)", se.Status)}
	case socket.CodeFaulted:
		return &ExitStatus{Code: ExitFaulted, Err: msg("the station says it is faulted or unavailable (status %s)", se.Status)}
	case socket.CodeTimeout:
		return &ExitStatus{Code: ExitTimeout, Err: msg("the station did not answer within charger.call_timeout")}
	case socket.CodeUnlockFailed:
		if se.Status == "UnlockFailed" {
			return &ExitStatus{Code: ExitUnlockFailed, Err: msg("the station could not release the cable lock (status %s)", se.Status)}
		}
		return &ExitStatus{Code: ExitUnlockFailed, Err: msg("the station does not support this (status %s)", se.Status)}
	case socket.CodeRefused:
		// control's message already names the EVSE.
		return &ExitStatus{Code: ExitRefused, Err: errors.New(se.Message)}
	default:
		return &ExitStatus{Code: ExitError, Err: errors.New(se.Message)}
	}
}

func noun(op socket.Op) string {
	switch op {
	case socket.OpReserve:
		return "reservation"
	case socket.OpCancel:
		return "cancellation"
	case socket.OpStart:
		return "remote start"
	case socket.OpStop:
		return "remote stop"
	default:
		return string(op)
	}
}
