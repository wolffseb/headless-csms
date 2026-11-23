package csms

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

func TestIdleTimeoutIsLookedUpPerChargePoint(t *testing.T) {
	t.Parallel()

	// Two stations on one listener with their own heartbeat_timeout: the
	// short one is dropped, the other keeps the server-wide fallback.
	h := newHarness(t, func(o *Options) {
		o.IdleTimeoutFor = func(id string) time.Duration {
			if id == "FAST" {
				return 250 * time.Millisecond
			}
			return 0
		}
	})
	fast := ocpptest.MustDial(t, h.url("FAST"))
	defer fast.Close()
	slow := ocpptest.MustDial(t, h.url("SLOW"))
	defer slow.Close()

	waitFor(t, "both to register", func() bool { return h.core.IsOnline("FAST") && h.core.IsOnline("SLOW") })
	waitFor(t, "the short-timeout station to be dropped", func() bool { return !h.core.IsOnline("FAST") })
	time.Sleep(300 * time.Millisecond)
	if !h.core.IsOnline("SLOW") {
		t.Error("the station on the default timeout was dropped with the short one")
	}
}
