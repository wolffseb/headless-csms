package credentials_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/credentials"
	"github.com/wolffseb/cli-cpms/internal/state"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// Token B is the counterparty's to choose, so it is made up here rather than
// read from config.
const (
	tokenB    = "emsp-token-b-0001"
	newTokenB = "emsp-token-b-0002"
)

// fakeEMSP is the counterparty: it serves versions and version details and
// records every request.
type fakeEMSP struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []emspRequest
	// versionsAnswer, when set, replaces the versions endpoint's answer.
	versionsAnswer func(w http.ResponseWriter)
	// version is the one version offered; default 2.3.0.
	version string
	// noLocations leaves the locations receiver out of version details.
	noLocations bool
	// deleteAnswer, when set, runs in place of the answer to DELETE credentials.
	deleteAnswer func(w http.ResponseWriter)
}

type emspRequest struct {
	method, path, auth string
}

func newFakeEMSP(t *testing.T) *fakeEMSP {
	t.Helper()
	f := &fakeEMSP{t: t, version: ocpi.Version}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEMSP) versionsURL() string { return f.srv.URL + "/emsp/versions" }

func (f *fakeEMSP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, emspRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
	answer, version, noLocations := f.versionsAnswer, f.version, f.noLocations
	deleteAnswer := f.deleteAnswer
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/emsp/versions":
		if answer != nil {
			answer(w)
			return
		}
		writeEnvelope(w, []map[string]string{{"version": version, "url": f.srv.URL + "/emsp/" + version}})
	case r.Method == http.MethodGet && r.URL.Path == "/emsp/"+ocpi.Version:
		endpoints := []map[string]string{
			{"identifier": "credentials", "role": "SENDER", "url": f.srv.URL + "/emsp/2.3.0/credentials"},
			{"identifier": "tokens", "role": "SENDER", "url": f.srv.URL + "/emsp/2.3.0/tokens"},
		}
		if !noLocations {
			endpoints = append(endpoints, map[string]string{"identifier": "locations", "role": "RECEIVER", "url": f.srv.URL + "/emsp/2.3.0/locations"})
		}
		writeEnvelope(w, map[string]any{"version": ocpi.Version, "endpoints": endpoints})
	case r.Method == http.MethodDelete && r.URL.Path == "/emsp/2.3.0/credentials":
		if deleteAnswer != nil {
			deleteAnswer(w)
			return
		}
		writeEnvelope(w, nil)
	default:
		http.NotFound(w, r)
	}
}

func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data, "status_code": 1000, "status_message": "Success", "timestamp": "2026-10-22T10:00:00Z",
	})
}

func (f *fakeEMSP) got() []emspRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]emspRequest(nil), f.requests...)
}

// syncBuffer is a log sink the race detector is happy with.
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

// harness is our side: an OCPI server with the credentials module, as cpms
// run wires it, over a real state file.
type harness struct {
	t      *testing.T
	cfg    *config.Config
	store  *statetest.Store
	reg    *credentials.Registrar
	server *ocpi.Server
	http   *httptest.Server
	logs   *syncBuffer
	// gate lets a test hold one request between the Authenticator and its
	// handler.
	gate *gatedAuth
}

func exampleConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("loading config.example.yaml: %v", err)
	}
	return cfg
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOn(t, statetest.New(t), &syncBuffer{})
}

// newHarnessOn builds our side over an existing store, which is how a test
// restarts cpms.
func newHarnessOn(t *testing.T, store *statetest.Store, logs *syncBuffer) *harness {
	t.Helper()
	cfg := exampleConfig(t)
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reg := credentials.New(credentials.Options{
		Store:        store,
		TokenA:       cfg.OCPI.TokenA,
		TokenC:       cfg.OCPI.TokenC,
		CountryCode:  cfg.OCPI.CountryCode,
		PartyID:      cfg.OCPI.PartyID,
		BusinessName: cfg.OCPIBusinessName(),
		Log:          log,
	})
	gate := &gatedAuth{inner: reg.Authenticator()}
	s, err := ocpi.New(ocpi.Options{PublicBaseURL: cfg.OCPI.PublicBaseURL, Auth: gate, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Mount(s); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return &harness{t: t, cfg: cfg, store: store, reg: reg, server: s, http: h, logs: logs, gate: gate}
}

type reply struct {
	status int
	header http.Header
	env    struct {
		Data          json.RawMessage `json:"data"`
		StatusCode    int             `json:"status_code"`
		StatusMessage string          `json:"status_message"`
	}
}

// do calls our server at path below the version root ("credentials") or at
// "versions", with token.
func (h *harness) do(method, path, token string, body any) reply {
	h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	case io.Reader:
		r = b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	url := h.server.VersionURL() + "/" + path
	if path == "versions" {
		url = h.server.VersionsURL()
	}
	// The advertised host is the example's LAN address; the test server is
	// local, so only the path is kept.
	url = h.http.URL + strings.TrimPrefix(url, strings.TrimSuffix(h.cfg.OCPI.PublicBaseURL, mustPath(h.t, h.cfg.OCPI.PublicBaseURL)))
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := reply{status: resp.StatusCode, header: resp.Header}
	if err := json.Unmarshal(raw, &out.env); err != nil {
		h.t.Fatalf("%s %s: not an envelope: %v\n%s", method, path, err, raw)
	}
	return out
}

func mustPath(t *testing.T, raw string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req.URL.Path
}

// theirCredentials is what the counterparty POSTs.
func theirCredentials(token, versionsURL string) map[string]any {
	return map[string]any{
		"token": token,
		"url":   versionsURL,
		"roles": []map[string]any{{
			"role": "EMSP", "party_id": "ABC", "country_code": "NL",
			"business_details": map[string]any{"name": "Fake eMSP", "website": "https://emsp.example"},
		}},
	}
}

// register runs the handshake and fails the test unless it succeeds.
func (h *harness) register(emsp *fakeEMSP) {
	h.t.Helper()
	r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
	if r.status != http.StatusOK || r.env.StatusCode != 1000 {
		h.t.Fatalf("POST credentials: HTTP %d, status_code %d (%s)", r.status, r.env.StatusCode, r.env.StatusMessage)
	}
}

// onDisk reads state.json the way a restarted cpms would.
func (h *harness) onDisk() state.State {
	h.t.Helper()
	st, err := state.Open(h.store.Path())
	if err != nil {
		h.t.Fatal(err)
	}
	return st.Get()
}

func (h *harness) noStateFile() {
	h.t.Helper()
	if _, err := os.Stat(h.store.Path()); !errors.Is(err, fs.ErrNotExist) {
		h.t.Errorf("state.json was written (stat: %v)", err)
	}
}

type theirView struct {
	Token string `json:"token"`
	URL   string `json:"url"`
	Roles []struct {
		Role            string `json:"role"`
		PartyID         string `json:"party_id"`
		CountryCode     string `json:"country_code"`
		BusinessDetails struct {
			Name string `json:"name"`
		} `json:"business_details"`
	} `json:"roles"`
}

func decodeCreds(t *testing.T, r reply) theirView {
	t.Helper()
	var v theirView
	if err := json.Unmarshal(r.env.Data, &v); err != nil {
		t.Fatalf("decoding credentials: %v", err)
	}
	return v
}

func TestRegisterHappyPath(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
	if r.status != http.StatusOK || r.env.StatusCode != 1000 {
		t.Fatalf("POST: HTTP %d, status_code %d (%s)", r.status, r.env.StatusCode, r.env.StatusMessage)
	}

	ours := decodeCreds(t, r)
	if ours.Token != h.cfg.OCPI.TokenC {
		t.Errorf("token in our answer is not TOKEN_C")
	}
	if ours.URL != h.server.VersionsURL() {
		t.Errorf("url = %q, want %q", ours.URL, h.server.VersionsURL())
	}
	if len(ours.Roles) != 1 {
		t.Fatalf("roles = %+v, want one", ours.Roles)
	}
	role := ours.Roles[0]
	if role.Role != "CPO" || role.PartyID != h.cfg.OCPI.PartyID || role.CountryCode != h.cfg.OCPI.CountryCode ||
		role.BusinessDetails.Name != h.cfg.OCPIBusinessName() {
		t.Errorf("role = %+v", role)
	}

	// We called back: versions, then the details, both with TOKEN_B.
	got := emsp.got()
	wantAuth := "Token " + base64.StdEncoding.EncodeToString([]byte(tokenB))
	if len(got) != 2 || got[0].path != "/emsp/versions" || got[1].path != "/emsp/2.3.0" {
		t.Fatalf("eMSP got %+v, want GET versions then GET details", got)
	}
	for _, req := range got {
		if req.method != http.MethodGet || req.auth != wantAuth {
			t.Errorf("eMSP got %s %s with the wrong token", req.method, req.path)
		}
	}

	reg := h.onDisk().OCPI
	if reg == nil {
		t.Fatal("state.json holds no registration")
	}
	if reg.TokenB != tokenB || reg.VersionsURL != emsp.versionsURL() || reg.Version != ocpi.Version {
		t.Errorf("stored registration = %+v", reg)
	}
	if len(reg.Endpoints) != 3 {
		t.Errorf("stored endpoints = %+v, want the 3 offered", reg.Endpoints)
	}
	if len(reg.Roles) != 1 || reg.Roles[0] != (state.PartyRole{CountryCode: "NL", PartyID: "ABC", Role: "EMSP"}) {
		t.Errorf("stored roles = %+v", reg.Roles)
	}
	if reg.RegisteredAt.IsZero() {
		t.Error("registered_at is not set")
	}
}

func TestTokensSwapOnRegistration(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	// Before: TOKEN_A gets in, TOKEN_C does not.
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenA, nil); r.status != http.StatusOK {
		t.Errorf("TOKEN_A before registration: HTTP %d", r.status)
	}
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenC, nil); r.status != http.StatusUnauthorized {
		t.Errorf("TOKEN_C before registration: HTTP %d, want 401", r.status)
	}

	h.register(emsp)

	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenA, nil); r.status != http.StatusUnauthorized {
		t.Errorf("TOKEN_A after registration: HTTP %d, want 401", r.status)
	}
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenC, nil); r.status != http.StatusOK {
		t.Errorf("TOKEN_C after registration: HTTP %d, want 200", r.status)
	}
	// Token B is theirs, for us to use: it opens nothing here.
	if r := h.do(http.MethodGet, "versions", tokenB, nil); r.status != http.StatusUnauthorized {
		t.Errorf("TOKEN_B on our side: HTTP %d, want 401", r.status)
	}
}

func TestAuthenticatorGrantsByState(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	auth := h.reg.Authenticator()

	if a := auth.Access(h.cfg.OCPI.TokenA); a != ocpi.AccessRegistration {
		t.Errorf("TOKEN_A unregistered = %s", a)
	}
	if a := auth.Access(h.cfg.OCPI.TokenC); a != ocpi.AccessNone {
		t.Errorf("TOKEN_C unregistered = %s", a)
	}
	h.register(emsp)
	if a := auth.Access(h.cfg.OCPI.TokenA); a != ocpi.AccessNone {
		t.Errorf("TOKEN_A registered = %s", a)
	}
	if a := auth.Access(h.cfg.OCPI.TokenC); a != ocpi.AccessModules {
		t.Errorf("TOKEN_C registered = %s", a)
	}
}

func TestGetAnswersWithTheCallersToken(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	r := h.do(http.MethodGet, "credentials", h.cfg.OCPI.TokenA, nil)
	if r.status != http.StatusOK || decodeCreds(t, r).Token != h.cfg.OCPI.TokenA {
		t.Errorf("GET before registration: HTTP %d, not TOKEN_A", r.status)
	}
	h.register(emsp)
	r = h.do(http.MethodGet, "credentials", h.cfg.OCPI.TokenC, nil)
	if r.status != http.StatusOK || decodeCreds(t, r).Token != h.cfg.OCPI.TokenC {
		t.Errorf("GET after registration: HTTP %d, not TOKEN_C", r.status)
	}
}

func TestSecondPostIs405(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)
	before, calls := h.onDisk(), len(emsp.got())

	r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenC, theirCredentials(newTokenB, emsp.versionsURL()))
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("second POST: HTTP %d, want 405", r.status)
	}
	if allow := r.header.Get("Allow"); allow != "GET, PUT, DELETE" {
		t.Errorf("Allow = %q", allow)
	}
	if after := h.onDisk(); after.OCPI.TokenB != before.OCPI.TokenB || !after.OCPI.RegisteredAt.Equal(before.OCPI.RegisteredAt) {
		t.Errorf("state changed: %+v -> %+v", before.OCPI, after.OCPI)
	}
	if n := len(emsp.got()); n != calls {
		t.Errorf("the eMSP was called back %d more times", n-calls)
	}
}

func TestConcurrentPostsRegisterOnce(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	registered := make(chan bool, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
			registered <- r.status == http.StatusOK && r.env.StatusCode == 1000
		}()
	}
	wg.Wait()
	close(registered)

	count := 0
	for ok := range registered {
		if ok {
			count++
		}
	}
	// The loser was refused as busy (2000), saw the registration (405) or was
	// refused TOKEN_A because it was already gone (401); it never registers
	// twice.
	if count != 1 {
		t.Errorf("%d POSTs registered, want exactly one", count)
	}
	if n := len(emsp.got()); n != 2 {
		t.Errorf("eMSP was called %d times, want one handshake (2)", n)
	}
}

func TestPutUpdatesTheRegistration(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)
	first := h.onDisk().OCPI
	calls := len(emsp.got())

	r := h.do(http.MethodPut, "credentials", h.cfg.OCPI.TokenC, theirCredentials(newTokenB, emsp.versionsURL()))
	if r.status != http.StatusOK || r.env.StatusCode != 1000 {
		t.Fatalf("PUT: HTTP %d, status_code %d (%s)", r.status, r.env.StatusCode, r.env.StatusMessage)
	}
	// By decision, TOKEN_C stays what config says.
	if decodeCreds(t, r).Token != h.cfg.OCPI.TokenC {
		t.Error("PUT did not answer with TOKEN_C")
	}

	got := emsp.got()[calls:]
	wantAuth := "Token " + base64.StdEncoding.EncodeToString([]byte(newTokenB))
	if len(got) != 2 {
		t.Fatalf("PUT made %d calls back, want 2", len(got))
	}
	for _, req := range got {
		if req.auth != wantAuth {
			t.Errorf("PUT called back %s with the old token", req.path)
		}
	}

	reg := h.onDisk().OCPI
	if reg.TokenB != newTokenB {
		t.Error("state.json still holds the old token B")
	}
	if !reg.RegisteredAt.Equal(first.RegisteredAt) {
		t.Errorf("registered_at moved from %s to %s", first.RegisteredAt, reg.RegisteredAt)
	}
	// TOKEN_C still works afterwards.
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenC, nil); r.status != http.StatusOK {
		t.Errorf("TOKEN_C after PUT: HTTP %d", r.status)
	}
}

func TestPutFailureKeepsTheOldRegistration(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)

	emsp.mu.Lock()
	emsp.versionsAnswer = func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }
	emsp.mu.Unlock()
	r := h.do(http.MethodPut, "credentials", h.cfg.OCPI.TokenC, theirCredentials(newTokenB, emsp.versionsURL()))
	if r.env.StatusCode != 3001 {
		t.Errorf("PUT against a broken eMSP: status_code %d, want 3001", r.env.StatusCode)
	}
	if reg := h.onDisk().OCPI; reg == nil || reg.TokenB != tokenB {
		t.Errorf("registration after a failed PUT = %+v, want the old one", reg)
	}
}

func TestPutBeforeRegistrationIs405(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	r := h.do(http.MethodPut, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT before registration: HTTP %d, want 405", r.status)
	}
	if len(emsp.got()) != 0 {
		t.Error("PUT before registration called the eMSP")
	}
	h.noStateFile()
}

func TestDeleteUnregisters(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)

	r := h.do(http.MethodDelete, "credentials", h.cfg.OCPI.TokenC, nil)
	if r.status != http.StatusOK || r.env.StatusCode != 1000 {
		t.Fatalf("DELETE: HTTP %d, status_code %d", r.status, r.env.StatusCode)
	}
	if h.onDisk().OCPI != nil {
		t.Error("state.json still holds the registration")
	}
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenA, nil); r.status != http.StatusOK {
		t.Errorf("TOKEN_A after DELETE: HTTP %d, want 200", r.status)
	}
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenC, nil); r.status != http.StatusUnauthorized {
		t.Errorf("TOKEN_C after DELETE: HTTP %d, want 401", r.status)
	}
	// And the counterparty can register again.
	h.register(emsp)
}

func TestDeleteBeforeRegistrationIs405(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	r := h.do(http.MethodDelete, "credentials", h.cfg.OCPI.TokenA, nil)
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE before registration: HTTP %d, want 405", r.status)
	}
	if allow := r.header.Get("Allow"); allow != "GET, POST" {
		t.Errorf("Allow = %q", allow)
	}
	h.noStateFile()
}

func TestRegistrationFailsWhenTheyCannotBeUsed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer func(w http.ResponseWriter)
		down   bool
	}{
		{name: "down", down: true},
		{name: "HTTP 500", answer: func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }},
		{name: "non-1000 envelope", answer: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status_code":2000,"status_message":"Unauthorized","timestamp":"2026-10-22T10:00:00Z"}`)
		}},
		{name: "not a version list", answer: func(w http.ResponseWriter) { writeEnvelope(w, map[string]string{"a": "b"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, emsp := newHarness(t), newFakeEMSP(t)
			emsp.versionsAnswer = tc.answer
			versionsURL := emsp.versionsURL()
			if tc.down {
				emsp.srv.Close()
			}

			r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, versionsURL))
			if r.env.StatusCode != 3001 {
				t.Errorf("status_code %d (%s), want 3001", r.env.StatusCode, r.env.StatusMessage)
			}
			h.noStateFile()
			if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenA, nil); r.status != http.StatusOK {
				t.Errorf("TOKEN_A after a failed registration: HTTP %d", r.status)
			}
		})
	}
}

func TestRegistrationTimesOutOnASlowPeer(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	emsp.versionsAnswer = func(http.ResponseWriter) { <-release }

	// The inbound request's own deadline bounds the call back, so the test
	// does not wait out the client's 10 s default.
	body, _ := json.Marshal(theirCredentials(tokenB, emsp.versionsURL()))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, h.server.VersionURL()+"/credentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(h.cfg.OCPI.TokenA)))
	rec := httptest.NewRecorder()
	start := time.Now()
	h.server.ServeHTTP(rec, req)

	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("POST took %s against a peer that never answers", d)
	}
	var env struct {
		StatusCode int `json:"status_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.StatusCode != 3001 {
		t.Errorf("status_code %d (%v), want 3001; body %s", env.StatusCode, err, rec.Body)
	}
	h.noStateFile()
}

func TestRegistrationWithoutVersion230Is3002(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	emsp.version = "2.2.1"

	r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
	if r.env.StatusCode != 3002 {
		t.Errorf("status_code %d (%s), want 3002", r.env.StatusCode, r.env.StatusMessage)
	}
	h.noStateFile()
}

func TestRegistrationWithoutLocationsReceiverWarns(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	emsp.noLocations = true

	h.register(emsp)
	if h.onDisk().OCPI == nil {
		t.Fatal("pull-only registration was not stored")
	}
	if !strings.Contains(h.logs.String(), "level=WARN") || !strings.Contains(h.logs.String(), "no locations receiver") {
		t.Errorf("no warning about the missing locations receiver:\n%s", h.logs)
	}
}

func TestInvalidCredentialsAre2001(t *testing.T) {
	t.Parallel()

	valid := func(edit func(map[string]any)) map[string]any {
		c := theirCredentials(tokenB, "http://emsp.invalid/versions")
		edit(c)
		return c
	}
	cases := []struct {
		name string
		body any
	}{
		{"missing token", valid(func(c map[string]any) { delete(c, "token") })},
		{"65-char token", valid(func(c map[string]any) { c["token"] = strings.Repeat("b", 65) })},
		{"token with a space", valid(func(c map[string]any) { c["token"] = "token b" })},
		{"relative url", valid(func(c map[string]any) { c["url"] = "/emsp/versions" })},
		{"empty roles", valid(func(c map[string]any) { c["roles"] = []any{} })},
		{"role without party", valid(func(c map[string]any) { c["roles"] = []any{map[string]any{"role": "EMSP"}} })},
		{"not JSON", "{"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, tc.body)
			if r.status != http.StatusBadRequest || r.env.StatusCode != 2001 {
				t.Errorf("HTTP %d, status_code %d (%s); want 400, 2001", r.status, r.env.StatusCode, r.env.StatusMessage)
			}
			h.noStateFile()
		})
	}
}

func TestA64CharTokenIsAccepted(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	token := strings.Repeat("b", 64)
	r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(token, emsp.versionsURL()))
	if r.status != http.StatusOK || r.env.StatusCode != 1000 {
		t.Errorf("64-char token: HTTP %d, status_code %d (%s)", r.status, r.env.StatusCode, r.env.StatusMessage)
	}
}

func TestStateWriteFailureRegistersNothing(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.store.Fail(true)

	r := h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
	if r.status != http.StatusInternalServerError || r.env.StatusCode != 3000 {
		t.Errorf("HTTP %d, status_code %d; want 500, 3000", r.status, r.env.StatusCode)
	}
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenC, nil); r.status != http.StatusUnauthorized {
		t.Errorf("TOKEN_C after a failed write: HTTP %d, want 401", r.status)
	}
}

func TestRegistrationSurvivesARestart(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)

	// A fresh process: a new store read from the same file, a new server.
	again := newHarnessOn(t, statetest.Open(t, h.store.Path()), &syncBuffer{})
	if r := again.do(http.MethodGet, "versions", again.cfg.OCPI.TokenC, nil); r.status != http.StatusOK {
		t.Errorf("TOKEN_C after restart: HTTP %d, want 200", r.status)
	}
	if r := again.do(http.MethodGet, "versions", again.cfg.OCPI.TokenA, nil); r.status != http.StatusUnauthorized {
		t.Errorf("TOKEN_A after restart: HTTP %d, want 401", r.status)
	}
}

func TestUnregisterForgetsAndTellsThem(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)
	calls := len(emsp.got())

	got, err := h.reg.Unregister(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Party != "NL*ABC" || got.NotifyError != "" {
		t.Errorf("Unregister = %+v", got)
	}
	if h.onDisk().OCPI != nil {
		t.Error("state.json still holds the registration")
	}
	told := emsp.got()[calls:]
	wantAuth := "Token " + base64.StdEncoding.EncodeToString([]byte(tokenB))
	if len(told) != 1 || told[0].method != http.MethodDelete || told[0].path != "/emsp/2.3.0/credentials" || told[0].auth != wantAuth {
		t.Errorf("eMSP got %+v, want one DELETE on its credentials with TOKEN_B", told)
	}
	if r := h.do(http.MethodGet, "versions", h.cfg.OCPI.TokenA, nil); r.status != http.StatusOK {
		t.Errorf("TOKEN_A after Unregister: HTTP %d", r.status)
	}

	if _, err := h.reg.Unregister(context.Background()); !credentials.ErrNotRegistered(err) {
		t.Errorf("second Unregister = %v, want not registered", err)
	}
}

func TestUnregisterForgetsEvenWhenTheyAreDown(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)
	emsp.srv.Close()

	got, err := h.reg.Unregister(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.NotifyError == "" {
		t.Error("NotifyError is empty although the eMSP is down")
	}
	if h.onDisk().OCPI != nil {
		t.Error("state.json still holds the registration")
	}
}

func TestClientUsesTheStoredTokenB(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)
	calls := len(emsp.got())

	if _, err := h.reg.Client().Get(context.Background(), emsp.versionsURL()); err != nil {
		t.Fatal(err)
	}
	if got := emsp.got()[calls:]; len(got) != 1 || got[0].auth != "Token "+base64.StdEncoding.EncodeToString([]byte(tokenB)) {
		t.Errorf("Client sent %+v, want TOKEN_B", got)
	}
}

func TestNoTokenInTheLogs(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)

	// Every path that logs: a failure, a success, an update, an
	// unregistration, all at debug level.
	emsp.version = "2.2.1"
	h.do(http.MethodPost, "credentials", h.cfg.OCPI.TokenA, theirCredentials(tokenB, emsp.versionsURL()))
	emsp.mu.Lock()
	emsp.version, emsp.noLocations = ocpi.Version, true
	emsp.mu.Unlock()
	h.register(emsp)
	h.do(http.MethodPut, "credentials", h.cfg.OCPI.TokenC, theirCredentials(newTokenB, emsp.versionsURL()))
	if _, err := h.reg.Unregister(context.Background()); err != nil {
		t.Fatal(err)
	}

	logs := h.logs.String()
	if !strings.Contains(logs, "ocpi registered") {
		t.Fatalf("the logs do not cover the flow:\n%s", logs)
	}
	for _, tok := range []string{h.cfg.OCPI.TokenA, h.cfg.OCPI.TokenC, tokenB, newTokenB} {
		for _, form := range []string{tok, base64.StdEncoding.EncodeToString([]byte(tok))} {
			if strings.Contains(logs, form) {
				t.Errorf("a token appears in the logs:\n%s", logs)
			}
		}
	}
}

func TestPartyAndPushEndpoint(t *testing.T) {
	t.Parallel()
	reg := &state.OCPIRegistration{
		Roles: []state.PartyRole{
			{CountryCode: "DE", PartyID: "ABC", Role: "EMSP"},
			{CountryCode: "DE", PartyID: "ABC", Role: "NSP"},
			{CountryCode: "NL", PartyID: "XYZ", Role: "EMSP"},
		},
		Endpoints: []state.Endpoint{
			{Identifier: "locations", Role: "SENDER", URL: "http://x/sender"},
			{Identifier: "locations", Role: "RECEIVER", URL: "http://x/receiver"},
		},
	}
	if got := credentials.Party(reg); got != "DE*ABC, NL*XYZ" {
		t.Errorf("Party = %q", got)
	}
	if got, ok := credentials.PushEndpoint(reg); !ok || got != "http://x/receiver" {
		t.Errorf("PushEndpoint = %q, %v", got, ok)
	}
}
