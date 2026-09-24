// Package ocppj implements the OCPP-J RPC layer that sits on a WebSocket:
// the [2,id,action,payload] framing, request/response correlation, and the
// goroutines that drive one connection.
//
// It is deliberately direction-agnostic. The CSMS uses it for connections a
// charge point dialled in on; the charge point simulator uses it for the
// connection it dials out. Only who performs the WebSocket handshake differs,
// so only that lives outside this package.
package ocppj

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

const (
	// writeWait bounds a single frame write, so a wedged TCP connection cannot
	// block the writer goroutine forever.
	writeWait = 10 * time.Second
	// sendBuffer is how many outbound frames may queue before writes block.
	sendBuffer = 32
	// callBuffer is how many inbound CALLs may queue for the handler.
	//
	// Inbound CALLs are handled one at a time so that two StatusNotifications
	// for the same connector are applied in the order the charger sent them.
	// If the handler ever fell this far behind, the read loop would block,
	// which is correct backpressure onto the charger.
	callBuffer = 64
)

// ErrConnClosed is returned by Call when the connection is gone.
var ErrConnClosed = errors.New("ocpp: connection closed")

// callResult is the outcome of an outbound CALL.
type callResult struct {
	payload json.RawMessage
	rpcErr  *ocpp.RPCError
}

// Conn is one OCPP-J connection, from either end.
//
// It owns three goroutines: a reader, a writer (gorilla permits only one
// concurrent writer), and a handler that processes inbound CALLs in order.
type Conn struct {
	id      string
	version ocpp.Version
	ws      *websocket.Conn
	log     *slog.Logger

	callTimeout time.Duration
	// idleTimeout doubles as the heartbeat watchdog: it is applied as a read
	// deadline and refreshed by every inbound OCPP message, so a station that
	// stops talking is dropped without a second timer to keep in sync.
	// WebSocket pongs pointedly do not refresh it; see readLoop.
	idleTimeout time.Duration

	send  chan []byte
	calls chan Frame

	pendingMu sync.Mutex
	pending   map[string]chan callResult

	nextID atomic.Uint64

	closeOnce sync.Once
	done      chan struct{}
}

// Options configure a Conn.
type Options struct {
	// ID names the peer in logs. On the CSMS side it is the charge point
	// identity; on the charge point side it is our own.
	ID string
	// Version is the negotiated OCPP version.
	Version ocpp.Version
	// CallTimeout bounds an outbound request.
	CallTimeout time.Duration
	// IdleTimeout drops a connection with no inbound traffic for this long.
	IdleTimeout time.Duration
	Log         *slog.Logger
}

// New wraps an already-handshaken WebSocket connection.
func New(ws *websocket.Conn, opts Options) *Conn {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 30 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 90 * time.Second
	}
	return &Conn{
		id:          opts.ID,
		version:     opts.Version,
		ws:          ws,
		log:         opts.Log,
		callTimeout: opts.CallTimeout,
		idleTimeout: opts.IdleTimeout,
		send:        make(chan []byte, sendBuffer),
		calls:       make(chan Frame, callBuffer),
		pending:     make(map[string]chan callResult),
		done:        make(chan struct{}),
	}
}

// ID is the peer identity this connection belongs to.
func (c *Conn) ID() string { return c.id }

// Version is the OCPP version negotiated for this connection.
func (c *Conn) Version() ocpp.Version { return c.version }

// Done is closed when the connection has finished.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Call sends a CALL and waits for the matching CALLRESULT or CALLERROR.
//
// It returns when the peer answers, when ctx is cancelled, when the call
// timeout expires, or when the connection drops — and in every one of those
// cases it removes its pending entry, so a slow or silent charger cannot leak
// entries or goroutines.
func (c *Conn) Call(ctx context.Context, action string, payload any) (json.RawMessage, error) {
	id := strconv.FormatUint(c.nextID.Add(1), 10)

	data, err := EncodeCall(id, action, payload)
	if err != nil {
		return nil, err
	}

	ch := make(chan callResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer c.forget(id)

	select {
	case c.send <- data:
	case <-c.done:
		return nil, ErrConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	timer := time.NewTimer(c.callTimeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		if res.rpcErr != nil {
			return nil, res.rpcErr
		}
		return res.payload, nil
	case <-timer.C:
		return nil, fmt.Errorf("%s: no answer within %s", action, c.callTimeout)
	case <-c.done:
		return nil, ErrConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) forget(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// PendingCount reports how many calls are awaiting an answer.
func (c *Conn) PendingCount() int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending)
}

// Close shuts the connection down once, unblocking everything waiting on it.
// It is safe to call more than once.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// Run drives the connection until it ends, returning why it ended.
func (c *Conn) Run(ctx context.Context, handler ocpp.Handler) string {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writeLoop() }()
	go func() { defer wg.Done(); c.handleLoop(ctx, handler) }()

	reason := c.readLoop()

	c.Close()
	wg.Wait()
	return reason
}

// refreshDeadline restarts the idle watchdog. Any inbound OCPP message counts
// as a sign of life, not only Heartbeat: the spec treats any message as one.
// Pongs do not count; see readLoop.
func (c *Conn) refreshDeadline() {
	_ = c.ws.SetReadDeadline(time.Now().Add(c.idleTimeout))
}

func (c *Conn) readLoop() string {
	c.refreshDeadline()

	// A pong deliberately does NOT refresh the deadline.
	//
	// We ping every idleTimeout/3, and any WebSocket stack answers a ping with
	// a pong automatically, without the peer's application being involved. If
	// a pong counted as a sign of life, our own keepalive would reset the
	// watchdog on every cycle and it could never fire — least of all in the
	// case it exists for: a station whose TCP connection is healthy but whose
	// OCPP layer has stopped talking.
	//
	// So the watchdog measures application liveness (inbound OCPP messages)
	// and the ping measures transport liveness (a failed write drops the
	// connection from writeLoop). They are separate on purpose.
	c.ws.SetPongHandler(func(string) error { return nil })

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return closeReason(err)
		}
		c.refreshDeadline()

		f, errID, rpcErr := ParseFrame(data)
		if rpcErr != nil {
			c.log.Warn("malformed frame", "charge_point", c.id, "error", rpcErr.Error())
			c.sendCallError(errID, rpcErr)
			continue
		}

		switch f.Type {
		case MessageTypeCall:
			select {
			case c.calls <- f:
			case <-c.done:
				return "closed"
			}
		case MessageTypeCallResult:
			c.deliver(f.ID, callResult{payload: f.Payload})
		case MessageTypeCallError:
			c.deliver(f.ID, callResult{rpcErr: &ocpp.RPCError{
				Code:        f.ErrorCode,
				Description: f.ErrorDescription,
			}})
		}
	}
}

// deliver hands a response to whoever is waiting for it. A response we have no
// pending call for is logged and dropped: a charger answering twice, or late,
// must not be able to bring the connection down.
func (c *Conn) deliver(id string, res callResult) {
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	c.pendingMu.Unlock()

	if !ok {
		c.log.Warn("response for unknown message id", "charge_point", c.id, "message_id", id)
		return
	}
	select {
	case ch <- res:
	default:
	}
}

// handleLoop processes inbound CALLs one at a time, preserving the order the
// charger sent them in.
func (c *Conn) handleLoop(ctx context.Context, handler ocpp.Handler) {
	for {
		select {
		case <-c.done:
			return
		case f := <-c.calls:
			result, rpcErr := c.dispatch(ctx, handler, f)
			if rpcErr != nil {
				c.sendCallError(f.ID, rpcErr)
				continue
			}
			data, err := EncodeCallResult(f.ID, result)
			if err != nil {
				c.log.Error("encoding result", "charge_point", c.id, "action", f.Action, "error", err)
				c.sendCallError(f.ID, ocpp.Errorf(ocpp.ErrInternalError, "could not encode result"))
				continue
			}
			c.enqueue(data)
		}
	}
}

// dispatch calls the version handler, converting a panic into an InternalError
// so that one bad message cannot take the process down.
func (c *Conn) dispatch(ctx context.Context, handler ocpp.Handler, f Frame) (result any, rpcErr *ocpp.RPCError) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("handler panicked", "charge_point", c.id, "action", f.Action, "panic", r)
			result, rpcErr = nil, ocpp.Errorf(ocpp.ErrInternalError, "handler failed")
		}
	}()
	return handler.HandleCall(ctx, c.id, f.Action, f.Payload)
}

func (c *Conn) sendCallError(id string, rpcErr *ocpp.RPCError) {
	data, err := EncodeCallError(id, rpcErr)
	if err != nil {
		c.log.Error("encoding call error", "charge_point", c.id, "error", err)
		return
	}
	c.enqueue(data)
}

// enqueue queues a frame for the writer, dropping it if the connection is
// already gone.
func (c *Conn) enqueue(data []byte) {
	select {
	case c.send <- data:
	case <-c.done:
	}
}

func (c *Conn) writeLoop() {
	// Ping often enough to notice a dead peer well inside the idle timeout.
	pingPeriod := c.idleTimeout / 3
	if pingPeriod < time.Second {
		pingPeriod = time.Second
	}
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return

		case data := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, data); err != nil {
				c.log.Debug("write failed", "charge_point", c.id, "error", err)
				c.Close()
				return
			}

		case <-ticker.C:
			// WriteControl is safe alongside WriteMessage, which is why the
			// ping lives here rather than needing its own serialisation.
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.log.Debug("ping failed", "charge_point", c.id, "error", err)
				c.Close()
				return
			}
		}
	}
}

// closeReason turns a read error into something worth putting in a log line.
func closeReason(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "heartbeat timeout"
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return "closed by charge point"
	}
	return err.Error()
}
