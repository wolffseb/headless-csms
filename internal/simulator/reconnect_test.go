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

	// Core restores each EVSE's last status the moment the socket is up
	// (Connected), before the simulator's BootNotification has been handled,
	// so "online and available" says nothing about the boot. Wait for the
	// boot event itself.
	boots := 0
	timeout := time.After(3 * time.Second)
	for boots == 0 {
		select {
		case e := <-events:
			if e.Kind == core.EventBootNotified {
				boots++
			}
		case <-timeout:
			t.Fatal("no BootNotification after the reconnect")
		}
	}
	waitFor(t, "the station to come back", func() bool {
		return r.core.IsOnline(testCP) && r.core.BootedSinceConnect(testCP) && evsesAre(r, core.StatusAvailable)
	})
	// A duplicate boot on this connection would be queued by now.
	for drained := false; !drained; {
		select {
		case e := <-events:
			if e.Kind == core.EventBootNotified {
				boots++
			}
		default:
			drained = true
		}
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

// TestResumeWithoutBootSkipsTheBootAndTheStatusRound: with ResumeWithoutBoot
// a redial is a station that kept running, as an Alpitronic HYC50 does
// (#53). It sends what it queued offline, but no BootNotification and no
// status round, so the CSMS learns nothing new about its connectors until it
// asks. The first Connect still boots. Not parallel, for goleak.
func TestResumeWithoutBootSkipsTheBootAndTheStatusRound(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 1)
	p := ocpptest.NewProxy(t, strings.TrimPrefix(f.url(), "ws://"))
	clk := clocktest.New(time.Date(2026, 11, 16, 16, 23, 0, 0, time.UTC))
	b := &backoffLog{wait: time.Second}
	sim := startSim(t, p.URL(), reconnecting(clk, b), func(o *simulator.Options) { o.ResumeWithoutBoot = true })

	waitFor(t, "the first boot and status round", func() bool {
		return f.count(v16.ActionBootNotification) == 1 && f.count(v16.ActionStatusNotification) == 3
	})

	p.Cut()
	waitFor(t, "the redial to be scheduled", func() bool { return clk.Pending() == 1 })
	// A session started offline changes connector 1, and the station only
	// queues its transaction messages, not the status.
	if err := sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("offline start: %v", err)
	}
	p.Restore()
	clk.Advance(time.Second)

	waitFor(t, "the queued StartTransaction", func() bool { return f.count(v16.ActionStartTransaction) == 1 })
	// A status round would follow the queue at once; give it the chance.
	time.Sleep(200 * time.Millisecond)
	if n := f.count(v16.ActionBootNotification); n != 1 {
		t.Errorf("%d BootNotifications, want only the first connection's", n)
	}
	if n := f.count(v16.ActionStatusNotification); n != 3 {
		t.Errorf("%d StatusNotifications, want only the first connection's 3", n)
	}

	// The resumed connection is a working one: asked, the station reports.
	one := 1
	payload, _ := json.Marshal(v16.TriggerMessageReq{RequestedMessage: v16.ActionStatusNotification, ConnectorID: &one})
	if _, rpcErr := sim.HandleCall(context.Background(), "", v16.ActionTriggerMessage, payload); rpcErr != nil {
		t.Fatalf("TriggerMessage: %v", rpcErr)
	}
	waitFor(t, "the triggered status", func() bool { return f.count(v16.ActionStatusNotification) == 4 })
	calls, _ := f.snapshot()
	var last v16.StatusNotificationReq
	for _, c := range calls {
		if c.action == v16.ActionStatusNotification {
			_ = json.Unmarshal(c.payload, &last)
		}
	}
	if last.ConnectorID != 1 || last.Status != "Charging" {
		t.Errorf("triggered status = connector %d %s, want connector 1 Charging", last.ConnectorID, last.Status)
	}
}
