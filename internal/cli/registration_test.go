package cli_test

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// TestRegistrationThroughCPMSRun is the handshake against a running cpms:
// the partner POSTs its credentials, `cpms status` shows the registration,
// and `cpms ocpi unregister` undoes it and tells the partner.
func TestRegistrationThroughCPMSRun(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19450, 19451
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

	if st := s.status(); st.OCPI.Registered {
		t.Fatalf("registered before any handshake: %+v", st.OCPI)
	}
	if !strings.Contains(s.mustCPMS("status"), "OCPI  not registered") {
		t.Error("status does not say OCPI is not registered")
	}
	// Unregistering with nothing registered is a local refusal.
	_, stderr, code := s.cpms("ocpi", "unregister")
	expectExit(t, "unregister before registration", code, cli.ExitRefused, stderr, "not registered")

	emsp := newCLIFakeEMSP(t)
	credentialsURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort)) + "/ocpi/2.3.0/credentials"
	register := func() {
		t.Helper()
		body := `{"token":"emsp-token-b","url":"` + emsp.srv.URL + `/versions",` +
			`"roles":[{"role":"EMSP","party_id":"ABC","country_code":"NL","business_details":{"name":"Fake"}}]}`
		req, err := http.NewRequest(http.MethodPost, credentialsURL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(cfg.OCPI.TokenA)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			StatusCode int `json:"status_code"`
		}
		err = json.NewDecoder(resp.Body).Decode(&env)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || env.StatusCode != 1000 {
			t.Fatalf("POST credentials: HTTP %d, status_code %d, %v", resp.StatusCode, env.StatusCode, err)
		}
	}
	register()

	push := emsp.srv.URL + "/2.3.0/locations"
	st := s.status()
	if !st.OCPI.Registered || st.OCPI.Party != "NL*ABC" || st.OCPI.PushEndpoint != push || st.OCPI.Since == nil {
		t.Errorf("status OCPI = %+v", st.OCPI)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "OCPI  registered with NL*ABC since ") ||
		!strings.Contains(human, "push: "+push) {
		t.Errorf("status does not show the registration:\n%s", human)
	}
	if strings.Contains(s.mustCPMS("status", "--json"), "emsp-token-b") {
		t.Error("status --json shows token B")
	}

	out := s.mustCPMS("ocpi", "unregister")
	if !strings.Contains(out, "unregistered from NL*ABC") {
		t.Errorf("unregister said: %s", out)
	}
	if !emsp.sawDelete() {
		t.Error("the partner was not told with a DELETE on its credentials")
	}
	if st := s.status(); st.OCPI.Registered {
		t.Errorf("still registered after unregister: %+v", st.OCPI)
	}

	// A partner that cannot be told does not undo the unregistration: the
	// command still exits 0, and warns on stderr.
	register()
	emsp.setDeleteStatus(http.StatusInternalServerError)
	stdout, stderr, code := s.cpms("ocpi", "unregister")
	if code != 0 {
		t.Fatalf("unregister with a failing partner exited %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "NL*ABC") {
		t.Errorf("stderr = %q, want a warning naming the partner", stderr)
	}
	if !strings.Contains(stdout, "unregistered from NL*ABC") {
		t.Errorf("stdout = %q", stdout)
	}
	if st := s.status(); st.OCPI.Registered {
		t.Errorf("still registered after a failed notification: %+v", st.OCPI)
	}
	s.stopRun()

	// state.json agrees after shutdown.
	store, err := state.Open(filepath.Join(filepath.Dir(s.configPath), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Get().OCPI != nil {
		t.Error("state.json still holds the registration")
	}
}

// cliFakeEMSP is a partner with versions, version details and a credentials
// endpoint, recording whether it was told about an unregistration.
type cliFakeEMSP struct {
	srv     *httptest.Server
	mu      sync.Mutex
	deleted bool
	// deleteStatus, when set, is the HTTP status answering DELETE.
	deleteStatus int
}

func (f *cliFakeEMSP) setDeleteStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteStatus = code
}

func newCLIFakeEMSP(t *testing.T) *cliFakeEMSP {
	t.Helper()
	f := &cliFakeEMSP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.Method + " " + r.URL.Path {
		case "GET /versions":
			data = []map[string]string{{"version": "2.3.0", "url": f.srv.URL + "/2.3.0"}}
		case "GET /2.3.0":
			data = map[string]any{"version": "2.3.0", "endpoints": []map[string]string{
				{"identifier": "credentials", "role": "SENDER", "url": f.srv.URL + "/2.3.0/credentials"},
				{"identifier": "locations", "role": "RECEIVER", "url": f.srv.URL + "/2.3.0/locations"},
			}}
		case "DELETE /2.3.0/credentials":
			f.mu.Lock()
			f.deleted = true
			status := f.deleteStatus
			f.mu.Unlock()
			if status != 0 {
				http.Error(w, "boom", status)
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

func (f *cliFakeEMSP) sawDelete() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted
}
