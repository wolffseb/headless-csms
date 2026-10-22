package ocpi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// idsModule answers with the ids its handler finds in the request context.
func idsModule() ocpi.Module {
	return ocpi.Module{
		Identifier: "ids",
		Role:       ocpi.RoleReceiver,
		Access:     ocpi.AccessRegistration,
		Routes: []ocpi.Route{{
			Method: http.MethodGet,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				ocpi.WriteOK(w, map[string]string{
					"request_id":     ocpi.RequestID(r.Context()),
					"correlation_id": ocpi.CorrelationID(r.Context()),
				})
			},
		}},
	}
}

func TestHandlersSeeTheRequestAndCorrelationIDs(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, idsModule())
	url := f.root() + "/2.3.0/ids"
	token := tokenHeader(f.cfg.OCPI.TokenA)

	check := func(name string, r reply) {
		t.Helper()
		var got map[string]string
		if err := json.Unmarshal(r.env.Data, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got["request_id"] == "" || got["correlation_id"] == "" {
			t.Errorf("%s: handler saw empty ids %v", name, got)
		}
		if got["request_id"] != r.header.Get("X-Request-ID") {
			t.Errorf("%s: handler request id %q, header %q", name, got["request_id"], r.header.Get("X-Request-ID"))
		}
		if got["correlation_id"] != r.header.Get("X-Correlation-ID") {
			t.Errorf("%s: handler correlation id %q, header %q", name, got["correlation_id"], r.header.Get("X-Correlation-ID"))
		}
	}

	r := f.do(http.MethodGet, url, token, nil, "X-Request-ID", "req-7", "X-Correlation-ID", "corr-7")
	check("sent", r)
	if r.header.Get("X-Correlation-ID") != "corr-7" {
		t.Errorf("sent correlation id was not kept: %q", r.header.Get("X-Correlation-ID"))
	}
	check("generated", f.do(http.MethodGet, url, token, nil))
}

// startServer serves a real listener, which is what the lifetime context and
// the timeouts act on.
func startServer(t *testing.T, edit func(*ocpi.Options), modules ...ocpi.Module) (*ocpi.Server, *fixture) {
	t.Helper()
	f := newFixture(t, func(o *ocpi.Options) {
		o.Bind = "127.0.0.1:0"
		if edit != nil {
			edit(o)
		}
	}, modules...)
	if err := f.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.server.Shutdown(context.Background()) })
	return f.server, f
}

func TestShutdownCancelsRequestsInFlight(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	finished := make(chan struct{})
	var lifetime context.Context
	block := ocpi.Module{
		Identifier: "block",
		Role:       ocpi.RoleReceiver,
		Access:     ocpi.AccessRegistration,
		Routes: []ocpi.Route{{
			Method: http.MethodGet,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(finished)
				ocpi.WriteOK(w, nil)
			},
		}},
	}
	s, f := startServer(t, nil, block)
	lifetime = s.Context()
	if lifetime.Err() != nil {
		t.Fatal("the lifetime context is already done")
	}

	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+f.root()+"/2.3.0/block", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", tokenHeader(f.cfg.OCPI.TokenA))
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with a handler waiting on its context: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Error("Shutdown returned before the handler saw its context end")
	}
	if lifetime.Err() == nil {
		t.Error("the lifetime context was not cancelled by Shutdown")
	}
}

// A client that stalls mid-request must not hold a connection forever.
func TestSlowRequestsAreCutOff(t *testing.T) {
	t.Parallel()

	s, f := startServer(t, func(o *ocpi.Options) { o.ReadTimeout = 200 * time.Millisecond })

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The headers promise a body that never comes.
	_, err = io.WriteString(conn, "POST "+f.root()+"/2.3.0/credentials HTTP/1.1\r\nHost: x\r\nAuthorization: "+
		tokenHeader(f.cfg.OCPI.TokenA)+"\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadAll(conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the server still held the stalled connection after 5s")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the server still held the stalled connection after 5s")
	}
}

func TestIdleConnectionsAreClosed(t *testing.T) {
	t.Parallel()

	s, f := startServer(t, func(o *ocpi.Options) { o.IdleTimeout = 200 * time.Millisecond })

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// One complete request on a keep-alive connection, then silence.
	_, err = io.WriteString(conn, "GET "+f.root()+"/versions HTTP/1.1\r\nHost: x\r\nAuthorization: "+
		tokenHeader(f.cfg.OCPI.TokenA)+"\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if _, err := io.ReadAll(br); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the server still held an idle connection after 5s")
	}
}
