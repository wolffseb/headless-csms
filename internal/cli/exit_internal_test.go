package cli

import (
	"strings"
	"testing"

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

func TestNotSupportedHasItsOwnExitCode(t *testing.T) {
	t.Parallel()
	if code := ExitCode(exitFor(socket.OpLimitSet, "EVSE-1", &socket.Error{Code: socket.CodeNotSupported})); code != 11 {
		t.Errorf("not_supported exits %d, want 11", code)
	}
}
