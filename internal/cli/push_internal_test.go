package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// The status line cuts the last error at 60 characters, so what the pusher
// stores must open with the reason. A partner that is down is the common
// failure: its reason has to survive the cut.
func TestPushHealthShowsWhyAnUnreachablePartnerFails(t *testing.T) {
	t.Parallel()

	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, reason := range []string{
		"dial tcp 127.0.0.1:19590: connect: connection refused",
		"ocpi: HTTP 500: Internal Server Error",
	} {
		line := pushHealth(socket.PushStatus{LastError: reason, LastErrorAt: &at, Pending: 1})
		if !strings.Contains(line, reason) {
			t.Errorf("pushHealth with %q = %q, want the whole reason in it", reason, line)
		}
		if !strings.Contains(line, "last push failed") || !strings.Contains(line, "1 pending") {
			t.Errorf("pushHealth with %q = %q", reason, line)
		}
	}
}
