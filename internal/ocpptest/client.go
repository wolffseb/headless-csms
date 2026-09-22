// Package ocpptest provides a raw OCPP-J WebSocket client for tests.
//
// It speaks the wire format directly rather than reusing the server's own
// framing code, so a bug in that code cannot hide behind a test that shares
// it. The charge point simulator built later is a richer thing entirely; this
// is the minimum needed to drive and observe a CSMS.
package ocpptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Message type ids from OCPP-J.
const (
	TypeCall       = 2
	TypeCallResult = 3
	TypeCallError  = 4
)

// Response is an inbound CALLRESULT or CALLERROR.
type Response struct {
	Type             int
	ID               string
	Payload          json.RawMessage
	ErrorCode        string
	ErrorDescription string
}

// IsError reports whether the response is a CALLERROR.
func (r Response) IsError() bool { return r.Type == TypeCallError }

// Call is an inbound CALL from the server.
type Call struct {
	ID      string
	Action  string
	Payload json.RawMessage
}

// Client is a charge-point-side OCPP-J connection.
type Client struct {
	tb testing.TB
	ws *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan Response
	nextID  int
	handler func(Call) (any, error)

	// unsolicited collects responses that match no pending call, which is how
	// tests observe the CALLERRORs the server sends for malformed frames.
	unsolicited chan Response

	closeOnce sync.Once
	done      chan struct{}
}

// Handshake is what the server said during the upgrade, kept after the
// response body has been closed so that failed handshakes can still be
// inspected.
type Handshake struct {
	StatusCode int
	// Subprotocol is the Sec-WebSocket-Protocol the server echoed, empty when
	// it selected none.
	Subprotocol string
	Header      http.Header
}

// Dial connects to a CSMS, offering the given subprotocols. The handshake is
// returned even when the connection failed, which is how the rejection paths
// are tested.
func Dial(tb testing.TB, url string, subprotocols ...string) (*Client, *Handshake, error) {
	tb.Helper()

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		Subprotocols:     subprotocols,
	}
	ws, resp, err := dialer.Dial(url, nil)

	hs := &Handshake{}
	if resp != nil {
		hs.StatusCode = resp.StatusCode
		hs.Header = resp.Header
		hs.Subprotocol = resp.Header.Get("Sec-WebSocket-Protocol")
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		return nil, hs, err
	}

	c := &Client{
		tb:          tb,
		ws:          ws,
		pending:     make(map[string]chan Response),
		unsolicited: make(chan Response, 16),
		done:        make(chan struct{}),
	}
	go c.readLoop()
	tb.Cleanup(c.Close)
	return c, hs, nil
}

// MustDial connects with the ocpp1.6 subprotocol and fails the test if it
// cannot.
func MustDial(tb testing.TB, url string) *Client {
	tb.Helper()

	c, _, err := Dial(tb, url, "ocpp1.6")
	if err != nil {
		tb.Fatalf("dialling %s: %v", url, err)
	}
	return c
}

// OnCall registers a handler for inbound CALLs. Returning an error answers
// with a GenericError CALLERROR. Without a handler, inbound calls are answered
// with NotImplemented.
func (c *Client) OnCall(fn func(Call) (any, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = fn
}

// Call sends a CALL and waits for its answer.
func (c *Client) Call(action string, payload any) (Response, error) {
	c.mu.Lock()
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan Response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if payload == nil {
		payload = struct{}{}
	}
	data, err := json.Marshal([]any{TypeCall, id, action, payload})
	if err != nil {
		return Response{}, err
	}
	if err := c.SendRaw(data); err != nil {
		return Response{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-c.done:
		return Response{}, fmt.Errorf("connection closed while waiting for %s", action)
	case <-time.After(5 * time.Second):
		return Response{}, fmt.Errorf("no answer to %s within 5s", action)
	}
}

// MustCall sends a CALL and requires a successful CALLRESULT.
func (c *Client) MustCall(action string, payload any) json.RawMessage {
	c.tb.Helper()

	resp, err := c.Call(action, payload)
	if err != nil {
		c.tb.Fatalf("%s: %v", action, err)
	}
	if resp.IsError() {
		c.tb.Fatalf("%s answered with %s: %s", action, resp.ErrorCode, resp.ErrorDescription)
	}
	return resp.Payload
}

// CallExpectingError sends a CALL and requires a CALLERROR back.
func (c *Client) CallExpectingError(action string, payload any) Response {
	c.tb.Helper()

	resp, err := c.Call(action, payload)
	if err != nil {
		c.tb.Fatalf("%s: %v", action, err)
	}
	if !resp.IsError() {
		c.tb.Fatalf("%s succeeded, expected a CALLERROR", action)
	}
	return resp
}

// SendRaw writes bytes verbatim, for testing malformed input.
func (c *Client) SendRaw(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// NextUnsolicited waits for a response that matched no pending call, such as
// the CALLERROR sent in reply to a malformed frame.
func (c *Client) NextUnsolicited(timeout time.Duration) (Response, bool) {
	select {
	case r := <-c.unsolicited:
		return r, true
	case <-time.After(timeout):
		return Response{}, false
	}
}

// Closed reports whether the connection has ended.
func (c *Client) Closed() <-chan struct{} { return c.done }

// Close shuts the connection down. It is safe to call more than once.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		_ = c.ws.Close()
		close(c.done)
	})
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.Close()
			return
		}

		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil || len(raw) < 3 {
			continue
		}

		var typ int
		var id string
		if err := json.Unmarshal(raw[0], &typ); err != nil {
			continue
		}
		if err := json.Unmarshal(raw[1], &id); err != nil {
			continue
		}

		switch typ {
		case TypeCall:
			c.answerCall(raw, id)
		case TypeCallResult:
			c.route(Response{Type: typ, ID: id, Payload: raw[2]})
		case TypeCallError:
			resp := Response{Type: typ, ID: id}
			_ = json.Unmarshal(raw[2], &resp.ErrorCode)
			if len(raw) > 3 {
				_ = json.Unmarshal(raw[3], &resp.ErrorDescription)
			}
			c.route(resp)
		}
	}
}

func (c *Client) answerCall(raw []json.RawMessage, id string) {
	if len(raw) < 4 {
		return
	}
	var action string
	if err := json.Unmarshal(raw[2], &action); err != nil {
		return
	}

	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()

	if handler == nil {
		_ = c.SendRaw(mustMarshal([]any{TypeCallError, id, "NotImplemented", "no handler registered", map[string]any{}}))
		return
	}

	result, err := handler(Call{ID: id, Action: action, Payload: raw[3]})
	if err != nil {
		_ = c.SendRaw(mustMarshal([]any{TypeCallError, id, "GenericError", err.Error(), map[string]any{}}))
		return
	}
	if result == nil {
		result = struct{}{}
	}
	_ = c.SendRaw(mustMarshal([]any{TypeCallResult, id, result}))
}

func (c *Client) route(resp Response) {
	c.mu.Lock()
	ch, ok := c.pending[resp.ID]
	c.mu.Unlock()

	if ok {
		select {
		case ch <- resp:
		default:
		}
		return
	}
	select {
	case c.unsolicited <- resp:
	default:
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
