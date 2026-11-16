package control_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// reconnect cuts the station off, waits for core to see it go, runs offline
// if it is not nil, and lets the station redial. It returns once the new
// connection is up and its OnConnect is waiting out the boot grace.
func reconnect(t *testing.T, r *simRig, p *ocpptest.Proxy, rclk *clocktest.Clock, offline func()) {
	t.Helper()

	r.waitFor(t, "the first connection's boot grace", func() bool { return r.grace.Pending() == 1 })
	p.Cut()
	r.waitFor(t, "the station to go offline", func() bool {
		return !r.core.IsOnline(testCP) && r.status("EVSE-1") == core.StatusUnknown && r.status("EVSE-2") == core.StatusUnknown
	})
	r.waitFor(t, "the first connection's OnConnect to end", func() bool { return r.grace.Pending() == 0 })
	r.waitFor(t, "the redial to be scheduled", func() bool { return rclk.Pending() == 1 })
	if offline != nil {
		offline()
	}
	p.Restore()
	rclk.Advance(time.Second)
	r.waitFor(t, "the station to be back, with OnConnect waiting", func() bool {
		return r.core.IsOnline(testCP) && r.grace.Pending() == 1
	})
}

// TestStatusAskedForAfterAReconnectWithoutBoot is #53 against the simulator:
// a station that redials without booting reports nothing by itself. Core
// then has only what the station said before the outage, which a session
// started offline has made wrong, until cpms asks.
//
// (When cpms itself restarted, core has nothing at all and the EVSEs read
// UNKNOWN; TestRunAsksAResumedStationForItsStatus in internal/cli covers
// that.)
func TestStatusAskedForAfterAReconnectWithoutBoot(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	rclk := clocktest.New(t0)
	r := newSimRig(t, 5*time.Second, behindProxy(t, &p, rclk), func(o *simulator.Options) {
		o.ResumeWithoutBoot = true
	})
	reconnect(t, r, p, rclk, func() {
		if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
			t.Fatalf("offline start: %v", err)
		}
	})
	r.waitFor(t, "the offline session to be delivered", func() bool {
		return len(r.core.ActiveTransactions("EVSE-1")) == 1
	})

	if r.core.BootedSinceConnect(testCP) {
		t.Fatal("the simulator booted on the redial; the test needs a station that resumes")
	}
	if p.Saw(v16.ActionTriggerMessage) {
		t.Fatal("asked for the statuses before the boot grace was over")
	}
	if got := r.status("EVSE-1"); got != core.StatusAvailable {
		t.Fatalf("EVSE-1 = %s before anyone asked, want the stale AVAILABLE", got)
	}

	r.grace.Advance(control.DefaultBootGrace)
	r.waitFor(t, "EVSE-1 to show the session", func() bool { return r.status("EVSE-1") == core.StatusCharging })
	if got := r.status("EVSE-2"); got != core.StatusAvailable {
		t.Errorf("EVSE-2 = %s, want AVAILABLE", got)
	}
	if !p.Saw(v16.ActionTriggerMessage) {
		t.Error("the status arrived without a TriggerMessage")
	}
}

// TestNoStatusRequestAfterAReconnectWithBoot: a station that boots on the
// redial reports every status by itself, and its boot leaves no
// TriggerMessage behind.
func TestNoStatusRequestAfterAReconnectWithBoot(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	rclk := clocktest.New(t0)
	r := newSimRig(t, 5*time.Second, behindProxy(t, &p, rclk))
	reconnect(t, r, p, rclk, nil)
	r.waitFor(t, "the boot's own status round", func() bool {
		return r.status("EVSE-1") == core.StatusAvailable && r.status("EVSE-2") == core.StatusAvailable
	})

	r.grace.Advance(control.DefaultBootGrace)
	// The first connection's OnConnect ended with it; this is the second.
	r.waitFor(t, "the redial's OnConnect to return", func() bool { return r.hooksDone() == 2 })
	if p.Saw(v16.ActionTriggerMessage) {
		t.Error("a TriggerMessage went to a station that had just booted")
	}
}

// TestStationWithoutRemoteTriggerStaysConnected: a station that answers the
// status request with NotImplemented, as a TriggerMessage.conf or as a
// CALLERROR, is asked once, stays connected, and its EVSEs stay UNKNOWN.
func TestStationWithoutRemoteTriggerStaysConnected(t *testing.T) {
	t.Parallel()

	for name, answer := range map[string]func(ocpptest.Call) (any, error){
		"conf": func(ocpptest.Call) (any, error) {
			return map[string]string{"status": v16.TriggerNotImplemented}, nil
		},
		// No handler: the client answers every CALL with a NotImplemented
		// CALLERROR, as a station without the profile may.
		"CALLERROR": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			logs := &logBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, nil))
			cfg := testConfig()
			svc := core.New(cfg, core.WithLogger(logger))
			t.Cleanup(svc.Close)

			grace := clocktest.New(t0)
			var ctl *control.Service
			var done sync.WaitGroup
			server, err := csms.New(csms.Options{
				Bind:     "127.0.0.1:0",
				Core:     svc,
				Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: v16.NewHandler(cfg, svc, logger)},
				NewChargePoint: func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
					return v16.NewChargePoint(conn, cfg), nil
				},
				OnConnect: func(ctx context.Context, cp ocpp.ChargePoint) {
					defer done.Done()
					ctl.RefreshStatusOnConnect(ctx, cp)
				},
				CallTimeout: 5 * time.Second,
				IdleTimeout: 30 * time.Second,
				Log:         logger,
			})
			if err != nil {
				t.Fatalf("csms.New: %v", err)
			}
			ctl = control.New(control.Options{
				Core: svc, Commands: server, Config: cfg, Log: logger,
				AfterFunc: func(d time.Duration, fn func()) core.Stopper { return grace.AfterFunc(d, fn) },
			})
			if err := server.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = server.Shutdown(ctx)
			})

			var mu sync.Mutex
			var asked []json.RawMessage
			done.Add(1)
			client := ocpptest.MustDial(t, "ws://"+server.Addr()+"/ocpp/"+testCP)
			t.Cleanup(client.Close)
			if answer != nil {
				client.OnCall(func(c ocpptest.Call) (any, error) {
					mu.Lock()
					asked = append(asked, c.Payload)
					mu.Unlock()
					return answer(c)
				})
			}

			waitUntil(t, "the boot grace to start", func() bool { return grace.Pending() == 1 })
			grace.Advance(control.DefaultBootGrace)
			hookDone := make(chan struct{})
			go func() { done.Wait(); close(hookDone) }()
			waitClosed(t, "OnConnect to return", hookDone)

			if answer != nil {
				mu.Lock()
				n := len(asked)
				mu.Unlock()
				if n != 1 {
					t.Errorf("station was asked %d times, want once", n)
				}
			}
			// The connection is still usable, and nothing about it changed.
			client.MustCall(v16.ActionHeartbeat, struct{}{})
			select {
			case <-client.Closed():
				t.Fatal("the connection was closed")
			default:
			}
			if !svc.IsOnline(testCP) {
				t.Error("station went offline")
			}
			for _, uid := range []string{"EVSE-1", "EVSE-2"} {
				if st, _ := svc.EVSEStatus(uid); st != core.StatusUnknown {
					t.Errorf("%s = %s, want UNKNOWN", uid, st)
				}
			}
			if lines := logs.lines("will not report its status"); len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") {
				t.Errorf("refusal logged as %q, want once at INFO", lines)
			}
		})
	}
}
