package client_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/client"
)

// recorder is a peer that records the headers and bodies it receives and
// answers with whatever answer is set to.
type recorder struct {
	mu       sync.Mutex
	requests []recorded
	answer   func(w http.ResponseWriter)
}

type recorded struct {
	method string
	header http.Header
	body   string
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.requests = append(rec.requests, recorded{method: r.Method, header: r.Header.Clone(), body: string(body)})
	answer := rec.answer
	rec.mu.Unlock()
	if answer == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"ok":true},"status_code":1000,"status_message":"Success","timestamp":"2026-10-22T10:00:00Z"}`)
		return
	}
	answer(w)
}

func (rec *recorder) all() []recorded {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]recorded(nil), rec.requests...)
}

func newPeer(t *testing.T) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return rec, srv
}

func TestClientSendsTokenAndIDs(t *testing.T) {
	t.Parallel()
	rec, srv := newPeer(t)

	const token = "token-b-123"
	c := client.New(client.Options{Token: func() string { return token }})

	// Two calls outside any request: a fresh correlation id each.
	for range 2 {
		if _, err := c.Get(context.Background(), srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	// One call inside an inbound request: its correlation id carries over.
	ctx := ocpi.WithCorrelationID(context.Background(), "corr-from-inbound")
	if _, err := c.Post(ctx, srv.URL, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}

	got := rec.all()
	if len(got) != 3 {
		t.Fatalf("peer got %d requests, want 3", len(got))
	}
	wantAuth := "Token " + base64.StdEncoding.EncodeToString([]byte(token))
	seen := map[string]bool{}
	for i, r := range got {
		if a := r.header.Get("Authorization"); a != wantAuth {
			t.Errorf("request %d: Authorization = %q, want %q", i, a, wantAuth)
		}
		id := r.header.Get("X-Request-ID")
		if id == "" || seen[id] {
			t.Errorf("request %d: X-Request-ID %q is empty or reused", i, id)
		}
		seen[id] = true
	}
	if a, b := got[0].header.Get("X-Correlation-ID"), got[1].header.Get("X-Correlation-ID"); a == "" || a == b {
		t.Errorf("correlation ids outside a request = %q, %q; want two fresh ones", a, b)
	}
	if c := got[2].header.Get("X-Correlation-ID"); c != "corr-from-inbound" {
		t.Errorf("X-Correlation-ID = %q, want the inbound one", c)
	}
	if ct := got[0].header.Get("Content-Type"); ct != "" {
		t.Errorf("GET sent Content-Type %q without a body", ct)
	}
	if ct := got[2].header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("POST Content-Type = %q", ct)
	}
	if got[2].body != `{"a":"b"}` {
		t.Errorf("POST body = %q", got[2].body)
	}
}

func TestClientWithTokenOverrides(t *testing.T) {
	t.Parallel()
	rec, srv := newPeer(t)

	c := client.New(client.Options{Token: func() string { return "stored-token" }})
	if _, err := c.WithToken("explicit-token").Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	got := rec.all()
	if want := "Token " + base64.StdEncoding.EncodeToString([]byte("explicit-token")); got[0].header.Get("Authorization") != want {
		t.Errorf("WithToken sent %q", got[0].header.Get("Authorization"))
	}
	if want := "Token " + base64.StdEncoding.EncodeToString([]byte("stored-token")); got[1].header.Get("Authorization") != want {
		t.Errorf("the original client changed: sent %q", got[1].header.Get("Authorization"))
	}
}

func TestClientDecodesEnvelope(t *testing.T) {
	t.Parallel()
	_, srv := newPeer(t)

	resp, err := client.New(client.Options{}).Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var data struct{ OK bool }
	if err := resp.DecodeData(&data); err != nil || !data.OK {
		t.Fatalf("DecodeData = %+v, %v", data, err)
	}
	if resp.StatusCode != ocpi.StatusSuccess {
		t.Errorf("StatusCode = %d", resp.StatusCode)
	}
}

func TestClientErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		body       string
		wantHTTP   int
		wantStatus ocpi.StatusCode
	}{
		{"server error without envelope", 500, "oops", 500, 0},
		{"server error with envelope", 500, `{"status_code":3000,"status_message":"broken","timestamp":"x"}`, 500, 3000},
		{"non-1000 envelope", 200, `{"status_code":2001,"status_message":"bad","timestamp":"x"}`, 200, 2001},
		{"not an envelope", 200, `{"hello":"world"}`, 200, 0},
		{"unauthorized", 401, `{"status_code":2000,"status_message":"Unauthorized","timestamp":"x"}`, 401, 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, srv := newPeer(t)
			rec.answer = func(w http.ResponseWriter) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}
			_, err := client.New(client.Options{}).Get(context.Background(), srv.URL)
			var e *client.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *client.Error", err)
			}
			if e.HTTPStatus != tc.wantHTTP || e.StatusCode != tc.wantStatus {
				t.Errorf("Error = %+v, want HTTP %d, status_code %d", e, tc.wantHTTP, tc.wantStatus)
			}
		})
	}
}

func TestClientUnreachableIsATransportError(t *testing.T) {
	t.Parallel()
	_, srv := newPeer(t)
	url := srv.URL
	srv.Close()

	_, err := client.New(client.Options{}).Get(context.Background(), url)
	var e *client.Error
	if err == nil || errors.As(err, &e) {
		t.Fatalf("err = %v, want a transport error", err)
	}
}

func TestClientTimesOutWithTheContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Bounded, so a client that ignores its context fails the test
		// instead of hanging it.
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.New(client.Options{}).Get(ctx, srv.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s; the context deadline was not honoured", d)
	}
}

func TestClientNeverLogsTheToken(t *testing.T) {
	t.Parallel()
	_, srv := newPeer(t)

	var logs bytes.Buffer
	const token = "secret-token-b"
	c := client.New(client.Options{
		Token: func() string { return token },
		Log:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if logs.Len() == 0 {
		t.Fatal("expected a debug line per call")
	}
	for _, s := range []string{token, base64.StdEncoding.EncodeToString([]byte(token))} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("log output contains the token: %s", logs.String())
		}
	}
}
