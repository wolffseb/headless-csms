package simulator

import (
	"testing"
	"time"
)

// TestBackoff pins the default redial schedule: doubling from 1 s, capped at
// 30 s, and jitter that only ever shortens a wait, by at most a fifth.
func TestBackoff(t *testing.T) {
	t.Parallel()

	none := func() float64 { return 0 }
	most := func() float64 { return 0.999999 }

	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, w := range want {
		attempt := i + 1
		if got := backoff(attempt, none); got != w {
			t.Errorf("attempt %d without jitter: %s, want %s", attempt, got, w)
		}
		if got := backoff(attempt, most); got <= w*4/5-time.Millisecond || got > w {
			t.Errorf("attempt %d with full jitter: %s, want just over %s", attempt, got, w*4/5)
		}
	}
	// Far past the cap, the shift must not overflow into a short wait.
	if got := backoff(1000, none); got != 30*time.Second {
		t.Errorf("attempt 1000: %s, want the 30s cap", got)
	}
}
