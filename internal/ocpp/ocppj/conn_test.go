package ocppj

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

type nopHandler struct{}

func (nopHandler) Version() ocpp.Version { return ocpp.Version16 }

func (nopHandler) HandleCall(context.Context, string, string, json.RawMessage) (any, *ocpp.RPCError) {
	return nil, nil
}

// silentPeer returns a Conn whose peer reads every frame and never answers.
func silentPeer(t *testing.T, callTimeout time.Duration) *Conn {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn := New(ws, Options{
		ID:          "CP",
		Version:     ocpp.Version16,
		CallTimeout: callTimeout,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(context.Background(), nopHandler{}) }()
	t.Cleanup(func() { conn.Close(); <-done })
	return conn
}

func TestCallTimeoutMatchesErrTimeout(t *testing.T) {
	t.Parallel()

	conn := silentPeer(t, 50*time.Millisecond)

	_, err := conn.Call(context.Background(), "UnlockConnector", map[string]any{})
	if !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("err = %v, want one matching ocpp.ErrTimeout", err)
	}
	// The message is what lands in a log; it must still say what timed out.
	if want := "UnlockConnector: no answer within 50ms"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if errors.Is(err, ocpp.ErrNotConnected) {
		t.Error("a timeout must not also claim the charge point is gone")
	}
}

func TestCallOnClosedConnMatchesErrNotConnected(t *testing.T) {
	t.Parallel()

	conn := silentPeer(t, time.Second)
	conn.Close()

	_, err := conn.Call(context.Background(), "UnlockConnector", map[string]any{})
	if !errors.Is(err, ErrConnClosed) {
		t.Fatalf("err = %v, want ErrConnClosed", err)
	}
	if !errors.Is(err, ocpp.ErrNotConnected) {
		t.Errorf("ErrConnClosed does not match ocpp.ErrNotConnected")
	}
	if errors.Is(err, ocpp.ErrTimeout) {
		t.Error("a closed connection must not look like a timeout")
	}
}

// wirePeer is a charge point that records every CALL frame it receives and
// answers only when the test tells it to, so a test sees exactly what is on
// the wire and when.
type wirePeer struct {
	t      *testing.T
	conn   *Conn
	frames chan Frame

	mu sync.Mutex
	ws *websocket.Conn
}

func newWirePeer(t *testing.T, callTimeout time.Duration, tune ...func(*Options)) *wirePeer {
	t.Helper()

	p := &wirePeer{t: t, frames: make(chan Frame, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.ws = ws
		p.mu.Unlock()
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			f, _, rpcErr := ParseFrame(data)
			if rpcErr != nil {
				t.Errorf("peer got a malformed frame: %v", rpcErr)
				return
			}
			p.frames <- f
		}
	}))
	t.Cleanup(srv.Close)

	ws, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	opts := Options{
		ID:          "CP",
		Version:     ocpp.Version16,
		CallTimeout: callTimeout,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range tune {
		m(&opts)
	}
	p.conn = New(ws, opts)
	done := make(chan struct{})
	go func() { defer close(done); p.conn.Run(context.Background(), nopHandler{}) }()
	t.Cleanup(func() { p.conn.Close(); <-done })
	return p
}

// next waits for the next CALL frame the peer receives.
func (p *wirePeer) next() Frame {
	p.t.Helper()
	select {
	case f := <-p.frames:
		return f
	case <-time.After(5 * time.Second):
		p.t.Fatal("no CALL frame reached the peer")
		return Frame{}
	}
}

// quiet fails the test if a CALL frame reaches the peer within d. It is the
// only way to show that something was not sent, so d is kept short and the
// test is proven to fail without the change it covers.
func (p *wirePeer) quiet(d time.Duration, why string) {
	p.t.Helper()
	select {
	case f := <-p.frames:
		p.t.Fatalf("%s: CALL %s (%s) reached the peer", why, f.ID, f.Action)
	case <-time.After(d):
	}
}

// answer sends a CALLRESULT for the CALL with the given id.
func (p *wirePeer) answer(f Frame, payload any) {
	p.t.Helper()
	data, err := EncodeCallResult(f.ID, payload)
	if err != nil {
		p.t.Fatalf("encode: %v", err)
	}
	p.write(data)
}

// answerError sends a CALLERROR for the CALL with the given id.
func (p *wirePeer) answerError(f Frame) {
	p.t.Helper()
	data, err := EncodeCallError(f.ID, ocpp.Errorf(ocpp.ErrGenericError, "no"))
	if err != nil {
		p.t.Fatalf("encode: %v", err)
	}
	p.write(data)
}

func (p *wirePeer) write(data []byte) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ws.WriteMessage(websocket.TextMessage, data); err != nil {
		p.t.Errorf("peer write: %v", err)
	}
}

type callOutcome struct {
	payload json.RawMessage
	err     error
}

// callAsync starts a Call and reports its outcome on the returned channel.
func (p *wirePeer) callAsync(ctx context.Context, action string) <-chan callOutcome {
	out := make(chan callOutcome, 1)
	go func() {
		raw, err := p.conn.Call(ctx, action, map[string]any{})
		out <- callOutcome{raw, err}
	}()
	return out
}

func outcome(t *testing.T, ch <-chan callOutcome, what string) callOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return callOutcome{}
	}
}

// settle is how long a test waits to be sure a frame is being held back.
const settle = 100 * time.Millisecond

func TestSecondCallWaitsForTheFirstResult(t *testing.T) {
	t.Parallel()

	p := newWirePeer(t, 10*time.Second)
	ctx := context.Background()

	first := p.callAsync(ctx, "GetConfiguration")
	f1 := p.next()
	second := p.callAsync(ctx, "UnlockConnector")

	p.quiet(settle, "a second CALL went out with the first unanswered")
	if got := p.conn.PendingCount(); got != 1 {
		t.Errorf("PendingCount = %d with one call on the wire and one queued, want 1", got)
	}

	p.answer(f1, map[string]any{"n": 1})
	if o := outcome(t, first, "first call"); o.err != nil || string(o.payload) != `{"n":1}` {
		t.Fatalf("first call = %s, %v", o.payload, o.err)
	}
	f2 := p.next()
	if f1.Action != "GetConfiguration" || f2.Action != "UnlockConnector" {
		t.Errorf("wire order = %s, %s", f1.Action, f2.Action)
	}
	p.answer(f2, map[string]any{"n": 2})
	if o := outcome(t, second, "second call"); o.err != nil || string(o.payload) != `{"n":2}` {
		t.Fatalf("second call = %s, %v", o.payload, o.err)
	}
}

func TestSecondCallGoesOutAfterACallError(t *testing.T) {
	t.Parallel()

	p := newWirePeer(t, 10*time.Second)
	ctx := context.Background()

	first := p.callAsync(ctx, "GetConfiguration")
	f1 := p.next()
	second := p.callAsync(ctx, "UnlockConnector")
	p.quiet(settle, "a second CALL went out with the first unanswered")

	p.answerError(f1)
	var rpcErr *ocpp.RPCError
	if o := outcome(t, first, "first call"); !errors.As(o.err, &rpcErr) {
		t.Fatalf("first call err = %v, want a CALLERROR", o.err)
	}
	f2 := p.next()
	p.answer(f2, map[string]any{})
	if o := outcome(t, second, "second call"); o.err != nil {
		t.Fatalf("second call: %v", o.err)
	}
}

func TestSecondCallGoesOutWhenTheFirstTimesOut(t *testing.T) {
	t.Parallel()

	// The queue cap is far off, so that the second call's own timer cannot
	// race the first one's timeout for the slot.
	p := newWirePeer(t, 200*time.Millisecond, func(o *Options) { o.QueueTimeout = time.Minute })
	ctx := context.Background()

	first := p.callAsync(ctx, "GetConfiguration")
	p.next()
	second := p.callAsync(ctx, "UnlockConnector")

	o := outcome(t, first, "first call")
	if !errors.Is(o.err, ocpp.ErrTimeout) {
		t.Fatalf("first call err = %v, want a timeout", o.err)
	}
	// Right after: the second does not have to wait out a timeout of its own
	// before it is sent. The test has five seconds, the timeout is 200ms.
	f2 := p.next()
	if f2.Action != "UnlockConnector" {
		t.Fatalf("second frame = %s", f2.Action)
	}
	p.answer(f2, map[string]any{"ok": true})
	if o := outcome(t, second, "second call"); o.err != nil {
		t.Fatalf("second call: %v", o.err)
	}
}

// A call that waited the whole queue timeout for its turn comes back as "not
// sent". That is a promise the layers above pass on to the person who typed
// the command (nothing happened, ask again), so it is shown the hard way:
// the frame never reaches the peer, not then and not later, however the call
// in front of it ends.
func TestQueuedCallPastTheCapIsNotSent(t *testing.T) {
	t.Parallel()

	const queue = 150 * time.Millisecond
	p := newWirePeer(t, 10*time.Second, func(o *Options) { o.QueueTimeout = queue })

	first := p.callAsync(context.Background(), "GetConfiguration")
	f1 := p.next()

	start := time.Now()
	o := outcome(t, p.callAsync(context.Background(), "UnlockConnector"), "queued call")
	if !errors.Is(o.err, ErrNotSent) || !errors.Is(o.err, ocpp.ErrNotSent) {
		t.Fatalf("queued call err = %v, want ErrNotSent (and ocpp.ErrNotSent)", o.err)
	}
	if errors.Is(o.err, ocpp.ErrTimeout) || errors.Is(o.err, ocpp.ErrNotConnected) {
		t.Errorf("err %v also matches a timeout or not-connected, which say the command may have gone out", o.err)
	}
	if waited := time.Since(start); waited < queue {
		t.Errorf("gave up after %s, before the %s cap", waited, queue)
	}
	p.quiet(settle, "a call that was reported as not sent went out anyway")

	// The first call is untouched, and answering it neither sends the call
	// that gave up nor leaves the slot taken.
	third := p.callAsync(context.Background(), "ClearCache")
	p.answer(f1, map[string]any{})
	if o := outcome(t, first, "first call"); o.err != nil {
		t.Fatalf("first call: %v", o.err)
	}
	f3 := p.next()
	if f3.Action != "ClearCache" {
		t.Fatalf("next frame = %s, want ClearCache: the call that gave up must stay unsent", f3.Action)
	}
	p.answer(f3, map[string]any{})
	if o := outcome(t, third, "third call"); o.err != nil {
		t.Fatalf("third call: %v", o.err)
	}
	if n := p.conn.PendingCount(); n != 0 {
		t.Errorf("PendingCount = %d after every call ended", n)
	}
}

// The queue cap follows the call timeout unless told otherwise, so a call's
// whole life is under twice the call timeout: the bound the CLI's wait is
// built on.
func TestQueueCapDefaultsToTheCallTimeout(t *testing.T) {
	t.Parallel()

	p := newWirePeer(t, 123*time.Millisecond)
	if got := p.conn.queueTimeout; got != 123*time.Millisecond {
		t.Errorf("queue timeout = %s, want the call timeout", got)
	}
}

func TestCloseReleasesAQueuedCall(t *testing.T) {
	t.Parallel()

	p := newWirePeer(t, 10*time.Second)
	ctx := context.Background()

	first := p.callAsync(ctx, "GetConfiguration")
	p.next()
	second := p.callAsync(ctx, "UnlockConnector")
	p.quiet(settle, "a second CALL went out with the first unanswered")

	p.conn.Close()

	for name, ch := range map[string]<-chan callOutcome{"first": first, "queued": second} {
		o := outcome(t, ch, name+" call")
		if !errors.Is(o.err, ocpp.ErrNotConnected) {
			t.Errorf("%s call err = %v, want one matching ocpp.ErrNotConnected", name, o.err)
		}
	}
	// A queued call that is released by the close must not be sent as the
	// connection goes down.
	p.quiet(settle, "the queued call was sent after the close")
}

func TestQueuedCallGivesUpWithItsContext(t *testing.T) {
	t.Parallel()

	p := newWirePeer(t, 10*time.Second)

	first := p.callAsync(context.Background(), "GetConfiguration")
	f1 := p.next()

	// Its deadline covers the wait in the queue: it never reaches the wire.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	o := outcome(t, p.callAsync(ctx, "UnlockConnector"), "queued call")
	if !errors.Is(o.err, context.DeadlineExceeded) {
		t.Fatalf("queued call err = %v, want its own deadline", o.err)
	}
	p.quiet(settle, "a queued call that gave up was sent anyway")

	// Giving up must not eat the slot, nor stall the call in front.
	third := p.callAsync(context.Background(), "ClearCache")
	p.answer(f1, map[string]any{})
	if o := outcome(t, first, "first call"); o.err != nil {
		t.Fatalf("first call: %v", o.err)
	}
	f3 := p.next()
	if f3.Action != "ClearCache" {
		t.Fatalf("next frame = %s, want ClearCache", f3.Action)
	}
	p.answer(f3, map[string]any{})
	if o := outcome(t, third, "third call"); o.err != nil {
		t.Fatalf("third call: %v", o.err)
	}
}

func TestInFlightCallGivingUpFreesTheSlot(t *testing.T) {
	t.Parallel()

	p := newWirePeer(t, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	first := p.callAsync(ctx, "GetConfiguration")
	p.next()
	second := p.callAsync(context.Background(), "UnlockConnector")
	p.quiet(settle, "a second CALL went out with the first unanswered")

	cancel()
	if o := outcome(t, first, "first call"); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("first call err = %v, want context.Canceled", o.err)
	}
	f2 := p.next()
	p.answer(f2, map[string]any{})
	if o := outcome(t, second, "second call"); o.err != nil {
		t.Fatalf("second call: %v", o.err)
	}
}

// Many callers at once: never more than one CALL unanswered, and every one
// of them is answered. This is the test to run with -race and -cpu 1,2,4.
func TestManyCallersOneInFlight(t *testing.T) {
	t.Parallel()

	const callers = 8
	p := newWirePeer(t, 10*time.Second)

	outs := make([]<-chan callOutcome, callers)
	for i := range outs {
		outs[i] = p.callAsync(context.Background(), "ClearCache")
	}
	for i := 0; i < callers; i++ {
		f := p.next()
		p.quiet(10*time.Millisecond, "another CALL went out with one unanswered")
		p.answer(f, map[string]any{"i": i})
	}
	for i, ch := range outs {
		if o := outcome(t, ch, "a caller"); o.err != nil {
			t.Errorf("caller %d: %v", i, o.err)
		}
	}
}
