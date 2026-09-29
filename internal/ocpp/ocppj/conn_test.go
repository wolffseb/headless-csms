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
