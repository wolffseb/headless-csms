package push

import (
	"testing"
	"time"
)

// The default backoff doubles from one second, adds a little jitter, and
// never passes thirty seconds.
func TestDefaultBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	for retry := 1; retry <= 12; retry++ {
		floor := baseBackoff << (retry - 1)
		if floor > maxBackoff {
			floor = maxBackoff
		}
		for range 200 {
			d := defaultBackoff(retry)
			if d < floor || d > maxBackoff || d > floor+floor/4 {
				t.Fatalf("retry %d: %v, want within [%v, %v] and at most %v", retry, d, floor, floor+floor/4, maxBackoff)
			}
		}
	}
	if d := defaultBackoff(1); d < time.Second || d > 1250*time.Millisecond {
		t.Errorf("first retry waits %v, want about 1s", d)
	}
}

// Jitter is there: a hundred draws are not all the same.
func TestDefaultBackoffJitters(t *testing.T) {
	t.Parallel()

	seen := map[time.Duration]bool{}
	for range 100 {
		seen[defaultBackoff(3)] = true
	}
	if len(seen) < 2 {
		t.Error("no jitter")
	}
}
