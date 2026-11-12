package simulator_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// backoffLog records the attempts the simulator asked for a wait before,
// and answers every one with the same wait.
type backoffLog struct {
	wait time.Duration

	mu       sync.Mutex
	attempts []int
}

func (b *backoffLog) backoff(attempt int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts = append(b.attempts, attempt)
	return b.wait
}

func (b *backoffLog) seen() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.attempts...)
}

// reconnecting sets the options for a simulator that redials on clk.
func reconnecting(clk *clocktest.Clock, b *backoffLog) func(*simulator.Options) {
	return func(o *simulator.Options) {
		o.Reconnect = true
		o.ReconnectBackoff = b.backoff
		o.ReconnectAfterFunc = func(d time.Duration, fn func()) simulator.Stopper { return clk.AfterFunc(d, fn) }
	}
}

// startSim connects a simulator with the given options and stops it when the
// test ends.
func startSim(t *testing.T, url string, mutate ...func(*simulator.Options)) *simulator.Simulator {
	t.Helper()

	opts := simulator.Options{URL: url, ID: testCP, Connectors: 2, IDTag: testTag, Log: discard()}
	for _, m := range mutate {
		m(&opts)
	}
	sim, err := simulator.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sim.Close()
		sim.Wait()
	})
	if err := sim.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return sim
}

// statusesAfter lists the connector ids of the StatusNotifications sent after
// the nth BootNotification.
func statusesAfter(t *testing.T, calls []fakeCall, boot int) []int {
	t.Helper()

	var ids []int
	boots := 0
	for _, c := range calls {
		switch c.action {
		case v16.ActionBootNotification:
			boots++
		case v16.ActionStatusNotification:
			if boots < boot {
				continue
			}
			var req v16.StatusNotificationReq
			if err := json.Unmarshal(c.payload, &req); err != nil {
				t.Fatalf("decoding StatusNotification: %v", err)
			}
			ids = append(ids, req.ConnectorID)
		}
	}
	return ids
}

// TestReconnectBootsAndReportsAgain cuts the connection and keeps it cut for
// one failed redial: the simulator must back off on the injected clock, ask
// for attempt 1 and then 2, and once the CSMS is reachable again boot again
// and report the station and every connector again. Not parallel, for
// goleak.
func TestReconnectBootsAndReportsAgain(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 1)
	p := ocpptest.NewProxy(t, strings.TrimPrefix(f.url(), "ws://"))
	clk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	b := &backoffLog{wait: 5 * time.Second}
	sim := startSim(t, p.URL(), reconnecting(clk, b))

	waitFor(t, "the first status round", func() bool { return f.count(v16.ActionStatusNotification) == 3 })

	p.Cut()
	waitFor(t, "the first redial to be scheduled", func() bool { return clk.Pending() == 1 })
	select {
	case <-sim.Done():
		t.Fatal("the simulator stopped on a dropped connection despite Reconnect")
	default:
	}

	// Still cut: this redial fails, and the next one waits again.
	clk.Advance(5 * time.Second)
	waitFor(t, "the second redial to be scheduled", func() bool { return len(b.seen()) == 2 && clk.Pending() == 1 })

	p.Restore()
	clk.Advance(5 * time.Second)
	waitFor(t, "a second boot", func() bool { return f.count(v16.ActionBootNotification) == 2 })
	waitFor(t, "the second status round", func() bool { return f.count(v16.ActionStatusNotification) == 6 })

	if got := b.seen(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("backoff attempts = %v, want [1 2]", got)
	}
	calls, _ := f.snapshot()
	if got := statusesAfter(t, calls, 2); len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Errorf("status after the reconnect for connectors %v, want [0 1 2]", got)
	}

	// The new connection is a working one: commands and local sessions use it.
	if err := sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start after reconnect: %v", err)
	}
	if got := sim.TransactionID(1); got != 1 {
		t.Errorf("transaction after reconnect = %d, want the CSMS's 1", got)
	}

	// A second drop starts the count again.
	p.Cut()
	waitFor(t, "a redial after the second drop", func() bool { return len(b.seen()) == 3 })
	if got := b.seen()[2]; got != 1 {
		t.Errorf("first attempt after the second drop = %d, want 1", got)
	}
}

// TestReconnectRejectedBootStops keeps the old behaviour for a rejected boot:
// the simulator gives up, and Run says why. Not parallel, for goleak.
func TestReconnectRejectedBootStops(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 1)
	f.mu.Lock()
	f.bootStatus = func(n int) string {
		if n > 1 {
			return "Rejected"
		}
		return "Accepted"
	}
	f.mu.Unlock()

	clk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	opts := simulator.Options{URL: f.url(), ID: testCP, IDTag: testTag, Log: discard()}
	reconnecting(clk, &backoffLog{wait: time.Second})(&opts)
	sim, err := simulator.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ran := make(chan error, 1)
	go func() { ran <- sim.Run(context.Background()) }()
	waitFor(t, "the first boot", func() bool { return f.count(v16.ActionStatusNotification) == 2 })

	f.mu.Lock()
	_ = f.conn.Close()
	f.mu.Unlock()
	waitFor(t, "the redial to be scheduled", func() bool { return clk.Pending() == 1 })
	clk.Advance(time.Second)

	select {
	case err := <-ran:
		if err == nil || !strings.Contains(err.Error(), "Rejected") {
			t.Errorf("Run = %v, want the rejected boot", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the reconnect was rejected")
	}
	if n := f.count(v16.ActionBootNotification); n != 2 {
		t.Errorf("%d boots, want 2: one rejection is final", n)
	}
}

// TestReconnectEndsCleanly stops a simulator that is waiting to redial, once
// by Close and once by cancelling its context: no goroutine may stay behind
// in either case. Not parallel, for goleak.
func TestReconnectEndsCleanly(t *testing.T) {
	for _, how := range []string{"close", "cancel"} {
		t.Run(how, func(t *testing.T) {
			ignore := goleak.IgnoreCurrent()
			t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

			f := newFakeCSMS(t, 0, 1)
			p := ocpptest.NewProxy(t, strings.TrimPrefix(f.url(), "ws://"))
			clk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
			opts := simulator.Options{URL: p.URL(), ID: testCP, IDTag: testTag, Log: discard()}
			reconnecting(clk, &backoffLog{wait: time.Hour})(&opts)
			sim, err := simulator.New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := sim.Connect(ctx); err != nil {
				t.Fatalf("connect: %v", err)
			}

			p.Cut()
			waitFor(t, "the redial to be scheduled", func() bool { return clk.Pending() == 1 })

			if how == "close" {
				sim.Close()
			} else {
				cancel()
			}
			waited := make(chan struct{})
			go func() { sim.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-time.After(3 * time.Second):
				t.Fatalf("a simulator goroutine outlived %s", how)
			}
			if n := clk.Pending(); n != 0 {
				t.Errorf("%d redial timers still armed", n)
			}
			sim.Close()
		})
	}
}

// TestReconnectAgainstTheCSMS is the reconnect against the real CSMS and
// core: after a drop, core sees the station go and come back, booted again,
// with every EVSE reported again.
func TestReconnectAgainstTheCSMS(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	clk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	r := newRig(t, reconnecting(clk, &backoffLog{wait: time.Second}), func(o *simulator.Options) {
		p = ocpptest.NewProxy(t, strings.TrimPrefix(o.URL, "ws://"))
		o.URL = p.URL()
	})
	waitFor(t, "both EVSEs to report available", func() bool { return evsesAre(r, core.StatusAvailable) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	p.Cut()
	waitFor(t, "core to see the station go", func() bool { return !r.core.IsOnline(testCP) && evsesAre(r, core.StatusUnknown) })
	waitFor(t, "the redial to be scheduled", func() bool { return clk.Pending() == 1 })
	p.Restore()
	clk.Advance(time.Second)

	waitFor(t, "the station to come back", func() bool { return r.core.IsOnline(testCP) && evsesAre(r, core.StatusAvailable) })
	boots := 0
	for {
		select {
		case e := <-events:
			if e.Kind == core.EventBootNotified {
				boots++
			}
			continue
		default:
		}
		break
	}
	if boots != 1 {
		t.Errorf("%d BootNotifications after the reconnect, want 1", boots)
	}
}

func evsesAre(r *rig, want core.EVSEStatus) bool {
	a, _ := r.core.EVSEStatus("EVSE-1")
	b, _ := r.core.EVSEStatus("EVSE-2")
	return a == want && b == want
}
