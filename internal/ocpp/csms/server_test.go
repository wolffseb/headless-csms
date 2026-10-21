package csms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

const testCP = "ALP-HYC-001"

// stubHandler stands in for a version adapter. These tests are about
// transport, framing and correlation, so the handler only needs enough
// behaviour to exercise each response path.
type stubHandler struct{}

func (stubHandler) Version() ocpp.Version { return ocpp.Version16 }

func (stubHandler) HandleCall(_ context.Context, _, action string, payload json.RawMessage) (any, *ocpp.RPCError) {
	switch action {
	case "Echo":
		return map[string]any{"echo": json.RawMessage(payload)}, nil
	case "Boom":
		panic("handler exploded")
	case "Bad":
		return nil, ocpp.Errorf(ocpp.ErrFormationViolation, "payload does not fit")
	default:
		return nil, ocpp.Errorf(ocpp.ErrNotImplemented, "action %q is not implemented", action)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Charger: config.Charger{ID: testCP},
		Location: config.Location{
			EVSEs: []config.EVSE{{UID: "EVSE-1", OCPPConnectorID: 1}},
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type harness struct {
	*Server
	core *core.Service
}

// url is the WebSocket address for a charge point identity.
func (h *harness) url(id string) string { return "ws://" + h.Addr() + "/ocpp/" + id }

func newHarness(t *testing.T, mutate ...func(*Options)) *harness {
	t.Helper()

	svc := core.New(testConfig())
	opts := Options{
		Bind:        "127.0.0.1:0",
		Core:        svc,
		Handlers:    map[ocpp.Version]ocpp.Handler{ocpp.Version16: stubHandler{}},
		CallTimeout: 2 * time.Second,
		IdleTimeout: 30 * time.Second,
		Log:         discardLogger(),
	}
	for _, m := range mutate {
		m(&opts)
	}

	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	return &harness{Server: srv, core: svc}
}

// waitFor polls until cond holds, so tests do not race the server's
// asynchronous bookkeeping.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHandshakeAcceptsOCPP16(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	client, hs, err := ocpptest.Dial(t, h.url(testCP), "ocpp1.6")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// The server must echo exactly the subprotocol it selected.
	if got := hs.Subprotocol; got != "ocpp1.6" {
		t.Errorf("negotiated subprotocol = %q, want %q", got, "ocpp1.6")
	}
	waitFor(t, "the charge point to register", func() bool { return h.core.IsOnline(testCP) })
}

func TestHandshakeRejectsWithoutACommonVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	tests := []struct {
		name         string
		subprotocols []string
	}{
		{"none offered", nil},
		{"only a version we do not speak", []string{"ocpp2.0.1"}},
		{"an unrelated subprotocol", []string{"chat"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, hs, err := ocpptest.Dial(t, h.url(testCP), tt.subprotocols...)
			if err == nil {
				t.Fatal("expected the handshake to fail")
			}
			if hs.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", hs.StatusCode, http.StatusBadRequest)
			}
			// OCPP-J requires no subprotocol header on failure; that is what
			// tells the charger to try a different version.
			if got := hs.Subprotocol; got != "" {
				t.Errorf("failed handshake echoed subprotocol %q", got)
			}
		})
	}
}

func TestHandshakeRejectsUnknownChargePoint(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(o *Options) {
		o.Accept = func(id string) bool { return id == testCP }
	})

	_, hs, err := ocpptest.Dial(t, h.url("SOMEONE-ELSE"), "ocpp1.6")
	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if hs.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", hs.StatusCode, http.StatusNotFound)
	}
}

func TestChargePointIDComesFromTheLastPathSegment(t *testing.T) {
	t.Parallel()

	// Vendors disagree about the prefix, so any of these must work.
	paths := []string{
		"/ocpp/" + testCP,
		"/" + testCP,
		"/steve/websocket/CentralSystemService/" + testCP,
		"/ocpp/" + testCP + "/",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, func(o *Options) {
				o.Accept = func(id string) bool { return id == testCP }
			})

			client, _, err := ocpptest.Dial(t, "ws://"+h.Addr()+p, "ocpp1.6")
			if err != nil {
				t.Fatalf("dial %s: %v", p, err)
			}
			defer client.Close()

			waitFor(t, "registration", func() bool { return h.core.IsOnline(testCP) })
		})
	}
}

func TestConnectionWithoutAnIDIsRejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, hs, err := ocpptest.Dial(t, "ws://"+h.Addr()+"/", "ocpp1.6")
	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if hs.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", hs.StatusCode, http.StatusNotFound)
	}
}

func TestUnknownActionAnswersNotImplemented(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	resp := client.CallExpectingError("NoSuchAction", map[string]any{})
	if resp.ErrorCode != string(ocpp.ErrNotImplemented) {
		t.Errorf("code = %s, want %s", resp.ErrorCode, ocpp.ErrNotImplemented)
	}
}

func TestHandlerErrorBecomesCallError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	resp := client.CallExpectingError("Bad", map[string]any{})
	if resp.ErrorCode != string(ocpp.ErrFormationViolation) {
		t.Errorf("code = %s, want %s", resp.ErrorCode, ocpp.ErrFormationViolation)
	}
}

func TestHandlerPanicBecomesInternalError(t *testing.T) {
	t.Parallel()

	// One bad message must not take the process down, and the connection has
	// to survive it.
	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	resp := client.CallExpectingError("Boom", map[string]any{})
	if resp.ErrorCode != string(ocpp.ErrInternalError) {
		t.Errorf("code = %s, want %s", resp.ErrorCode, ocpp.ErrInternalError)
	}

	// The connection must still work afterwards.
	client.MustCall("Echo", map[string]any{"still": "here"})
}

func TestMalformedFrameAnswersRpcFrameworkError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	if err := client.SendRaw([]byte(`this is not json`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, ok := client.NextUnsolicited(3 * time.Second)
	if !ok {
		t.Fatal("expected a CALLERROR for a malformed frame")
	}
	if resp.ErrorCode != string(ocpp.ErrRpcFrameworkError) {
		t.Errorf("code = %s, want %s", resp.ErrorCode, ocpp.ErrRpcFrameworkError)
	}
	if resp.ID != ocppj.UnknownMessageID {
		t.Errorf("reply id = %q, want %q when the id could not be read", resp.ID, ocppj.UnknownMessageID)
	}

	// A malformed frame must not kill the connection.
	client.MustCall("Echo", map[string]any{})
}

func TestResponseForUnknownMessageIDIsIgnored(t *testing.T) {
	t.Parallel()

	// A charger answering twice, or late, must not be able to bring the
	// connection down.
	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	if err := client.SendRaw([]byte(`[3,"no-such-call",{"status":"Accepted"}]`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	client.MustCall("Echo", map[string]any{"alive": true})
}

func TestOutboundCallsAreCorrelated(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	// Answer every call by echoing back the number we were given, so a
	// mismatched correlation shows up as a wrong value rather than a hang.
	client.OnCall(func(c ocpptest.Call) (any, error) {
		var req struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(c.Payload, &req); err != nil {
			return nil, err
		}
		return map[string]int{"n": req.N}, nil
	})

	waitFor(t, "the connection to register", func() bool {
		_, ok := h.ChargePoint(testCP)
		return ok
	})
	conn, _ := h.ChargePoint(testCP)

	const calls = 50
	var wg sync.WaitGroup
	errs := make(chan error, calls)

	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()

			raw, err := conn.Call(context.Background(), "Ping", map[string]int{"n": n})
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", n, err)
				return
			}
			var got struct {
				N int `json:"n"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				errs <- fmt.Errorf("call %d: %w", n, err)
				return
			}
			if got.N != n {
				errs <- fmt.Errorf("call %d got answer for %d", n, got.N)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := conn.PendingCount(); got != 0 {
		t.Errorf("%d pending entries left behind, want 0", got)
	}
}

func TestOutboundCallTimesOutWithoutLeaking(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(o *Options) { o.CallTimeout = 100 * time.Millisecond })
	client := ocpptest.MustDial(t, h.url(testCP))

	// A charger that receives the call and never answers.
	client.OnCall(func(ocpptest.Call) (any, error) {
		time.Sleep(2 * time.Second)
		return map[string]any{}, nil
	})

	waitFor(t, "the connection to register", func() bool {
		_, ok := h.ChargePoint(testCP)
		return ok
	})
	conn, _ := h.ChargePoint(testCP)

	before := runtime.NumGoroutine()

	_, err := conn.Call(context.Background(), "Ping", map[string]any{})
	if err == nil {
		t.Fatal("expected a timeout")
	}

	if got := conn.PendingCount(); got != 0 {
		t.Errorf("%d pending entries left after a timeout, want 0", got)
	}
	// A timed-out call must not leave a goroutine behind.
	waitFor(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= before+1 })
}

func TestOutboundCallHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))
	client.OnCall(func(ocpptest.Call) (any, error) {
		time.Sleep(2 * time.Second)
		return map[string]any{}, nil
	})

	waitFor(t, "the connection to register", func() bool {
		_, ok := h.ChargePoint(testCP)
		return ok
	})
	conn, _ := h.ChargePoint(testCP)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	if _, err := conn.Call(ctx, "Ping", map[string]any{}); err == nil {
		t.Fatal("expected cancellation to end the call")
	}
	if got := conn.PendingCount(); got != 0 {
		t.Errorf("%d pending entries left after cancellation, want 0", got)
	}
}

func TestOutboundCallAfterDisconnect(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	client := ocpptest.MustDial(t, h.url(testCP))

	waitFor(t, "the connection to register", func() bool {
		_, ok := h.ChargePoint(testCP)
		return ok
	})
	conn, _ := h.ChargePoint(testCP)

	client.Close()
	waitFor(t, "the server to notice", func() bool { return !h.core.IsOnline(testCP) })

	if _, err := conn.Call(context.Background(), "Ping", map[string]any{}); err == nil {
		t.Fatal("expected calling a closed connection to fail")
	}
}

// stayOnline fails the test if the station goes offline within d.
func stayOnline(t *testing.T, h *harness, d time.Duration) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if !h.core.IsOnline(testCP) {
			t.Fatalf("station went offline with %s of the observation window left", time.Until(end))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// goesOffline waits up to idle+5s for the station to be marked offline and
// returns how long that took.
func goesOffline(t *testing.T, h *harness, idle time.Duration) time.Duration {
	t.Helper()
	// A budget of its own: the shared waitFor caps at 3s, which would race the
	// timeout under test rather than observe it.
	start := time.Now()
	deadline := start.Add(idle + 5*time.Second)
	for h.core.IsOnline(testCP) {
		if time.Now().After(deadline) {
			t.Fatalf("station still online %s after connecting", time.Since(start))
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

// Above the one-second ping floor, so pings really go out, yet short enough to
// keep the tests quick.
const idleWithPings = 2 * time.Second

// TestSilentStationIsDropped: a station that answers neither OCPP nor pings is
// marked offline after the timeout, and not much before it.
func TestSilentStationIsDropped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(o *Options) { o.IdleTimeout = idleWithPings })
	client := ocpptest.MustDialIgnoringPings(t, h.url(testCP))
	defer client.Close()

	waitFor(t, "the charge point to register", func() bool { return h.core.IsOnline(testCP) })

	if elapsed := goesOffline(t, h, idleWithPings); elapsed < idleWithPings/2 {
		t.Errorf("dropped after %s, well before the %s timeout", elapsed, idleWithPings)
	}
}

// TestPongsKeepAQuietStationOnline is the rey case: it heartbeats every 300s
// regardless of the interval we hand out, but its WebSocket stack answers our
// pings. That is a live station and must stay online well past the timeout.
func TestPongsKeepAQuietStationOnline(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(o *Options) { o.IdleTimeout = idleWithPings })
	client := ocpptest.MustDial(t, h.url(testCP))
	defer client.Close()

	waitFor(t, "the charge point to register", func() bool { return h.core.IsOnline(testCP) })
	client.MustCall("Echo", map[string]any{}) // its one heartbeat; the next is 300s away

	stayOnline(t, h, 2*idleWithPings+time.Second)
}

// TestStationPingsKeepTheConnectionAlive covers WebSocketPingInterval: a
// station that pings us, and ignores ours, is alive.
func TestStationPingsKeepTheConnectionAlive(t *testing.T) {
	t.Parallel()

	const idle = 600 * time.Millisecond // below the ping floor: our pings play no part
	h := newHarness(t, func(o *Options) { o.IdleTimeout = idle })
	client := ocpptest.MustDialIgnoringPings(t, h.url(testCP))
	defer client.Close()

	waitFor(t, "the charge point to register", func() bool { return h.core.IsOnline(testCP) })

	end := time.Now().Add(3 * idle)
	for time.Now().Before(end) {
		if err := client.Ping(); err != nil {
			t.Fatalf("ping: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		if !h.core.IsOnline(testCP) {
			t.Fatal("a station sending WebSocket pings was dropped as idle")
		}
	}
}

// TestAnswersToOurCallsKeepTheConnectionAlive: CALLRESULTs and CALLERRORs are
// inbound frames too. The station ignores pings, so the answers are the only
// thing that can keep it alive.
func TestAnswersToOurCallsKeepTheConnectionAlive(t *testing.T) {
	t.Parallel()

	for name, answer := range map[string]func(ocpptest.Call) (any, error){
		"CALLRESULT": func(ocpptest.Call) (any, error) { return map[string]any{}, nil },
		"CALLERROR":  func(ocpptest.Call) (any, error) { return nil, errors.New("no") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const idle = 600 * time.Millisecond
			h := newHarness(t, func(o *Options) { o.IdleTimeout = idle })
			client := ocpptest.MustDialIgnoringPings(t, h.url(testCP))
			defer client.Close()
			client.OnCall(answer)

			waitFor(t, "the charge point to register", func() bool { return h.core.IsOnline(testCP) })
			conn, ok := h.ChargePoint(testCP)
			if !ok {
				t.Fatal("not connected")
			}

			end := time.Now().Add(3 * idle)
			for time.Now().Before(end) {
				_, _ = conn.Call(context.Background(), "Ping", map[string]any{})
				time.Sleep(100 * time.Millisecond)
				if !h.core.IsOnline(testCP) {
					t.Fatalf("a station answering with a %s was dropped as idle", name)
				}
			}
		})
	}
}

func TestTrafficKeepsTheConnectionAlive(t *testing.T) {
	t.Parallel()

	// Any inbound message counts as a sign of life, not only Heartbeat.
	h := newHarness(t, func(o *Options) { o.IdleTimeout = 400 * time.Millisecond })
	client := ocpptest.MustDialIgnoringPings(t, h.url(testCP))
	defer client.Close()

	waitFor(t, "the charge point to register", func() bool { return h.core.IsOnline(testCP) })

	for i := 0; i < 6; i++ {
		time.Sleep(100 * time.Millisecond)
		client.MustCall("Echo", map[string]any{"i": i})
	}
	if !h.core.IsOnline(testCP) {
		t.Error("a charge point sending traffic was dropped as idle")
	}
}

func TestReconnectReplacesTheOldConnection(t *testing.T) {
	t.Parallel()

	// A charger that reconnects before we noticed the old socket die must not
	// leave a stale connection registered.
	h := newHarness(t)

	first := ocpptest.MustDial(t, h.url(testCP))
	waitFor(t, "the first connection", func() bool {
		_, ok := h.ChargePoint(testCP)
		return ok
	})
	firstConn, _ := h.ChargePoint(testCP)

	second := ocpptest.MustDial(t, h.url(testCP))
	defer second.Close()

	waitFor(t, "the old connection to be closed", func() bool {
		select {
		case <-firstConn.Done():
			return true
		default:
			return false
		}
	})
	waitFor(t, "the new connection to take over", func() bool {
		conn, ok := h.ChargePoint(testCP)
		return ok && conn != firstConn
	})

	// The replacement must be usable, and the station still online.
	second.MustCall("Echo", map[string]any{})
	if !h.core.IsOnline(testCP) {
		t.Error("station went offline after a reconnect")
	}
	first.Close()
}

func TestConcurrentChargePoints(t *testing.T) {
	t.Parallel()

	// This is what justifies the single-mutex-plus-snapshot design in core and
	// the one-writer-goroutine design in conn: run it under -race.
	const points = 20
	const callsEach = 10

	h := newHarness(t, func(o *Options) { o.Accept = nil })

	events, unsubscribe := h.core.Subscribe("test")
	defer unsubscribe()
	go func() {
		for range events {
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < points; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()

			id := fmt.Sprintf("CP-%02d", n)
			// Dial registers a t.Cleanup close; closing here instead would
			// disconnect every charge point before the assertion below.
			client, _, err := ocpptest.Dial(t, h.url(id), "ocpp1.6")
			if err != nil {
				t.Errorf("dial %s: %v", id, err)
				return
			}

			for c := 0; c < callsEach; c++ {
				if _, err := client.Call("Echo", map[string]any{"c": c}); err != nil {
					t.Errorf("%s call %d: %v", id, c, err)
					return
				}
			}
			// Reading the snapshot concurrently with all of this is the point.
			_ = h.core.Snapshot()
		}(i)
	}
	wg.Wait()

	waitFor(t, "all charge points to register", func() bool {
		return len(h.Connected()) == points
	})
}

func TestShutdownClosesConnections(t *testing.T) {
	t.Parallel()

	// Not using newHarness: this test owns the shutdown.
	svc := core.New(testConfig())
	srv, err := New(Options{
		Bind:        "127.0.0.1:0",
		Core:        svc,
		Handlers:    map[ocpp.Version]ocpp.Handler{ocpp.Version16: stubHandler{}},
		CallTimeout: time.Second,
		IdleTimeout: 30 * time.Second,
		Log:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	before := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		client := ocpptest.MustDial(t, "ws://"+srv.Addr()+"/ocpp/"+fmt.Sprintf("CP-%d", i))
		defer client.Close()
	}
	waitFor(t, "connections to register", func() bool { return len(srv.Connected()) == 5 })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if got := len(srv.Connected()); got != 0 {
		t.Errorf("%d connections still registered after shutdown", got)
	}
	// http.Server.Shutdown does not wait for hijacked connections, so this is
	// the assertion that the wait group actually does its job.
	waitFor(t, "server goroutines to exit", func() bool {
		return runtime.NumGoroutine() <= before+5
	})
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{Bind: "127.0.0.1:0"}); err == nil {
		t.Error("expected an error without a core service")
	}
	if _, err := New(Options{Bind: "127.0.0.1:0", Core: core.New(testConfig())}); err == nil {
		t.Error("expected an error without any handler")
	}
}
