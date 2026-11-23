package push_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/credentials"
	"github.com/wolffseb/cli-cpms/internal/ocpi/push"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// Token B is the partner's to choose, so it is made up here rather than read
// from config.
const tokenB = "emsp-token-b-0001"

// start is the fake clock's first reading. It is far from the real time, so a
// last_updated taken from time.Now would show.
var start = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// eventually polls cond in real time, for things that happen on another
// goroutine. It never waits for a timer under test: those run on the fake
// clock.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
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

// count is how many log lines contain every one of parts.
func (b *syncBuffer) count(parts ...string) int {
	n := 0
	for _, line := range strings.Split(b.String(), "\n") {
		ok := line != ""
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			n++
		}
	}
	return n
}

// patch is one PATCH the fake receiver got.
type patch struct {
	path, auth string
	body       map[string]any
	raw        string
}

func (p patch) status() string { s, _ := p.body["status"].(string); return s }

// answer is what the receiver says to one PATCH. The zero value is success.
type answer struct {
	// httpStatus defaults to 200.
	httpStatus int
	// code is the OCPI status_code in the body; zero means none for an error
	// status and 1000 for a 200.
	code int
}

// fakeEMSP is the partner: it serves versions and version details, so that
// registration works, and records every PATCH on its Locations receiver.
type fakeEMSP struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	patches []patch
	// answers says how to reply to PATCH number n (from 1); nil means 200.
	answers func(n int) answer
	// hold, when set, runs inside the handler after the PATCH is recorded.
	hold func(ctx context.Context)
	// noLocations leaves the locations receiver out of the version details.
	noLocations bool
	// other counts requests that are neither registration nor PATCH.
	other int
	// calls counts every request.
	calls int
}

func newFakeEMSP(t *testing.T) *fakeEMSP {
	t.Helper()
	f := &fakeEMSP{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEMSP) versionsURL() string { return f.srv.URL + "/emsp/versions" }

func (f *fakeEMSP) locationsURL() string { return f.srv.URL + "/emsp/2.3.0/locations" }

func (f *fakeEMSP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/emsp/versions":
		writeEnvelope(w, http.StatusOK, 1000, []map[string]string{{"version": ocpi.Version, "url": f.srv.URL + "/emsp/" + ocpi.Version}})
	case r.Method == http.MethodGet && r.URL.Path == "/emsp/"+ocpi.Version:
		endpoints := []map[string]string{
			{"identifier": "credentials", "role": "SENDER", "url": f.srv.URL + "/emsp/2.3.0/credentials"},
		}
		f.mu.Lock()
		noLocations := f.noLocations
		f.mu.Unlock()
		if !noLocations {
			endpoints = append(endpoints, map[string]string{"identifier": "locations", "role": "RECEIVER", "url": f.locationsURL()})
		}
		writeEnvelope(w, http.StatusOK, 1000, map[string]any{"version": ocpi.Version, "endpoints": endpoints})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/emsp/2.3.0/locations/"):
		f.servePatch(w, r)
	case r.Method == http.MethodDelete && r.URL.Path == "/emsp/2.3.0/credentials":
		writeEnvelope(w, http.StatusOK, 1000, nil)
	default:
		f.mu.Lock()
		f.other++
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

func (f *fakeEMSP) servePatch(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	p := patch{path: r.URL.Path, auth: r.Header.Get("Authorization"), raw: string(raw)}
	_ = json.Unmarshal(raw, &p.body)

	f.mu.Lock()
	f.patches = append(f.patches, p)
	n := len(f.patches)
	answers, hold := f.answers, f.hold
	f.mu.Unlock()

	if hold != nil {
		hold(r.Context())
	}
	var a answer
	if answers != nil {
		a = answers(n)
	}
	if a.httpStatus == 0 {
		a.httpStatus = http.StatusOK
	}
	switch {
	case a.code != 0:
		writeEnvelope(w, a.httpStatus, a.code, nil)
	case a.httpStatus == http.StatusOK:
		writeEnvelope(w, http.StatusOK, 1000, nil)
	default:
		// An error with no envelope: what a proxy or a crashed backend sends.
		w.WriteHeader(a.httpStatus)
	}
}

func writeEnvelope(w http.ResponseWriter, httpStatus, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data, "status_code": code, "status_message": "x", "timestamp": "2030-01-01T00:00:00Z",
	})
}

func (f *fakeEMSP) setAnswers(fn func(n int) answer) {
	f.mu.Lock()
	f.answers = fn
	f.mu.Unlock()
}

func (f *fakeEMSP) setHold(fn func(ctx context.Context)) {
	f.mu.Lock()
	f.hold = fn
	f.mu.Unlock()
}

func (f *fakeEMSP) got() []patch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]patch(nil), f.patches...)
}

func (f *fakeEMSP) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// waitPatches waits for n PATCHes to have arrived and returns all so far.
func (f *fakeEMSP) waitPatches(n int) []patch {
	f.t.Helper()
	eventually(f.t, fmt.Sprintf("PATCH number %d", n), func() bool { return len(f.got()) >= n })
	return f.got()
}

// probeCore is core as the pusher sees it, with two seams: the bus can be
// flushed, which waits until the pusher has handled every event published so
// far, and Drops can be forced.
type probeCore struct {
	*core.Service
	cpID string

	mu     sync.Mutex
	drops  uint64
	marker chan struct{}
}

// Drops reports the forced count for the pusher's subscriber.
func (c *probeCore) Drops() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drops == 0 {
		return nil
	}
	return map[string]uint64{"ocpi-push": c.drops}
}

func (c *probeCore) setDrops(n uint64) {
	c.mu.Lock()
	c.drops = n
	c.mu.Unlock()
}

// Subscribe hands the pusher an unbuffered channel fed by the real one. The
// feeder only moves on once the pusher is back at its select, so when the
// marker event (a heartbeat) has been delivered, every event before it has
// been handled.
func (c *probeCore) Subscribe(name string) (<-chan core.Event, func()) {
	in, cancel := c.Service.Subscribe(name)
	out := make(chan core.Event)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for e := range in {
			select {
			case out <- e:
			case <-done:
				return
			}
			if e.Kind == core.EventHeartbeat {
				select {
				case c.marker <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	return out, func() { once.Do(func() { close(done); cancel() }) }
}

// flush returns once the pusher has handled every event published before it.
func (c *probeCore) flush(t *testing.T) {
	t.Helper()
	select {
	case <-c.marker:
	default:
	}
	c.Heartbeat(c.cpID)
	select {
	case <-c.marker:
	case <-time.After(5 * time.Second):
		t.Fatal("the pusher did not read the bus")
	}
}

// rig is cpms's side: core on a fake clock, the real credentials module on an
// OCPI server, and the pusher, in front of a fake partner.
type rig struct {
	t        *testing.T
	cfg      *config.Config
	clk      *clocktest.Clock
	core     *probeCore
	emsp     *fakeEMSP
	reg      *credentials.Registrar
	ocpiHTTP *httptest.Server
	credsURL string
	logs     *syncBuffer
	pusher   *push.Pusher
	cancel   context.CancelFunc
	debounce time.Duration

	mu       sync.Mutex
	backoffs []int
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("loading config.example.yaml: %v", err)
	}
	if len(cfg.Location.EVSEs) < 2 {
		t.Fatalf("the tests need two EVSEs, config.example.yaml has %d", len(cfg.Location.EVSEs))
	}
	return cfg
}

// newRig builds the rig and starts the pusher, on the fake clock. The partner
// is not registered.
func newRig(t *testing.T, tune ...func(*config.Config)) *rig {
	t.Helper()
	return build(t, false, tune...)
}

// newLiveRig is the rig on the real clock, for the tests that run a station
// against it: nothing there can be stepped by hand.
func newLiveRig(t *testing.T, tune ...func(*config.Config)) *rig {
	t.Helper()
	return build(t, true, tune...)
}

func build(t *testing.T, live bool, tune ...func(*config.Config)) *rig {
	t.Helper()
	cfg := loadConfig(t)
	for _, f := range tune {
		f(cfg)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := &rig{
		t: t, cfg: cfg, clk: clocktest.New(start), logs: logs,
		emsp:     newFakeEMSP(t),
		debounce: cfg.OCPI.Push.Debounce.Duration(),
	}
	var coreOpts []core.Option
	if !live {
		coreOpts = append(coreOpts, core.WithClock(r.clk.Now))
	}
	svc := core.New(cfg, coreOpts...)
	t.Cleanup(svc.Close)
	r.core = &probeCore{Service: svc, cpID: cfg.Chargers[0].ID, marker: make(chan struct{}, 1)}

	r.reg = credentials.New(credentials.Options{
		Store:        statetest.New(t),
		TokenA:       cfg.OCPI.TokenA,
		TokenC:       cfg.OCPI.TokenC,
		CountryCode:  cfg.OCPI.CountryCode,
		PartyID:      cfg.OCPI.PartyID,
		BusinessName: cfg.OCPIBusinessName(),
		Log:          log,
	})
	server, err := ocpi.New(ocpi.Options{PublicBaseURL: cfg.OCPI.PublicBaseURL, Auth: r.reg.Authenticator(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.Mount(server); err != nil {
		t.Fatal(err)
	}
	r.ocpiHTTP = httptest.NewServer(server)
	t.Cleanup(r.ocpiHTTP.Close)
	u, err := url.Parse(server.VersionURL())
	if err != nil {
		t.Fatal(err)
	}
	r.credsURL = r.ocpiHTTP.URL + u.Path + "/credentials"

	opts := push.Options{Config: cfg, Core: r.core, Registration: r.reg, Log: log}
	if !live {
		opts.AfterFunc = func(d time.Duration, fn func()) push.Stopper { return r.clk.AfterFunc(d, fn) }
		opts.Now = r.clk.Now
		// 1s, 2s, 4s, … without jitter, recorded.
		opts.Backoff = func(retry int) time.Duration {
			r.mu.Lock()
			r.backoffs = append(r.backoffs, retry)
			r.mu.Unlock()
			return time.Second << (retry - 1)
		}
	}
	p, err := push.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	r.pusher = p
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	p.Start(ctx)
	t.Cleanup(func() {
		cancel()
		waitCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := p.Wait(waitCtx); err != nil {
			t.Errorf("the pusher did not stop: %v", err)
		}
	})
	return r
}

func (r *rig) backoffCalls() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.backoffs...)
}

// call sends an OCPI request to our server with token.
func (r *rig) call(method, token string, body any) (int, int) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, r.credsURL, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
	resp, err := r.ocpiHTTP.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		StatusCode int `json:"status_code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env.StatusCode
}

// register is the partner's handshake: POST /credentials with TOKEN_A.
func (r *rig) register() {
	r.t.Helper()
	httpStatus, code := r.call(http.MethodPost, r.cfg.OCPI.TokenA, map[string]any{
		"token": tokenB, "url": r.emsp.versionsURL(),
		"roles": []map[string]any{{
			"role": "EMSP", "party_id": "ABC", "country_code": "NL",
			"business_details": map[string]any{"name": "Fake eMSP"},
		}},
	})
	if httpStatus != http.StatusOK || code != 1000 {
		r.t.Fatalf("POST credentials: HTTP %d, status_code %d", httpStatus, code)
	}
}

// unregister is the partner's DELETE /credentials with TOKEN_C.
func (r *rig) unregister() {
	r.t.Helper()
	httpStatus, code := r.call(http.MethodDelete, r.cfg.OCPI.TokenC, nil)
	if httpStatus != http.StatusOK || code != 1000 {
		r.t.Fatalf("DELETE credentials: HTTP %d, status_code %d", httpStatus, code)
	}
}

// station is the charge point's side of the core API.
func (r *rig) setStatus(evse int, status core.EVSEStatus) {
	r.core.SetConnectorStatus(r.cfg.Chargers[0].ID, r.cfg.Location.EVSEs[evse].OCPPConnectorID, status, "NoError")
}

// connect brings the station online with every EVSE Available, and lets the
// pusher see it.
func (r *rig) connect() {
	r.core.Connected(r.cfg.Chargers[0].ID)
	for i := range r.cfg.Location.EVSEs {
		r.setStatus(i, core.StatusAvailable)
	}
	r.core.flush(r.t)
}

// evses is how many EVSEs there are, so how many PATCHes a push of all of
// them makes.
func (r *rig) evses() int { return len(r.cfg.Location.EVSEs) }

// waitTimer waits until the pusher has armed n timers on the fake clock.
func (r *rig) waitTimer(n int) {
	r.t.Helper()
	eventually(r.t, "the pusher's timer", func() bool { return r.clk.Pending() == n })
}

// pushRound flushes the bus, waits for the debounce timers of n EVSEs, and
// fires them.
func (r *rig) pushRound(n int) {
	r.t.Helper()
	r.core.flush(r.t)
	r.waitTimer(n)
	r.clk.Advance(r.debounce)
}

// evsePath is the path a PATCH for the EVSE at index i must have, built from
// config the way OCPI spells it.
func (r *rig) evsePath(i int) string {
	return "/emsp/2.3.0/locations/" + r.cfg.OCPI.CountryCode + "/" + r.cfg.OCPI.PartyID +
		"/" + r.cfg.Location.ID + "/" + r.cfg.Location.EVSEs[i].UID
}

func (r *rig) authHeader() string {
	return "Token " + base64.StdEncoding.EncodeToString([]byte(tokenB))
}
