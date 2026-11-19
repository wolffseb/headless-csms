package control_test

import (
	"bytes"
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

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// The cases below run the real stack from the control service down to a
// WebSocket peer: control.Service, the 1.6 adapter and an ocppj.Conn with its
// call slot. A fake ChargePoint cannot show them, since what matters is the
// time a CancelReservation spends waiting for its turn behind other commands.

type nopHandler struct{}

func (nopHandler) Version() ocpp.Version { return ocpp.Version16 }
func (nopHandler) HandleCall(context.Context, string, string, json.RawMessage) (any, *ocpp.RPCError) {
	return nil, nil
}

// syncBuffer is a log sink the test can read while the service writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wireRig is a control.Service whose station is a bare WebSocket peer that
// records every CALL and answers only when the test says so.
type wireRig struct {
	t      *testing.T
	core   *core.Service
	ctl    *control.Service
	conn   *ocppj.Conn
	log    *syncBuffer
	frames chan ocppj.Frame

	mu sync.Mutex
	ws *websocket.Conn
}

func newWireRig(t *testing.T, callTimeout time.Duration) *wireRig {
	t.Helper()

	r := &wireRig{t: t, log: &syncBuffer{}, frames: make(chan ocppj.Frame, 32)}
	connected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
		if err != nil {
			return
		}
		r.mu.Lock()
		r.ws = ws
		r.mu.Unlock()
		close(connected)
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			f, _, rpcErr := ocppj.ParseFrame(data)
			if rpcErr != nil {
				t.Errorf("peer got a malformed frame: %v", rpcErr)
				return
			}
			r.frames <- f
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
	<-connected

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.conn = ocppj.New(ws, ocppj.Options{
		ID: testCP, Version: ocpp.Version16, CallTimeout: callTimeout, Log: discard,
	})
	done := make(chan struct{})
	go func() { defer close(done); r.conn.Run(context.Background(), nopHandler{}) }()
	t.Cleanup(func() { r.conn.Close(); <-done })

	cfg := testConfig()
	cfg.Charger.CallTimeout = config.NewDuration(callTimeout)
	r.core = core.New(cfg, core.WithStore(statetest.New(t)), core.WithLogger(discard))
	t.Cleanup(r.core.Close)
	cp := v16.NewChargePoint(r.conn, cfg)
	r.ctl = control.New(control.Options{
		Core: r.core, Config: cfg, Log: slog.New(slog.NewTextHandler(r.log, nil)),
		Commands: stationOf{cp},
	})
	return r
}

type stationOf struct{ cp ocpp.ChargePoint }

func (s stationOf) Command(string) (ocpp.ChargePoint, error) { return s.cp, nil }

// next waits for the next CALL the peer receives and checks its action.
func (r *wireRig) next(action string) ocppj.Frame {
	r.t.Helper()
	select {
	case f := <-r.frames:
		if f.Action != action {
			r.t.Fatalf("peer got %s, want %s", f.Action, action)
		}
		return f
	case <-time.After(15 * time.Second):
		r.t.Fatalf("no %s reached the peer", action)
		return ocppj.Frame{}
	}
}

// answer sends a CALLRESULT carrying a status.
func (r *wireRig) answer(f ocppj.Frame, status string) {
	r.t.Helper()
	data, err := ocppj.EncodeCallResult(f.ID, map[string]any{"status": status})
	if err != nil {
		r.t.Fatalf("encode: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ws.WriteMessage(websocket.TextMessage, data); err != nil {
		r.t.Errorf("peer write: %v", err)
	}
}

func (r *wireRig) reservations() []core.Reservation {
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	return snap.Reservations
}

// waitLog waits until the service has logged text.
func (r *wireRig) waitLog(text string) {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(r.log.String(), text) {
		if time.Now().After(deadline) {
			r.t.Fatalf("log never said %q:\n%s", text, r.log)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantCancelling fails unless the one reservation record is still cancelling
// and a new Reserve for its EVSE is refused without a ReserveNow going out.
func (r *wireRig) wantCancelling(when string) {
	r.t.Helper()
	if got := r.reservations(); len(got) != 1 || got[0].Status != core.ReservationCancelling {
		r.t.Fatalf("%s: reservations = %+v, want one cancelling: the station has not answered the cancel", when, got)
	}
	_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})
	if !errors.Is(err, core.ErrAlreadyReserved) {
		r.t.Fatalf("%s: a new Reserve = %v, want ErrAlreadyReserved: it would overtake the unanswered cancel", when, err)
	}
	select {
	case f := <-r.frames:
		r.t.Fatalf("%s: %s reached the peer while the cancel was unanswered", when, f.Action)
	default:
	}
}

// reserveSilently starts a Reserve whose ReserveNow the peer never answers
// and returns the channel its error arrives on.
func (r *wireRig) reserveSilently() <-chan error {
	out := make(chan error, 1)
	go func() {
		_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"})
		out <- err
	}()
	r.next("ReserveNow")
	return out
}

// unlockAsync queues an UnlockConnector behind whatever is on the wire.
func (r *wireRig) unlockAsync() {
	go func() { _ = r.ctl.Unlock(context.Background(), "EVSE-2") }()
}

// A cancel that waited in the queue behind another command is still owed a
// full call timeout on the wire. Its deadline used to be sized for a send that
// never waits (call_timeout plus one second from the moment it was queued), so
// the queue wait ate into it: the cancel was cut off while the station might
// still be answering, the record ended, and a new ReserveNow overtook it.
func TestOrphanCancelQueuedBehindAnUnlockKeepsItsFullCallTimeout(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("takes about six seconds")
	}
	// The second of slack in the old deadline must be shorter than the wait in
	// the queue, so the timeout is long enough for that.
	const callTimeout = 2 * time.Second
	r := newWireRig(t, callTimeout)

	reserved := r.reserveSilently()
	// The unlock queues behind the silent ReserveNow, so it gets the slot
	// before the cancel does.
	time.Sleep(100 * time.Millisecond)
	r.unlockAsync()

	if err := <-reserved; !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("Reserve = %v, want ErrTimeout", err)
	}
	unlock := r.next("UnlockConnector")
	// The station answers the unlock late in its call timeout, so the cancel,
	// queued meanwhile, waits almost a call timeout for its turn.
	time.Sleep(callTimeout * 9 / 10)
	r.answer(unlock, "Unlocked")

	cancel := r.next("CancelReservation")
	sent := time.Now()
	// The old deadline (call_timeout + 1s after the cancel was queued) is
	// over by now, and the call timer (call_timeout after the send) is not.
	time.Sleep(time.Until(sent.Add(callTimeout * 4 / 5)))
	r.wantCancelling("1.6 s after the cancel was sent")

	r.answer(cancel, "Accepted")
	waitFor(t, "the record to end once the station answered", func() bool { return len(r.reservations()) == 0 })
}

// A cancel that does not get its turn within the queue timeout is not sent,
// and the station may be holding the reservation. The record must stay
// cancelling and the cancel be tried again, not given up.
func TestOrphanCancelThatIsNotSentIsTriedAgain(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("takes about six seconds")
	}
	// Times below are in call timeouts (T) from the ReserveNow. Every call
	// waits at most T in the queue, the unlocks as well, so the second one is
	// queued late enough to still be waiting when it gets the slot.
	const callTimeout = 2 * time.Second
	r := newWireRig(t, callTimeout)

	reserved := r.reserveSilently()
	// Two unlocks queue behind the silent ReserveNow, ahead of the cancel: the
	// first at once, the second at 0.75 T, before the ReserveNow times out.
	time.Sleep(100 * time.Millisecond)
	r.unlockAsync()
	time.Sleep(callTimeout*3/4 - 100*time.Millisecond)
	r.unlockAsync()

	if err := <-reserved; !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("Reserve = %v, want ErrTimeout", err)
	}
	// The first unlock gets the slot at T and is answered at 1.5 T, which is
	// 0.25 T before the second one's turn runs out, and it holds the slot
	// past 2 T, when the cancel's turn runs out. The station leaves the
	// second unlock unanswered.
	first := r.next("UnlockConnector")
	time.Sleep(callTimeout / 2)
	r.answer(first, "Unlocked")
	r.next("UnlockConnector")

	r.waitLog("call not sent")
	time.Sleep(100 * time.Millisecond)
	r.wantCancelling("after the first cancel was not sent")

	// The second unlock times out, and the cancel gets its turn at last.
	cancel := r.next("CancelReservation")
	r.wantCancelling("with the second cancel on the wire")
	r.answer(cancel, "Accepted")
	waitFor(t, "the record to end once the station answered", func() bool { return len(r.reservations()) == 0 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A cancel that keeps not being sent is tried again, but not for ever: Close
// ends it, so that shutdown does not wait on a station that never frees up.
func TestOrphanCancelThatIsNotSentStopsAtClose(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["reserve"] = ocpp.ErrTimeout
	r.station.fail["cancel"] = ocpp.ErrNotSent

	if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("Reserve = %v, want ErrTimeout", err)
	}
	waitFor(t, "the cancel to be tried three times", func() bool {
		r.station.mu.Lock()
		defer r.station.mu.Unlock()
		return len(r.station.cancels) >= 3
	})
	if got := r.reservations(); len(got) != 1 || got[0].Status != core.ReservationCancelling {
		t.Fatalf("reservations while the cancel is not sent = %+v, want one cancelling", got)
	}

	closed := make(chan struct{})
	go func() { r.ctl.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close is still waiting for a cancel that is tried again")
	}
	if got := r.reservations(); len(got) != 0 {
		t.Errorf("reservations after Close = %+v, want none", got)
	}
}

// A cancel released unsent by a closing connection matches ErrNotSent too, but
// the connection is gone: trying again would never end.
func TestOrphanCancelOnAClosedConnectionIsNotTriedAgain(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["reserve"] = ocpp.ErrTimeout
	r.station.fail["cancel"] = ocppj.ErrClosedUnsent

	if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); !errors.Is(err, ocpp.ErrTimeout) {
		t.Fatalf("Reserve = %v, want ErrTimeout", err)
	}
	// Without Close: the record has to end on its own, not because shutdown
	// stopped the retries.
	waitFor(t, "the record to end with the connection", func() bool { return len(r.reservations()) == 0 })
	r.ctl.Close()
	r.station.mu.Lock()
	defer r.station.mu.Unlock()
	if len(r.station.cancels) != 1 {
		t.Errorf("cancels = %v, want one attempt", r.station.cancels)
	}
}

// A ReserveNow released unsent by a closing connection never reached the
// station: nothing to cancel.
func TestReserveQueuedWhenTheConnectionClosedSendsNoCancel(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.station.fail["reserve"] = ocppj.ErrClosedUnsent

	if _, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1"}); !errors.Is(err, ocpp.ErrNotConnected) {
		t.Fatalf("Reserve = %v, want ErrNotConnected", err)
	}
	r.ctl.Close()
	r.station.mu.Lock()
	defer r.station.mu.Unlock()
	if len(r.station.cancels) != 0 {
		t.Errorf("cancels = %v, want none for a ReserveNow that was never sent", r.station.cancels)
	}
	if got := r.reservations(); len(got) != 0 {
		t.Errorf("reservations = %+v, want none", got)
	}
}
