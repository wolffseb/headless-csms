package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// The message names what the station answered, and never prints an empty
// status: a station that answers with a CALLERROR has no status to name.
func TestExitForNamesTheStationsAnswer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   socket.Op
		err  *socket.Error
		want string
	}{
		{"callerror on reserve", socket.OpReserve,
			&socket.Error{Code: socket.CodeUnlockFailed, CallError: "NotImplemented"},
			"ALP-1: the station does not support this (CALLERROR NotImplemented)"},
		{"not supported status", socket.OpUnlock,
			&socket.Error{Code: socket.CodeUnlockFailed, Status: "NotSupported"},
			"ALP-1: the station does not support this (status NotSupported)"},
		{"no answer at all", socket.OpUnlock,
			&socket.Error{Code: socket.CodeUnlockFailed},
			"ALP-1: the station does not support this"},
		{"limit not supported", socket.OpLimitSet,
			&socket.Error{Code: socket.CodeNotSupported, Status: "NotSupported"},
			"ALP-1: the station does not support this charging profile (status NotSupported). `cpms probe` shows its SmartCharging support, allowed units and limits"},
		{"clear unknown", socket.OpLimitClear,
			&socket.Error{Code: socket.CodeRejected, Status: "Unknown", Message: "x; cpms dropped its 2 record(s) of them"},
			"ALP-1: the station holds no matching charging profile (status Unknown); cpms dropped its records of them"},
		{"rejected", socket.OpStart,
			&socket.Error{Code: socket.CodeRejected, Status: "Rejected"},
			"ALP-1: the station rejected the remote start (status Rejected)"},
	}
	for _, c := range cases {
		err := exitFor(c.op, "ALP-1", c.err)
		if got := err.Error(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(err.Error(), "status )") {
			t.Errorf("%s: empty status in %q", c.name, err)
		}
	}
}

// "Busy, not sent" is the one failure that promises nothing happened, so it
// must not share a code with a timeout, after which the station may well have
// acted, and the message says so.
func TestBusyHasItsOwnExitCode(t *testing.T) {
	t.Parallel()

	err := exitFor(socket.OpUnlock, "EVSE-1", &socket.Error{Code: socket.CodeBusy})
	if code := ExitCode(err); code != ExitBusy || ExitBusy != 12 {
		t.Errorf("busy exits %d, want 12", code)
	}
	for _, other := range []int{ExitOK, ExitError, ExitNotRunning, ExitNotConnected, ExitRejected, ExitOccupied,
		ExitFaulted, ExitTimeout, ExitUnlockFailed, ExitRefused, ExitMissingCapability, ExitNotSupported} {
		if other == ExitBusy {
			t.Errorf("exit code %d is used for busy and for something else", other)
		}
	}
	for _, want := range []string{"EVSE-1", "busy", "not sent", "Nothing changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should mention %q", err, want)
		}
	}
}

// The CLI must outwait the daemon: a command that waits the whole queue cap
// for its turn and then the whole call timeout for the station's answer is
// still answered by the daemon before the CLI gives up. A CLI that gave up
// earlier would report a failure for a command the station then carries out.
func TestSocketWaitOutlastsQueueAndCall(t *testing.T) {
	t.Parallel()

	for _, callTimeout := range []time.Duration{time.Second, 30 * time.Second, 2 * time.Minute} {
		queueCap := callTimeout // ocppj.Options.QueueTimeout defaults to the call timeout
		if got, need := socketWait(callTimeout), queueCap+callTimeout+socketGrace; got < need {
			t.Errorf("call_timeout %s: waits %s, want at least queue cap + call timeout + margin = %s", callTimeout, got, need)
		}
	}
}

func TestNotSupportedHasItsOwnExitCode(t *testing.T) {
	t.Parallel()
	if code := ExitCode(exitFor(socket.OpLimitSet, "EVSE-1", &socket.Error{Code: socket.CodeNotSupported})); code != 11 {
		t.Errorf("not_supported exits %d, want 11", code)
	}
}

// A profile that went out unanswered may be held by the station although
// cpms records nothing: the message names its id and how to find and clear
// it, and the exit code stays the timeout's or the connection's.
func TestUnconfirmedProfileNamesItsID(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		code socket.Code
		exit int
	}{{socket.CodeTimeout, ExitTimeout}, {socket.CodeNotConnected, ExitNotConnected}} {
		err := exitFor(socket.OpLimitSet, "EVSE-1", &socket.Error{Code: c.code, ProfileID: 7})
		if code := ExitCode(err); code != c.exit {
			t.Errorf("%s: exit %d, want %d", c.code, code, c.exit)
		}
		for _, want := range []string{"charging profile #7", "`cpms limit show`", "`cpms limit clear --id 7`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q, want it to mention %q", c.code, err, want)
			}
		}
	}
	// Without an id (any other command) the message is as before.
	if err := exitFor(socket.OpStart, "EVSE-1", &socket.Error{Code: socket.CodeTimeout}); strings.Contains(err.Error(), "limit") {
		t.Errorf("a timed-out start mentions limits: %q", err)
	}
}
