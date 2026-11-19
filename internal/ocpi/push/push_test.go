package push_test

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/push"
)

// A change is one PATCH: the path from config, TOKEN_B, and a body with
// exactly the status and the moment it changed.
func TestPatchCarriesTheChange(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.connect()
	r.pushRound(r.evses())
	r.emsp.waitPatches(r.evses())

	// The status changes a minute in; the PATCH goes out at the end of the
	// debounce, half a second after that.
	r.clk.Advance(time.Minute)
	changedAt := r.clk.Now()
	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)

	got := r.emsp.waitPatches(r.evses() + 1)
	p := got[r.evses()]
	if want := r.evsePath(0); p.path != want {
		t.Errorf("path = %s, want %s", p.path, want)
	}
	if want := r.authHeader(); p.auth != want {
		t.Errorf("Authorization = %q, want %q", p.auth, want)
	}
	want := `{"status":"CHARGING","last_updated":"` + ocpi.FormatDateTime(changedAt) + `"}`
	if p.raw != want && strings.TrimSpace(p.raw) != want {
		t.Errorf("body = %s, want %s", p.raw, want)
	}
	if len(got) != r.evses()+1 {
		t.Errorf("%d PATCHes, want %d", len(got), r.evses()+1)
	}
}

// Five changes inside the debounce make one PATCH, with the last status, and
// it goes out when the window that the first change opened ends. A window
// that restarted on every change would never end for an EVSE that flaps for
// long enough.
func TestFlappingIsOnePatchWithTheFinalStatus(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.connect()
	r.pushRound(r.evses())
	r.emsp.waitPatches(r.evses())

	statuses := []core.EVSEStatus{core.StatusBlocked, core.StatusCharging, core.StatusAvailable, core.StatusReserved, core.StatusCharging}
	for i, s := range statuses {
		r.setStatus(0, s)
		if i == 0 {
			r.core.flush(t)
			r.waitTimer(1)
		}
		r.clk.Advance(20 * time.Millisecond)
	}
	r.core.flush(t)
	if got := r.clk.Pending(); got != 1 {
		t.Fatalf("%d timers pending after five changes, want the one the first opened", got)
	}
	// 100ms have passed; the window ends at debounce after the first change.
	r.clk.Advance(r.debounce - 100*time.Millisecond)

	got := r.emsp.waitPatches(r.evses() + 1)
	if s := got[r.evses()].status(); s != "CHARGING" {
		t.Errorf("status = %s, want the final CHARGING", s)
	}
	// No second round follows from the changes that joined the first.
	r.core.flush(t)
	r.clk.Advance(time.Hour)
	r.core.flush(t)
	if n := len(r.emsp.got()); n != r.evses()+1 {
		t.Errorf("%d PATCHes, want %d", n, r.evses()+1)
	}
}

// 500, 500, 200: three attempts, each after its backoff, the third succeeds.
func TestRetryBacksOffThenSucceeds(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.emsp.setAnswers(func(n int) answer {
		if n <= 2 {
			return answer{httpStatus: http.StatusInternalServerError}
		}
		return answer{}
	})
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)

	r.pushRound(1)
	r.emsp.waitPatches(1)
	r.waitTimer(1)
	if st := r.pusher.Status(); st.Pending != 1 || !strings.Contains(st.LastError, "500") {
		t.Errorf("after the first failure: %+v", st)
	}

	// The first retry waits 1s: not a nanosecond earlier.
	r.clk.Advance(time.Second - time.Nanosecond)
	r.core.flush(t)
	if n := len(r.emsp.got()); n != 1 {
		t.Fatalf("%d attempts before the first backoff ended, want 1", n)
	}
	r.clk.Advance(time.Nanosecond)
	r.emsp.waitPatches(2)
	r.waitTimer(1)

	// The second waits 2s.
	r.clk.Advance(2*time.Second - time.Nanosecond)
	r.core.flush(t)
	if n := len(r.emsp.got()); n != 2 {
		t.Fatalf("%d attempts before the second backoff ended, want 2", n)
	}
	r.clk.Advance(time.Nanosecond)
	got := r.emsp.waitPatches(3)

	eventually(t, "the push to settle", func() bool { return r.pusher.Status().Pending == 0 })
	if len(got) != 3 {
		t.Errorf("%d attempts, want 3", len(got))
	}
	for i, p := range got {
		if p.status() != "CHARGING" || p.path != r.evsePath(0) {
			t.Errorf("attempt %d: %s %s", i+1, p.path, p.raw)
		}
	}
	if b := r.backoffCalls(); len(b) != 2 || b[0] != 1 || b[1] != 2 {
		t.Errorf("backoff asked for retries %v, want [1 2]", b)
	}
	if st := r.pusher.Status(); !st.LastSuccess.Equal(r.clk.Now()) || st.Pending != 0 {
		t.Errorf("status after success = %+v, want a success at %v", st, r.clk.Now())
	}
	if n := r.logs.count("level=ERROR"); n != 0 {
		t.Errorf("%d error logs for a push that got through:\n%s", n, r.logs.String())
	}
}

// A partner that always answers 500: max_retries retries after the first
// attempt, one error log naming the EVSE and the status, nothing left
// pending, and a later change goes out as usual.
func TestGiveUpAfterMaxRetries(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *config.Config) { c.OCPI.Push.MaxRetries = 3 })
	r.register()
	r.emsp.setAnswers(func(int) answer { return answer{httpStatus: http.StatusInternalServerError} })
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)

	r.pushRound(1)
	for attempt := 1; attempt <= 3; attempt++ {
		r.emsp.waitPatches(attempt)
		r.waitTimer(1)
		r.clk.Advance(time.Minute)
	}
	r.emsp.waitPatches(4)
	eventually(t, "the push to give up", func() bool { return r.pusher.Status().Pending == 0 })
	if n := len(r.emsp.got()); n != 4 {
		t.Errorf("%d attempts, want 1 + 3 retries", n)
	}
	if r.clk.Pending() != 0 {
		t.Errorf("a timer is still pending after giving up")
	}
	if n := r.logs.count("level=ERROR", r.cfg.Location.EVSEs[0].UID, "HTTP 500"); n != 1 {
		t.Errorf("%d error logs naming the EVSE and HTTP 500, want 1:\n%s", n, r.logs.String())
	}
	if n := r.logs.count("level=ERROR"); n != 1 {
		t.Errorf("%d error logs, want 1:\n%s", n, r.logs.String())
	}

	// cpms is fine, and the next change is pushed on its own merits.
	r.emsp.setAnswers(nil)
	r.setStatus(0, core.StatusAvailable)
	r.pushRound(1)
	got := r.emsp.waitPatches(5)
	if got[4].status() != "AVAILABLE" {
		t.Errorf("the later change was pushed as %s", got[4].status())
	}
	eventually(t, "the push to settle", func() bool { return r.pusher.Status().Pending == 0 })
}

// What is retried and what is not. 4xx is the partner refusing the request,
// so retrying cannot help, except 408 and 429, which mean "later". Anything
// that is not an answer, or not a success envelope, is retried.
func TestWhichFailuresAreRetried(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		reply answer
		retry bool
		logs  []string
	}{
		{"400", answer{httpStatus: 400}, false, []string{"HTTP 400"}},
		{"400 with an OCPI code", answer{httpStatus: 400, code: 2001}, false, []string{"HTTP 400", "status_code 2001"}},
		{"401", answer{httpStatus: 401}, false, nil},
		{"404", answer{httpStatus: 404}, false, nil},
		{"408", answer{httpStatus: 408}, true, nil},
		{"429", answer{httpStatus: 429}, true, nil},
		{"500", answer{httpStatus: 500}, true, nil},
		{"503", answer{httpStatus: 503}, true, nil},
		{"200 with a non-1000 envelope", answer{httpStatus: 200, code: 3000}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.register()
			r.emsp.setAnswers(func(int) answer { return tc.reply })
			r.core.Connected(r.cfg.Charger.ID)
			r.setStatus(0, core.StatusCharging)

			r.pushRound(1)
			r.emsp.waitPatches(1)
			if tc.retry {
				r.waitTimer(1)
				if n := r.logs.count("level=ERROR"); n != 0 {
					t.Errorf("an error log before the retries ran out:\n%s", r.logs.String())
				}
				return
			}
			// Exactly one attempt, one error log, nothing scheduled.
			eventually(t, "the push to be abandoned", func() bool { return r.pusher.Status().Pending == 0 })
			if r.clk.Pending() != 0 {
				t.Error("a retry is scheduled after a permanent error")
			}
			if n := len(r.emsp.got()); n != 1 {
				t.Errorf("%d attempts, want 1", n)
			}
			parts := append([]string{"level=ERROR", r.cfg.Location.EVSEs[0].UID}, tc.logs...)
			if n := r.logs.count(parts...); n != 1 {
				t.Errorf("%d error logs containing %q, want 1:\n%s", n, parts, r.logs.String())
			}
		})
	}
}

// A partner that cannot be reached is a failure to retry, not to give up on.
func TestUnreachablePartnerIsRetried(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.emsp.srv.Close()
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)

	r.pushRound(1)
	// The attempt fails without an answer, and a backoff is armed.
	eventually(t, "a backoff after the refused connection", func() bool {
		return r.pusher.Status().LastError != "" && r.clk.Pending() == 1
	})
	if n := r.logs.count("level=ERROR"); n != 0 {
		t.Errorf("an error log before the retries ran out:\n%s", r.logs.String())
	}

	// `cpms status` cuts this at 60 characters, so it has to open with the
	// reason: the client's own error opens with the URL, twice.
	reason := r.pusher.Status().LastError
	if !strings.Contains(reason, "connection refused") {
		t.Errorf("LastError = %q, want the reason (connection refused)", reason)
	}
	if strings.Contains(reason, r.emsp.srv.URL) || strings.Contains(reason, "/locations/") {
		t.Errorf("LastError = %q, want no URL in it", reason)
	}
}

// While backing off after a 500 the EVSE changes again. The retry carries the
// new status, there is no second timer, and the old status is not sent after.
func TestSupersededRetrySendsTheNewStatus(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.emsp.setAnswers(func(n int) answer {
		if n == 1 {
			return answer{httpStatus: http.StatusInternalServerError}
		}
		return answer{}
	})
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)
	r.emsp.waitPatches(1)
	r.waitTimer(1)

	r.clk.Advance(100 * time.Millisecond)
	r.setStatus(0, core.StatusAvailable)
	r.core.flush(t)
	if got := r.clk.Pending(); got != 1 {
		t.Fatalf("%d timers after the change during backoff, want the one backoff", got)
	}

	// The backoff is a fixed window: it ends 1s after the failure, not 1s
	// after the new change.
	r.clk.Advance(time.Second - 100*time.Millisecond)
	got := r.emsp.waitPatches(2)
	if got[1].status() != "AVAILABLE" {
		t.Errorf("the retry carried %s, want the new AVAILABLE", got[1].status())
	}
	eventually(t, "the push to settle", func() bool { return r.pusher.Status().Pending == 0 })
	r.clk.Advance(time.Hour)
	r.core.flush(t)
	got = r.emsp.got()
	if len(got) != 2 {
		t.Fatalf("%d PATCHes, want 2", len(got))
	}
	if got[0].status() != "CHARGING" || got[1].status() != "AVAILABLE" {
		t.Errorf("statuses sent: %s, %s", got[0].status(), got[1].status())
	}
}

// A change while a PATCH is on its way starts another round: the change's
// wake token is not swallowed by the round that was already reading the
// state.
func TestChangeDuringAPatchIsPushedNext(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	release := make(chan struct{})
	r.emsp.setHold(func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	})
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)
	r.emsp.waitPatches(1)

	// The first PATCH is in flight, holding its state: CHARGING.
	r.setStatus(0, core.StatusAvailable)
	r.core.flush(t)
	r.emsp.setHold(nil)
	close(release)

	r.waitTimer(1)
	r.clk.Advance(r.debounce)
	got := r.emsp.waitPatches(2)
	if got[0].status() != "CHARGING" || got[1].status() != "AVAILABLE" {
		t.Errorf("statuses sent: %s, %s", got[0].status(), got[1].status())
	}
}

// Nothing to push to: no HTTP at all, no error, and no goroutines left over.
// Not parallel, so that goleak sees only this test's.
func TestNotRegisteredPushesNothing(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	r := newRig(t)
	r.core.Connected(r.cfg.Charger.ID)
	r.core.flush(t)
	before := runtime.NumGoroutine()

	// One hundred changes, each its own round.
	for i := range 100 {
		status := core.StatusAvailable
		if i%2 == 0 {
			status = core.StatusCharging
		}
		r.setStatus(0, status)
		r.pushRound(1)
		eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })
	}
	// And a burst of one hundred inside one window is one round.
	for i := range 100 {
		status := core.StatusReserved
		if i%2 == 0 {
			status = core.StatusBlocked
		}
		r.setStatus(1, status)
	}
	r.pushRound(1)
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })

	if n := r.emsp.total(); n != 0 {
		t.Errorf("%d HTTP calls while not registered", n)
	}
	if n := r.logs.count("level=ERROR") + r.logs.count("level=WARN"); n != 0 {
		t.Errorf("%d error or warning logs while not registered:\n%s", n, r.logs.String())
	}
	if n := r.logs.count("not registered"); n != 101 {
		t.Errorf("%d debug lines about not being registered, want one per round (101)", n)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines went from %d to %d over 101 rounds", before, after)
	}
}

// A partner that lists no locations receiver is a valid pull-only
// integration: quiet, like not being registered.
func TestNoLocationsReceiverPushesNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.emsp.mu.Lock()
	r.emsp.noLocations = true
	r.emsp.mu.Unlock()
	r.register()
	calls := r.emsp.total()

	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })

	if n := r.emsp.total(); n != calls {
		t.Errorf("%d HTTP calls after registration, want none", n-calls)
	}
	if n := r.logs.count("level=ERROR"); n != 0 {
		t.Errorf("%d error logs:\n%s", n, r.logs.String())
	}
}

// Registration is read at send time: a partner that registers while cpms
// runs gets the next change, and one that unregisters gets none.
func TestRegistrationIsReadAtSendTime(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.core.Connected(r.cfg.Charger.ID)

	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })
	if n := len(r.emsp.got()); n != 0 {
		t.Fatalf("%d PATCHes before registration", n)
	}

	r.register()
	r.setStatus(0, core.StatusAvailable)
	r.pushRound(1)
	got := r.emsp.waitPatches(1)
	if got[0].status() != "AVAILABLE" {
		t.Errorf("first PATCH after registration carried %s", got[0].status())
	}
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })

	r.unregister()
	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })
	if n := len(r.emsp.got()); n != 1 {
		t.Errorf("%d PATCHes after unregistering, want the 1 from before", n)
	}
}

// A debounce armed while registered, firing after the partner unregistered,
// sends nothing: the registration is read when the timer fires.
func TestUnregisterDuringDebounceStopsTheSend(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)
	r.core.flush(t)
	r.waitTimer(1)

	r.unregister()
	r.clk.Advance(r.debounce)
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })
	if n := len(r.emsp.got()); n != 0 {
		t.Errorf("%d PATCHes after unregistering", n)
	}
}

// If the bus lost events for the pusher, every EVSE is pushed once. Drops is
// cumulative, so only a rise counts: the same total seen again is nothing.
func TestDropsPushEveryEVSEOncePerRise(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.core.Connected(r.cfg.Charger.ID)
	r.core.flush(t)
	if r.clk.Pending() != 0 {
		t.Fatal("something is pending before any change")
	}

	r.core.setDrops(1)
	r.pushRound(r.evses())
	got := r.emsp.waitPatches(r.evses())
	paths := map[string]bool{}
	for _, p := range got {
		paths[p.path] = true
	}
	for i := range r.cfg.Location.EVSEs {
		if !paths[r.evsePath(i)] {
			t.Errorf("no PATCH for EVSE %s", r.cfg.Location.EVSEs[i].UID)
		}
	}
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })

	// More events with the total unchanged at 1: nothing.
	r.core.flush(t)
	r.core.flush(t)
	if r.clk.Pending() != 0 {
		t.Error("an old drop count marked the EVSEs dirty again")
	}

	// A further rise is another round.
	r.core.setDrops(2)
	r.pushRound(r.evses())
	r.emsp.waitPatches(2 * r.evses())
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })
	if n := len(r.emsp.got()); n != 2*r.evses() {
		t.Errorf("%d PATCHes, want %d", n, 2*r.evses())
	}
}

// A station that goes away makes every EVSE UNKNOWN in core, one event each,
// and so one PATCH each.
func TestDisconnectPushesUnknownPerEVSE(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.connect()
	r.pushRound(r.evses())
	r.emsp.waitPatches(r.evses())
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })

	r.clk.Advance(time.Minute)
	gone := r.clk.Now()
	r.core.Disconnected(r.cfg.Charger.ID, "test")
	r.pushRound(r.evses())
	r.emsp.waitPatches(2 * r.evses())
	eventually(t, "the round to end", func() bool { return r.pusher.Status().Pending == 0 })

	got := r.emsp.got()
	if len(got) != 2*r.evses() {
		t.Fatalf("%d PATCHes, want %d", len(got), 2*r.evses())
	}
	seen := map[string]bool{}
	for _, p := range got[r.evses():] {
		if p.status() != "UNKNOWN" {
			t.Errorf("%s: status %s, want UNKNOWN", p.path, p.status())
		}
		if want := ocpi.FormatDateTime(gone); p.body["last_updated"] != want {
			t.Errorf("%s: last_updated %v, want the disconnect's %s", p.path, p.body["last_updated"], want)
		}
		if seen[p.path] {
			t.Errorf("two PATCHes for %s", p.path)
		}
		seen[p.path] = true
	}
}

// Events that are not an EVSE status change of the configured station do
// nothing. That includes a station nobody configured whose connector gets a
// made-up uid (core names it "<station>-<connector>") that happens to be the
// uid of an EVSE we do have.
func TestOtherEventsAreIgnored(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *config.Config) { c.Location.EVSEs[0].UID = "ROGUE-1" })
	r.register()
	r.core.Connected(r.cfg.Charger.ID)

	r.core.Connected("ROGUE")
	r.core.SetConnectorStatus("ROGUE", 1, core.StatusCharging, "NoError")
	r.core.SetConnectorStatus(r.cfg.Charger.ID, 99, core.StatusCharging, "NoError")
	r.core.Heartbeat(r.cfg.Charger.ID)
	r.core.flush(t)
	// The pusher has handled them; a worker that was woken arms its timer a
	// moment later, on its own goroutine. There is nothing to wait for when
	// none should have been, so give it that moment.
	time.Sleep(50 * time.Millisecond)
	if r.clk.Pending() != 0 {
		t.Error("an event that is not about a configured EVSE armed a timer")
	}
	if n := len(r.emsp.got()); n != 0 {
		t.Errorf("%d PATCHes", n)
	}
}

// Shutdown cancels a PATCH that is on the wire, within the grace, and does
// not report it as a push failure.
func TestShutdownCancelsAnInFlightPatch(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	cancelled := make(chan struct{})
	r.emsp.setHold(func(ctx context.Context) {
		select {
		case <-ctx.Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	})
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)
	r.pushRound(1)
	r.emsp.waitPatches(1)

	r.cancel()
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := r.pusher.Wait(ctx); err != nil {
		t.Fatalf("the pusher did not stop within the grace: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Error("the in-flight request was not cancelled")
	}
	for _, bad := range []string{"level=ERROR", "level=WARN", "status push failed"} {
		if n := r.logs.count(bad); n != 0 {
			t.Errorf("shutting down logged %s:\n%s", bad, r.logs.String())
		}
	}
	if st := r.pusher.Status(); st.LastError != "" {
		t.Errorf("shutting down was recorded as an error: %q", st.LastError)
	}
}

// Shutdown while a timer is armed stops the worker without sending.
func TestShutdownDuringDebounceSendsNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.register()
	r.core.Connected(r.cfg.Charger.ID)
	r.setStatus(0, core.StatusCharging)
	r.core.flush(t)
	r.waitTimer(1)

	r.cancel()
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := r.pusher.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if r.clk.Pending() != 0 {
		t.Error("the worker left its timer armed")
	}
	if n := len(r.emsp.got()); n != 0 {
		t.Errorf("%d PATCHes after shutdown", n)
	}
}

func TestNewNeedsItsParts(t *testing.T) {
	t.Parallel()
	if _, err := push.New(push.Options{}); err == nil {
		t.Error("New accepted no options")
	}
}
