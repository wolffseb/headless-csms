package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wolffseb/cli-cpms/internal/config"
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
	ExitRefused      = 9 // refused locally: already reserved, nothing to cancel or stop, shutting down
	// ExitMissingCapability means `cpms probe` found the station lacks a
	// capability cpms requires. It is distinct so the on-site hardware
	// check can script it.
	ExitMissingCapability = 10
	// ExitNotSupported means the station does not support a charging limit
	// or profile: it answered NotSupported, or a NotImplemented or
	// NotSupported CALLERROR.
	ExitNotSupported = 11
	// ExitBusy means the station was busy with an earlier command for as long
	// as this one could wait, so it was not sent. Nothing happened and asking
	// again is safe, which no other failure promises: after a timeout the
	// station may still have acted.
	ExitBusy = 12
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
// cfg only gives a timeout its duration; nil leaves the duration out.
func exitFor(cfg *config.Config, op socket.Op, evse string, err error) error {
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
			"the charge point is not connected to cpms. Check that the station's CSMS URL points here (`cpms config validate` prints it)%s", unconfirmed(se))}
	case socket.CodeRejected:
		hint := ""
		switch {
		case op == socket.OpReserve:
			hint = ". If it does not support the Reservation profile, `cpms probe` will say so"
		case op == socket.OpConfigSet && se.Status == "NotSupported":
			hint = ": it does not know this key. `cpms probe --raw` lists the keys it reports"
		case op == socket.OpConfigSet:
			hint = ". The key may be read-only, or the value not one it accepts"
		case op == socket.OpLimitClear && se.Status == "Unknown":
			dropped := ""
			if strings.Contains(se.Message, "cpms dropped") {
				dropped = "; cpms dropped its records of them"
			}
			return &ExitStatus{Code: ExitRejected, Err: msg("the station holds no matching charging profile (status Unknown)%s", dropped)}
		case op == socket.OpLimitSet || op == socket.OpProfileSet:
			hint = ". A tx profile needs a running session; otherwise check the unit, stack level and periods against `cpms probe`"
		}
		return &ExitStatus{Code: ExitRejected, Err: msg("the station rejected the %s%s%s", noun(op), answer(se), hint)}
	case socket.CodeOccupied:
		return &ExitStatus{Code: ExitOccupied, Err: msg("the station says it is occupied%s", answer(se))}
	case socket.CodeFaulted:
		return &ExitStatus{Code: ExitFaulted, Err: msg("the station says it is faulted or unavailable%s", answer(se))}
	case socket.CodeTimeout:
		return &ExitStatus{Code: ExitTimeout, Err: msg("the station did not answer within %s%s", callTimeoutText(cfg, subject), unconfirmed(se))}
	case socket.CodeBusy:
		return &ExitStatus{Code: ExitBusy, Err: msg(
			"the station was busy with an earlier command for the whole of charger.call_timeout, so this one was not sent. Nothing changed; try again")}
	case socket.CodeUnlockFailed:
		if se.Status == "UnlockFailed" {
			return &ExitStatus{Code: ExitUnlockFailed, Err: msg("the station could not release the cable lock%s", answer(se))}
		}
		return &ExitStatus{Code: ExitUnlockFailed, Err: msg("the station does not support this%s", answer(se))}
	case socket.CodeNotSupported:
		return &ExitStatus{Code: ExitNotSupported, Err: msg(
			"the station does not support this %s%s. `cpms probe` shows its SmartCharging support, allowed units and limits", noun(op), answer(se))}
	case socket.CodeRefused:
		// control's message already names the EVSE.
		return &ExitStatus{Code: ExitRefused, Err: errors.New(se.Message)}
	default:
		return &ExitStatus{Code: ExitError, Err: errors.New(se.Message)}
	}
}

// describe is the message a person sees when a command fails: the one-shot
// CLI's wording, for an error that came over the socket or, as in the TUI,
// straight from control. subject is the EVSE (or key) the command was about.
func describe(cfg *config.Config, op socket.Op, subject string, err error) string {
	var se *socket.Error
	if !errors.As(err, &se) {
		se = socket.Classify(err)
	}
	return exitFor(cfg, op, subject, se).Error()
}

// callTimeoutText names the call_timeout a command to subject waited for:
// "10s (charger.call_timeout)", or just the key when it cannot be told. The
// subject is an EVSE when it can be told; a command that addresses the
// station (or a config key) has none, so the duration is named only if every
// charger has the same.
func callTimeoutText(cfg *config.Config, subject string) string {
	const key = "charger.call_timeout"
	if cfg == nil || len(cfg.Chargers) == 0 {
		return key
	}
	var ch []config.Charger
	if e, ok := cfg.EVSEByUID(subject); ok {
		c, found := cfg.ChargerByID(cfg.ChargerOf(e))
		if !found {
			return key
		}
		ch = []config.Charger{c}
	} else {
		ch = cfg.Chargers
	}
	for _, c := range ch[1:] {
		if c.CallTimeout.Duration() != ch[0].CallTimeout.Duration() {
			return key
		}
	}
	if ch[0].CallTimeout.Duration() <= 0 {
		return key
	}
	return ch[0].CallTimeout.String() + " (" + key + ")"
}

// unconfirmed names a charging profile that went out unanswered, for the end
// of a timeout or connection message: the station may hold it although cpms
// records nothing. It is empty for every other request.
func unconfirmed(se *socket.Error) string {
	if se.ProfileID == 0 {
		return ""
	}
	return fmt.Sprintf(". It was sent as charging profile #%d, which the station may hold: check with `cpms limit show`, "+
		"remove it with `cpms limit clear --id %d`", se.ProfileID, se.ProfileID)
}

// answer is the station's own answer, for the end of a message: " (status
// Rejected)", or " (CALLERROR NotImplemented)" when it answered with an error
// instead of a status. It is empty when there is nothing to say, never an
// empty "(status )".
func answer(se *socket.Error) string {
	switch {
	case se.Status != "":
		return " (status " + se.Status + ")"
	case se.CallError != "":
		return " (CALLERROR " + se.CallError + ")"
	default:
		return ""
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
	case socket.OpConfigSet:
		return "configuration change"
	case socket.OpLimitSet, socket.OpProfileSet:
		return "charging profile"
	case socket.OpLimitClear:
		return "clearing of charging profiles"
	case socket.OpLimitShow:
		return "composite schedule request"
	default:
		return string(op)
	}
}
