package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// pushEMSP is a partner whose Locations receiver records the PATCHes it gets,
// and can be told to fail them.
type pushEMSP struct {
	srv *httptest.Server

	mu      sync.Mutex
	patches []pushedPatch
	// failing makes every PATCH answer 500.
	failing bool
}

type pushedPatch struct {
	path, auth string
	body       map[string]any
}

func newPushEMSP(t *testing.T) *pushEMSP {
	t.Helper()
	f := &pushEMSP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/versions":
			data = []map[string]string{{"version": "2.3.0", "url": f.srv.URL + "/2.3.0"}}
		case r.Method == http.MethodGet && r.URL.Path == "/2.3.0":
			data = map[string]any{"version": "2.3.0", "endpoints": []map[string]string{
				{"identifier": "credentials", "role": "SENDER", "url": f.srv.URL + "/2.3.0/credentials"},
				{"identifier": "locations", "role": "RECEIVER", "url": f.srv.URL + "/2.3.0/locations"},
			}}
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/2.3.0/locations/"):
			raw, _ := io.ReadAll(r.Body)
			p := pushedPatch{path: r.URL.Path, auth: r.Header.Get("Authorization")}
			_ = json.Unmarshal(raw, &p.body)
			f.mu.Lock()
			f.patches = append(f.patches, p)
			failing := f.failing
			f.mu.Unlock()
			if failing {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "status_code": 1000, "timestamp": "2026-10-22T10:00:00Z"})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *pushEMSP) got() []pushedPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushedPatch(nil), f.patches...)
}

func (f *pushEMSP) setFailing(v bool) {
	f.mu.Lock()
	f.failing = v
	f.mu.Unlock()
}

// next waits for the PATCH after the first n, within a second and a half: the
// debounce is half a second, and the issue asks for a PATCH within one
// second of the change.
func (f *pushEMSP) next(t *testing.T, n int, what string) pushedPatch {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		if got := f.got(); len(got) > n {
			return got[n]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no PATCH for %s; the partner got %+v", what, f.got())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settled fails if the count moves past n over a window longer than the
// debounce.
func (f *pushEMSP) settled(t *testing.T, n int) {
	t.Helper()
	time.Sleep(800 * time.Millisecond)
	if got := f.got(); len(got) != n {
		t.Fatalf("the partner got %d PATCHes, want %d: %+v", len(got), n, got)
	}
}

// TestStatusPushThroughCPMSRun is the push against a running cpms: a partner
// registers, and then both ways an EVSE changes (`cpms reserve` and the
// station itself) arrive as PATCHes. `cpms status` shows how the push is
// doing, and shutting down while it is backing off is clean.
func TestStatusPushThroughCPMSRun(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19550, 19551
	s := &station{
		t:          t,
		port:       ocppPort,
		configPath: runConfigPorts(t, ocppPort, ocpiPort),
		socketPath: filepath.Join(shortDir(t), "cpms.sock"),
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s.startRun()
	emsp := newPushEMSP(t)

	// Not registered: the station comes online and nothing is pushed.
	s.connect()
	emsp.settled(t, 0)
	if p := s.status().OCPI.Push; p != nil {
		t.Errorf("status shows a push before registration: %+v", p)
	}

	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(map[string]any{
		"token": "emsp-token-b", "url": emsp.srv.URL + "/versions",
		"roles": []map[string]any{{"role": "EMSP", "party_id": "ABC", "country_code": "NL",
			"business_details": map[string]any{"name": "Fake"}}},
	})
	req, err := http.NewRequest(http.MethodPost,
		"http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort))+"/ocpi/2.3.0/credentials", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(cfg.OCPI.TokenA)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST credentials: HTTP %d", resp.StatusCode)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "nothing pushed yet") {
		t.Errorf("status of a registration with nothing pushed:\n%s", human)
	}

	evse := cfg.Location.EVSEs[0]
	wantPath := "/2.3.0/locations/" + cfg.OCPI.CountryCode + "/" + cfg.OCPI.PartyID + "/" + cfg.Location.ID + "/" + evse.UID
	wantAuth := "Token " + base64.StdEncoding.EncodeToString([]byte("emsp-token-b"))
	check := func(p pushedPatch, status string) {
		t.Helper()
		if p.path != wantPath || p.auth != wantAuth || p.body["status"] != status {
			t.Errorf("PATCH %s %q %v, want %s %q status %s", p.path, p.auth, p.body, wantPath, wantAuth, status)
		}
		if ts, _ := p.body["last_updated"].(string); !strings.HasSuffix(ts, "Z") || len(p.body) != 2 {
			t.Errorf("PATCH body %v: want status and a UTC last_updated", p.body)
		}
	}

	// Source one: the terminal.
	s.mustCPMS("reserve")
	check(emsp.next(t, 0, "cpms reserve"), "RESERVED")
	emsp.settled(t, 1)

	// Source two: the station. A reservation ends with a cancel, which the
	// station answers with Available, and then somebody plugs in.
	s.mustCPMS("cancel")
	check(emsp.next(t, 1, "cpms cancel"), "AVAILABLE")
	if err := s.sim.SetConnectorStatus(context.Background(), evse.OCPPConnectorID, "Charging"); err != nil {
		t.Fatal(err)
	}
	check(emsp.next(t, 2, "the station's own change"), "CHARGING")
	emsp.settled(t, 3)

	// The health shows on the OCPI line and in --json.
	st := s.waitStatus("the push to settle", func(st socket.Status) bool {
		return st.OCPI.Push != nil && st.OCPI.Push.LastSuccess != nil && st.OCPI.Push.Pending == 0
	})
	if st.OCPI.Push.LastError != "" {
		t.Errorf("last_error = %q after only successes", st.OCPI.Push.LastError)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "push: "+emsp.srv.URL+"/2.3.0/locations (last push ") {
		t.Errorf("status does not show the last push:\n%s", human)
	}

	// A failing partner shows up as the reason, and cpms stays up; shutting
	// down in the middle of the backoff is clean (stopRun fails otherwise).
	emsp.setFailing(true)
	if err := s.sim.SetConnectorStatus(context.Background(), evse.OCPPConnectorID, "Available"); err != nil {
		t.Fatal(err)
	}
	st = s.waitStatus("the failure to show", func(st socket.Status) bool {
		return st.OCPI.Push != nil && st.OCPI.Push.LastError != "" && st.OCPI.Push.Pending == 1
	})
	if !strings.Contains(st.OCPI.Push.LastError, "500") {
		t.Errorf("last_error = %q, want the HTTP status", st.OCPI.Push.LastError)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "last push failed ") || !strings.Contains(human, "1 pending") {
		t.Errorf("status does not show the failure:\n%s", human)
	}
	s.stopRun()
}
