package cli_test

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// cpmsResult is what one `cpms` invocation returned.
type cpmsResult struct {
	stdout, stderr string
	code           int
	took           time.Duration
}

// cpmsConcurrently runs the same command n times at once, as n terminals
// would.
func (s *station) cpmsConcurrently(n int, args ...string) []cpmsResult {
	s.t.Helper()

	results := make([]cpmsResult, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			stdout, stderr, code := s.cpms(args...)
			results[i] = cpmsResult{stdout: stdout, stderr: stderr, code: code, took: time.Since(start)}
		}()
	}
	wg.Wait()
	return results
}

// Two commands sent at once are queued on the one-call-in-flight rule (#63).
// The one that waits must not run out of patience before it is answered: the
// CLI may never report a failure for a command the station carries out.
//
// The station answers each command after 5/6 of call_timeout, as the review
// of #76 had it: the second command then takes 5/3 of call_timeout from the
// moment it was typed, which is more than the CLI's old allowance of
// call_timeout plus five seconds. The numbers are the smallest that show it
// with that allowance, so this test takes about 15 seconds.
func TestConcurrentCommandsAreBothReportedAsDone(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("takes 15 seconds: it has to outlast the CLI's 5 s allowance")
	}

	s := newStation(t, 19800, "heartbeat_timeout: 90s", "heartbeat_timeout: 90s\n  call_timeout: 9s")
	s.connect(func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 7500 * time.Millisecond
	})

	for i, r := range s.cpmsConcurrently(2, "unlock") {
		if r.code != 0 || !strings.Contains(r.stdout, "EVSE-1 unlocked") {
			t.Errorf("unlock #%d exited %d after %s, want 0: it was sent to the station (stdout %q, stderr %q)",
				i+1, r.code, r.took.Round(100*time.Millisecond), r.stdout, r.stderr)
		}
	}
}

// A command that cannot get its turn on the station within call_timeout is
// not sent, and the CLI says so with a code of its own. That is the other half
// of the rule above: the CLI reports failure only for commands that did not
// go out, and "busy" is the one failure that promises it. After a timeout the
// station may have acted.
//
// Three commands at once, each answered after 1.2 s with a call_timeout of
// 2 s: two take their turns (0 to 1.2 s, 1.2 to 2.4 s), the third would only
// get the station at 2.4 s and gives up at 2 s.
func TestCommandThatCannotGetItsTurnIsNotSent(t *testing.T) {
	t.Parallel()

	s := newStation(t, 19801, "heartbeat_timeout: 90s", "heartbeat_timeout: 90s\n  call_timeout: 2s")
	station := &syncBuffer{}
	s.connect(func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 1200 * time.Millisecond
		// The simulator logs each command it receives at debug level.
		o.Log = slog.New(slog.NewTextHandler(station, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	var done, busy int
	for i, r := range s.cpmsConcurrently(3, "unlock") {
		switch r.code {
		case 0:
			done++
		case cli.ExitBusy:
			busy++
			for _, want := range []string{"EVSE-1", "busy", "not sent"} {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("unlock #%d: stderr %q should mention %q", i+1, r.stderr, want)
				}
			}
			if r.took > 4*time.Second {
				t.Errorf("unlock #%d took %s to give up, want about the 2 s call timeout", i+1, r.took)
			}
		default:
			t.Errorf("unlock #%d exited %d, want 0 or %d: %s", i+1, r.code, cli.ExitBusy, r.stderr)
		}
	}
	if done != 2 || busy != 1 {
		t.Fatalf("%d commands done, %d busy; want 2 and 1", done, busy)
	}

	// Both answered commands have reached the station by now. The one
	// reported as not sent is not among them (that it also never goes out
	// later is the ocppj test's job: TestQueuedCallPastTheCapIsNotSent).
	if got := strings.Count(station.String(), "action=UnlockConnector"); got != 2 {
		t.Errorf("the station received %d UnlockConnector commands, want 2; log:\n%s", got, station.String())
	}
}
