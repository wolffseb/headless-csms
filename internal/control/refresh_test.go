package control_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// logBuffer collects log output from several goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines returns the logged lines that contain s.
func (l *logBuffer) lines(s string) []string {
	var out []string
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

// refreshRig is control over a fake station, with the boot grace on a fake
// clock, for RefreshStatusOnConnect.
type refreshRig struct {
	clk     *clocktest.Clock
	core    *core.Service
	station *fakeStation
	ctl     *control.Service
	log     *logBuffer
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()

	clk := clocktest.New(t0)
	logs := &logBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := testConfig()
	svc := core.New(cfg, core.WithClock(clk.Now), core.WithLogger(logger))
	t.Cleanup(svc.Close)

	station := newFakeStation()
	ctl := control.New(control.Options{
		Core: svc, Commands: commands{station}, Config: cfg, Now: clk.Now, Log: logger,
		AfterFunc: func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) },
	})
	return &refreshRig{clk: clk, core: svc, station: station, ctl: ctl, log: logs}
}

// connect records a connection in core and starts RefreshStatusOnConnect
// for it, as csms does. The returned channel closes when it returns.
func (r *refreshRig) connect(t *testing.T, ctx context.Context) <-chan struct{} {
	t.Helper()

	r.core.Connected(testCP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ctl.RefreshStatusOnConnect(ctx, r.station)
	}()
	waitUntil(t, "the boot grace to start", func() bool { return r.clk.Pending() == 1 })
	return done
}

func (r *refreshRig) triggers() []string {
	r.station.mu.Lock()
	defer r.station.mu.Unlock()
	return slices.Clone(r.station.triggers)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestRefreshAsksEveryEVSEAfterConnectingWithoutBoot is #53: a station that
// reconnects without rebooting reports nothing, so once the boot grace is
// over every configured EVSE is asked for by its own TriggerMessage.
func TestRefreshAsksEveryEVSEAfterConnectingWithoutBoot(t *testing.T) {
	t.Parallel()

	r := newRefreshRig(t)
	done := r.connect(t, context.Background())

	r.clk.Advance(control.DefaultBootGrace - time.Millisecond)
	if r.clk.Pending() != 1 {
		t.Fatal("the boot grace ended early")
	}
	if got := r.triggers(); len(got) != 0 {
		t.Fatalf("asked for %v before the boot grace was over", got)
	}

	r.clk.Advance(time.Millisecond)
	waitClosed(t, "RefreshStatusOnConnect to return", done)
	if got, want := r.triggers(), []string{"EVSE-1", "EVSE-2"}; !slices.Equal(got, want) {
		t.Errorf("asked for %v, want %v", got, want)
	}
	if lines := r.log.lines("level=INFO"); len(lines) != 1 || !strings.Contains(lines[0], "asked the station for the status") {
		t.Errorf("INFO lines = %q, want one saying the station was asked", lines)
	}
}

// TestRefreshLeavesABootingStationAlone: a station that boots reports every
// status by itself, so no TriggerMessage follows its boot, however soon after
// connecting the boot came.
func TestRefreshLeavesABootingStationAlone(t *testing.T) {
	t.Parallel()

	r := newRefreshRig(t)
	done := r.connect(t, context.Background())

	r.core.Booted(testCP, core.BootInfo{Vendor: "Alpitronic"})
	r.clk.Advance(control.DefaultBootGrace)
	waitClosed(t, "RefreshStatusOnConnect to return", done)

	if got := r.triggers(); len(got) != 0 {
		t.Errorf("asked a station that booted for %v", got)
	}
}

// TestRefreshRacingABoot runs the boot and the end of the grace at the same
// moment. Either outcome is fine, as long as it is all or nothing and the
// race detector stays quiet.
func TestRefreshRacingABoot(t *testing.T) {
	t.Parallel()

	r := newRefreshRig(t)
	done := r.connect(t, context.Background())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.core.Booted(testCP, core.BootInfo{Vendor: "Alpitronic"}) }()
	go func() { defer wg.Done(); r.clk.Advance(control.DefaultBootGrace) }()
	wg.Wait()
	waitClosed(t, "RefreshStatusOnConnect to return", done)

	if got := r.triggers(); len(got) != 0 && len(got) != 2 {
		t.Errorf("asked for %v, want every EVSE or none", got)
	}
}

// TestRefreshStopsWhenTheConnectionEnds: the context ends with the
// connection, and nothing is asked of a station that has gone.
func TestRefreshStopsWhenTheConnectionEnds(t *testing.T) {
	t.Parallel()

	r := newRefreshRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := r.connect(t, ctx)

	cancel()
	waitClosed(t, "RefreshStatusOnConnect to return", done)
	if n := r.clk.Pending(); n != 0 {
		t.Errorf("%d timers left behind", n)
	}
	r.clk.Advance(control.DefaultBootGrace)
	if got := r.triggers(); len(got) != 0 {
		t.Errorf("asked for %v after the connection ended", got)
	}
}

// TestRefreshRefusalIsLoggedOnceAndNotRetried: a station without the
// RemoteTrigger profile says so for the first EVSE; that is logged once at
// INFO, not as a problem, and neither the other EVSEs nor a retry follow.
func TestRefreshRefusalIsLoggedOnceAndNotRetried(t *testing.T) {
	t.Parallel()

	for name, set := range map[string]func(*fakeStation){
		"Rejected": func(f *fakeStation) {
			f.answer["trigger"] = ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}
		},
		"NotImplemented": func(f *fakeStation) {
			f.answer["trigger"] = ocpp.Result{Status: ocpp.CommandNotImplemented, Raw: "NotImplemented"}
		},
		"CALLERROR": func(f *fakeStation) {
			f.fail["trigger"] = &ocpp.RPCError{Code: ocpp.ErrNotImplemented, Description: "TriggerMessage"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newRefreshRig(t)
			set(r.station)
			done := r.connect(t, context.Background())
			r.clk.Advance(control.DefaultBootGrace)
			waitClosed(t, "RefreshStatusOnConnect to return", done)

			if got, want := r.triggers(), []string{"EVSE-1"}; !slices.Equal(got, want) {
				t.Errorf("asked for %v, want only %v", got, want)
			}
			if lines := r.log.lines("will not report its status"); len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") {
				t.Errorf("refusal logged as %q, want once at INFO", lines)
			}
			if lines := append(r.log.lines("level=WARN"), r.log.lines("level=ERROR")...); len(lines) != 0 {
				t.Errorf("a refusal is not a problem, but logged %q", lines)
			}
			if n := r.clk.Pending(); n != 0 {
				t.Errorf("%d timers left behind, as if a retry were scheduled", n)
			}
		})
	}
}

// TestRefreshTimeoutIsNotRetried: a station that does not answer gets one
// WARN, and the remaining EVSEs are not asked, which would only cost another
// call timeout each.
func TestRefreshTimeoutIsNotRetried(t *testing.T) {
	t.Parallel()

	r := newRefreshRig(t)
	r.station.fail["trigger"] = ocpp.ErrTimeout
	done := r.connect(t, context.Background())
	r.clk.Advance(control.DefaultBootGrace)
	waitClosed(t, "RefreshStatusOnConnect to return", done)

	if got, want := r.triggers(), []string{"EVSE-1"}; !slices.Equal(got, want) {
		t.Errorf("asked for %v, want only %v", got, want)
	}
	if lines := r.log.lines("level=WARN"); len(lines) != 1 {
		t.Errorf("WARN lines = %q, want one", lines)
	}
}

// TestRefreshIgnoresAnotherChargePoint: only the configured station's EVSEs
// are known, so a connection under another identity is not asked anything.
func TestRefreshIgnoresAnotherChargePoint(t *testing.T) {
	t.Parallel()

	r := newRefreshRig(t)
	other := &otherStation{r.station}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ctl.RefreshStatusOnConnect(context.Background(), other)
	}()
	waitClosed(t, "RefreshStatusOnConnect to return at once", done)
	if got := r.triggers(); len(got) != 0 {
		t.Errorf("asked another charge point for %v", got)
	}
}

type otherStation struct{ *fakeStation }

func (otherStation) ID() string { return "SOMEONE-ELSE" }
