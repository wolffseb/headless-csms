package ocpi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// exampleConfig is the shipped example. Values are read from it rather than
// copied into the tests, so editing the example cannot silently desync them.
func exampleConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("loading config.example.yaml: %v", err)
	}
	return cfg
}

type fixture struct {
	t      *testing.T
	cfg    *config.Config
	server *ocpi.Server
	http   *httptest.Server
}

// newFixture serves a Server with the static authenticator and the
// credentials stub, as cpms run wires it. edit may adjust the options and
// register further modules before the server starts serving.
func newFixture(t *testing.T, edit func(*ocpi.Options), modules ...ocpi.Module) *fixture {
	t.Helper()

	cfg := exampleConfig(t)
	opts := ocpi.Options{
		PublicBaseURL: cfg.OCPI.PublicBaseURL,
		Auth:          ocpi.NewStaticAuthenticator(cfg.OCPI.TokenA),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if edit != nil {
		edit(&opts)
	}
	s, err := ocpi.New(opts)
	if err != nil {
		t.Fatalf("ocpi.New: %v", err)
	}
	for _, m := range append([]ocpi.Module{ocpi.CredentialsStub()}, modules...) {
		if err := s.Register(m); err != nil {
			t.Fatalf("Register(%s): %v", m.Identifier, err)
		}
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return &fixture{t: t, cfg: cfg, server: s, http: h}
}

// tokenHeader is the header value a conforming peer sends for token.
func tokenHeader(token string) string {
	return "Token " + base64.StdEncoding.EncodeToString([]byte(token))
}

type envelope struct {
	Data          json.RawMessage `json:"data"`
	StatusCode    int             `json:"status_code"`
	StatusMessage string          `json:"status_message"`
	Timestamp     string          `json:"timestamp"`
}

type reply struct {
	status int
	header http.Header
	body   []byte
	env    envelope
}

// do sends a request to path (below the served root, e.g. "/ocpi/versions")
// with the given Authorization value ("" sends none).
func (f *fixture) do(method, path, auth string, body io.Reader, headers ...string) reply {
	f.t.Helper()

	req, err := http.NewRequest(method, f.http.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}

	r := reply{status: resp.StatusCode, header: resp.Header, body: raw}
	if err := json.Unmarshal(raw, &r.env); err != nil {
		f.t.Fatalf("%s %s: body is not an envelope: %v\n%s", method, path, err, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Errorf("%s %s: Content-Type %q, want application/json", method, path, ct)
	}
	return r
}

// get is do for a GET with TOKEN_A.
func (f *fixture) get(path string) reply {
	f.t.Helper()
	return f.do(http.MethodGet, path, tokenHeader(f.cfg.OCPI.TokenA), nil)
}

// root is the served path of the advertised base URL, e.g. "/ocpi".
func (f *fixture) root() string {
	f.t.Helper()
	return strings.TrimSuffix(mustPath(f.t, f.cfg.OCPI.PublicBaseURL), "/")
}

func mustPath(t *testing.T, raw string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return req.URL.Path
}

func TestUnauthorizedAnswersGiveNoHint(t *testing.T) {
	t.Parallel()

	pinned := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, func(o *ocpi.Options) { o.Now = func() time.Time { return pinned } })
	versions := f.root() + "/versions"

	cases := map[string]string{
		"no header":       "",
		"raw TOKEN_A":     "Token " + f.cfg.OCPI.TokenA,
		"unknown token":   tokenHeader("not-a-token-we-issued"),
		"TOKEN_C":         tokenHeader(f.cfg.OCPI.TokenC),
		"other scheme":    "Bearer " + base64.StdEncoding.EncodeToString([]byte(f.cfg.OCPI.TokenA)),
		"empty token":     "Token ",
		"URL-safe base64": "Token " + base64.URLEncoding.EncodeToString([]byte("ab>cd?"+f.cfg.OCPI.TokenA)),
	}

	var first []byte
	for name, auth := range cases {
		r := f.do(http.MethodGet, versions, auth, nil)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s: HTTP %d, want 401", name, r.status)
		}
		if r.env.StatusCode != int(ocpi.StatusClientError) {
			t.Errorf("%s: status_code %d, want 2000", name, r.env.StatusCode)
		}
		if first == nil {
			first = r.body
		} else if !bytes.Equal(r.body, first) {
			t.Errorf("%s: body differs from the others, which tells a caller why it failed:\n%s\nvs\n%s", name, r.body, first)
		}
	}
}

func TestTokensAreStrictStandardBase64(t *testing.T) {
	t.Parallel()

	// A token whose standard encoding uses both characters the URL-safe
	// alphabet replaces, so mixing up the two encodings cannot pass.
	const token = "ab>cd?token-a"
	std := base64.StdEncoding.EncodeToString([]byte(token))
	if !strings.Contains(std, "+") || !strings.Contains(std, "/") {
		t.Fatalf("test token encodes to %q, which lacks + or /", std)
	}

	f := newFixture(t, func(o *ocpi.Options) { o.Auth = ocpi.NewStaticAuthenticator(token) })
	versions := f.root() + "/versions"

	if r := f.do(http.MethodGet, versions, "Token "+std, nil); r.status != http.StatusOK {
		t.Errorf("standard Base64: HTTP %d, want 200", r.status)
	}
	if r := f.do(http.MethodGet, versions, "token "+std, nil); r.status != http.StatusOK {
		t.Errorf("lowercase scheme: HTTP %d, want 200 (auth schemes are case-insensitive)", r.status)
	}
	urlSafe := base64.URLEncoding.EncodeToString([]byte(token))
	if r := f.do(http.MethodGet, versions, "Token "+urlSafe, nil); r.status != http.StatusUnauthorized {
		t.Errorf("URL-safe Base64: HTTP %d, want 401", r.status)
	}
	unpadded := strings.TrimRight(std, "=")
	if unpadded != std {
		if r := f.do(http.MethodGet, versions, "Token "+unpadded, nil); r.status != http.StatusUnauthorized {
			t.Errorf("unpadded Base64: HTTP %d, want 401", r.status)
		}
	}
	if r := f.do(http.MethodGet, versions, "Token "+token, nil); r.status != http.StatusUnauthorized {
		t.Errorf("raw token: HTTP %d, want 401 (no raw-token fallback)", r.status)
	}
}

func TestVersionsListsOneVersionUnderTheBaseURL(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	r := f.get(f.root() + "/versions")
	if r.status != http.StatusOK || r.env.StatusCode != int(ocpi.StatusSuccess) {
		t.Fatalf("HTTP %d, status_code %d; want 200 and 1000\n%s", r.status, r.env.StatusCode, r.body)
	}

	var versions []struct {
		Version string `json:"version"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(r.env.Data, &versions); err != nil {
		t.Fatalf("data: %v\n%s", err, r.body)
	}
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want exactly one: %s", len(versions), r.env.Data)
	}
	if versions[0].Version != "2.3.0" {
		t.Errorf("version %q, want 2.3.0", versions[0].Version)
	}
	if want := f.cfg.OCPI.PublicBaseURL + "/2.3.0"; versions[0].URL != want {
		t.Errorf("url %q, want %q", versions[0].URL, want)
	}
	if want := f.cfg.OCPI.PublicBaseURL + "/versions"; f.server.VersionsURL() != want {
		t.Errorf("VersionsURL() = %q, want %q", f.server.VersionsURL(), want)
	}
}

type endpoint struct {
	Identifier string `json:"identifier"`
	Role       string `json:"role"`
	URL        string `json:"url"`
}

func (f *fixture) versionDetails() []endpoint {
	f.t.Helper()

	r := f.get(f.root() + "/2.3.0")
	if r.status != http.StatusOK || r.env.StatusCode != int(ocpi.StatusSuccess) {
		f.t.Fatalf("version details: HTTP %d, status_code %d\n%s", r.status, r.env.StatusCode, r.body)
	}
	var details struct {
		Version   string     `json:"version"`
		Endpoints []endpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(r.env.Data, &details); err != nil {
		f.t.Fatalf("data: %v\n%s", err, r.body)
	}
	if details.Version != "2.3.0" {
		f.t.Errorf("version %q, want 2.3.0", details.Version)
	}
	return details.Endpoints
}

func TestVersionDetailsListCredentialsAsSender(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	endpoints := f.versionDetails()

	want := endpoint{"credentials", "SENDER", f.cfg.OCPI.PublicBaseURL + "/2.3.0/credentials"}
	if len(endpoints) != 1 || endpoints[0] != want {
		t.Fatalf("endpoints = %+v, want exactly %+v", endpoints, want)
	}

	// Until the handshake exists, the stub answers rather than 404ing, so a
	// peer sees the module is there but not ready.
	r := f.do(http.MethodPost, mustPath(t, want.URL), tokenHeader(f.cfg.OCPI.TokenA), strings.NewReader("{}"))
	if r.status != http.StatusNotImplemented {
		t.Errorf("POST credentials: HTTP %d, want 501", r.status)
	}
	if r := f.do(http.MethodGet, mustPath(t, want.URL), "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("credentials without a token: HTTP %d, want 401", r.status)
	}
}

func TestRoutesFollowThePublicBaseURLPath(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(o *ocpi.Options) { o.PublicBaseURL = "http://x/some/prefix/" })

	r := f.get("/some/prefix/versions")
	if r.status != http.StatusOK {
		t.Fatalf("GET /some/prefix/versions: HTTP %d\n%s", r.status, r.body)
	}
	if !strings.Contains(string(r.env.Data), `"http://x/some/prefix/2.3.0"`) {
		t.Errorf("advertised version URL should sit under the new base: %s", r.env.Data)
	}
	if r := f.get("/some/prefix/2.3.0/credentials"); r.status != http.StatusNotImplemented {
		t.Errorf("credentials under the new prefix: HTTP %d, want 501", r.status)
	}

	// The example config's path is a different one; it must no longer serve.
	if old := mustPath(t, f.cfg.OCPI.PublicBaseURL) + "/versions"; old != "/some/prefix/versions" {
		if r := f.get(old); r.status != http.StatusNotFound {
			t.Errorf("GET %s: HTTP %d, want 404", old, r.status)
		}
	}
	if r := f.get("/ocpi/versions"); r.status != http.StatusNotFound {
		t.Errorf("GET /ocpi/versions: HTTP %d, want 404", r.status)
	}
}

func TestRootBaseURLServesAtTheRoot(t *testing.T) {
	t.Parallel()

	f := newFixture(t, func(o *ocpi.Options) { o.PublicBaseURL = "http://x" })
	if r := f.get("/versions"); r.status != http.StatusOK {
		t.Fatalf("GET /versions: HTTP %d\n%s", r.status, r.body)
	}
	if got := f.server.VersionURL(); got != "http://x/2.3.0" {
		t.Errorf("VersionURL() = %q", got)
	}
}

func TestTrailingSlashesAreAccepted(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	for _, p := range []string{"/versions/", "/2.3.0/", "/2.3.0/credentials/"} {
		if r := f.get(f.root() + p); r.status == http.StatusNotFound {
			t.Errorf("GET %s: 404", f.root()+p)
		}
	}
}

func TestEveryTimestampIsUTCWithZ(t *testing.T) {
	t.Parallel()

	// A server whose clock runs in another zone must still answer in UTC.
	cest := time.FixedZone("CEST", 2*60*60)
	f := newFixture(t, func(o *ocpi.Options) { o.Now = func() time.Time { return time.Now().In(cest) } }, echoModule())
	root := f.root()

	replies := []reply{
		f.get(root + "/versions"),                         // 200
		f.get(root + "/2.3.0"),                            // 200
		f.do(http.MethodGet, root+"/versions", "", nil),   // 401
		f.get(root + "/2.3.0/credentials"),                // 501
		f.get(root + "/nowhere"),                          // 404
		f.do(http.MethodPatch, root+"/versions", "", nil), // 405
		f.do(http.MethodPost, root+"/2.3.0/echo", tokenHeader(f.cfg.OCPI.TokenA), strings.NewReader("{")), // 400
	}
	for i, r := range replies {
		ts, err := time.Parse(time.RFC3339, r.env.Timestamp)
		if err != nil {
			t.Errorf("reply %d (HTTP %d): timestamp %q is not RFC 3339: %v", i, r.status, r.env.Timestamp, err)
			continue
		}
		if !strings.HasSuffix(r.env.Timestamp, "Z") {
			t.Errorf("reply %d (HTTP %d): timestamp %q does not end in Z", i, r.status, r.env.Timestamp)
		}
		if d := time.Since(ts); d < -time.Minute || d > time.Minute {
			t.Errorf("reply %d: timestamp %q is %s off the wall clock", i, r.env.Timestamp, d)
		}
	}
}

func TestRequestIDsAreEchoedOrGenerated(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	versions := f.root() + "/versions"
	token := tokenHeader(f.cfg.OCPI.TokenA)

	r := f.do(http.MethodGet, versions, token, nil, "X-Request-ID", "req-1", "X-Correlation-ID", "corr-1")
	if got := r.header.Get("X-Request-ID"); got != "req-1" {
		t.Errorf("X-Request-ID = %q, want the one sent", got)
	}
	if got := r.header.Get("X-Correlation-ID"); got != "corr-1" {
		t.Errorf("X-Correlation-ID = %q, want the one sent", got)
	}

	// Without them, each answer still carries both, freshly made. A 401 and a
	// 404 count: debugging those is what the ids are for.
	seen := map[string]bool{}
	for _, r := range []reply{
		f.do(http.MethodGet, versions, token, nil),
		f.do(http.MethodGet, versions, token, nil),
		f.do(http.MethodGet, versions, "", nil),
		f.do(http.MethodGet, f.root()+"/nowhere", "", nil),
	} {
		for _, h := range []string{"X-Request-ID", "X-Correlation-ID"} {
			v := r.header.Get(h)
			if v == "" {
				t.Errorf("HTTP %d: %s missing", r.status, h)
				continue
			}
			if seen[v] {
				t.Errorf("HTTP %d: %s %q was already used", r.status, h, v)
			}
			seen[v] = true
		}
	}
}

// echoModule is a module mounted only by these tests: it proves a module can
// add itself to version details, and gives the body handling a POST route.
func echoModule() ocpi.Module {
	return ocpi.Module{
		Identifier: "echo",
		Role:       ocpi.RoleReceiver,
		Access:     ocpi.AccessRegistration,
		Routes: []ocpi.Route{{
			Method: http.MethodPost,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if !ocpi.DecodeJSON(w, r, &body) {
					return
				}
				ocpi.WriteOK(w, body)
			},
		}},
	}
}

func TestARegisteredModuleIsListedAndServed(t *testing.T) {
	t.Parallel()

	const moduleToken = "module-token-for-tests"
	f := newFixture(t, func(o *ocpi.Options) {
		// The seam a module's own tests use until registration exists: an
		// Authenticator that grants module access to a fixed token.
		static := o.Auth
		o.Auth = ocpi.AuthenticatorFunc(func(token string) ocpi.Access {
			if token == moduleToken {
				return ocpi.AccessModules
			}
			return static.Access(token)
		})
	}, ocpi.Module{
		Identifier: "dummy",
		Role:       ocpi.RoleSender,
		Access:     ocpi.AccessModules,
		Routes: []ocpi.Route{
			{Method: http.MethodGet, Handler: func(w http.ResponseWriter, _ *http.Request) {
				ocpi.WriteOK(w, []string{})
			}},
			{Method: http.MethodGet, Path: "{id}", Handler: func(w http.ResponseWriter, r *http.Request) {
				ocpi.WriteOK(w, map[string]string{"id": r.PathValue("id")})
			}},
		},
	})

	want := endpoint{"dummy", "SENDER", f.cfg.OCPI.PublicBaseURL + "/2.3.0/dummy"}
	found := false
	for _, e := range f.versionDetails() {
		found = found || e == want
	}
	if !found {
		t.Fatalf("version details lack %+v", want)
	}

	dummy := mustPath(t, want.URL)
	r := f.do(http.MethodGet, dummy+"/42", tokenHeader(moduleToken), nil)
	if r.status != http.StatusOK || string(r.env.Data) != `{"id":"42"}` {
		t.Errorf("GET dummy/42: HTTP %d, data %s", r.status, r.env.Data)
	}
	// The spec: TOKEN_A on anything but versions and credentials is a 401.
	if r := f.get(dummy); r.status != http.StatusUnauthorized {
		t.Errorf("GET dummy with TOKEN_A: HTTP %d, want 401", r.status)
	}
	// Module access still reaches versions.
	if r := f.do(http.MethodGet, f.root()+"/versions", tokenHeader(moduleToken), nil); r.status != http.StatusOK {
		t.Errorf("versions with module access: HTTP %d, want 200", r.status)
	}
}

func TestRegisterRejectsClashesAndLateModules(t *testing.T) {
	t.Parallel()

	cfg := exampleConfig(t)
	s, err := ocpi.New(ocpi.Options{
		Bind:          "127.0.0.1:0",
		PublicBaseURL: cfg.OCPI.PublicBaseURL,
		Auth:          ocpi.NewStaticAuthenticator(cfg.OCPI.TokenA),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ocpi.CredentialsStub()); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ocpi.CredentialsStub()); err == nil {
		t.Error("registering credentials twice should fail")
	}
	clash := echoModule()
	clash.Identifier, clash.Path = "other", "credentials"
	clash.Routes[0].Method = http.MethodPost
	if err := s.Register(clash); err == nil {
		t.Error("a module on another module's route should fail")
	}
	public := echoModule()
	public.Access = ocpi.AccessNone
	if err := s.Register(public); err == nil {
		t.Error("a module without an access requirement should fail")
	}

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	if err := s.Register(echoModule()); err == nil {
		t.Error("registering after Start should fail")
	}
}

func TestNewRejectsAnUnusableBaseURL(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "not a url", "http://x/ocpi?a=b", "http://x/{id}", "/ocpi"} {
		_, err := ocpi.New(ocpi.Options{PublicBaseURL: raw, Auth: ocpi.NewStaticAuthenticator("tokentoken")})
		if err == nil {
			t.Errorf("New accepted public base URL %q", raw)
		}
	}
	if _, err := ocpi.New(ocpi.Options{PublicBaseURL: "http://x/ocpi"}); err == nil {
		t.Error("New accepted options without an Authenticator")
	}
}

func TestRequestBodies(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil, echoModule())
	echo := f.root() + "/2.3.0/echo"
	token := tokenHeader(f.cfg.OCPI.TokenA)

	for name, body := range map[string]string{
		"truncated":  `{"a":`,
		"not JSON":   `hello`,
		"empty":      ``,
		"two values": `{} {}`,
	} {
		r := f.do(http.MethodPost, echo, token, strings.NewReader(body))
		if r.status != http.StatusBadRequest || r.env.StatusCode != int(ocpi.StatusInvalidParameters) {
			t.Errorf("%s: HTTP %d, status_code %d; want 400 and 2001", name, r.status, r.env.StatusCode)
		}
	}

	// Peers add vendor fields; refusing them is against the spec.
	r := f.do(http.MethodPost, echo, token, strings.NewReader(`{"known":1,"vendor_extra":{"x":true}}`))
	if r.status != http.StatusOK || r.env.StatusCode != int(ocpi.StatusSuccess) {
		t.Errorf("unknown fields: HTTP %d, status_code %d; want them accepted", r.status, r.env.StatusCode)
	}

	// Just over the cap, and valid JSON, so only the size can be the reason.
	big := `{"pad":"` + strings.Repeat("x", ocpi.MaxBodyBytes) + `"}`
	r = f.do(http.MethodPost, echo, token, strings.NewReader(big))
	if r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: HTTP %d, want 413", r.status)
	}
	// One just under is fine.
	fits := `{"pad":"` + strings.Repeat("x", ocpi.MaxBodyBytes-20) + `"}`
	if r := f.do(http.MethodPost, echo, token, strings.NewReader(fits)); r.status != http.StatusOK {
		t.Errorf("body under the cap: HTTP %d, want 200", r.status)
	}
}

func TestUnmatchedRequestsGetEnvelopes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	r := f.do(http.MethodDelete, f.root()+"/versions", tokenHeader(f.cfg.OCPI.TokenA), nil)
	if r.status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE versions: HTTP %d, want 405", r.status)
	}
	if allow := r.header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to name GET", allow)
	}
	if r := f.do(http.MethodGet, "/elsewhere", "", nil); r.status != http.StatusNotFound || r.env.StatusCode != int(ocpi.StatusClientError) {
		t.Errorf("unknown path: HTTP %d, status_code %d", r.status, r.env.StatusCode)
	}
}

// syncBuffer is a log sink the server may write to from its handlers.
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

func TestTokensNeverReachTheLog(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	f := newFixture(t, func(o *ocpi.Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}, echoModule())
	root := f.root()
	tokenA, tokenC := f.cfg.OCPI.TokenA, f.cfg.OCPI.TokenC
	const unknown = "unknown-token-xyz"

	for _, auth := range []string{tokenHeader(tokenA), "Token " + tokenA, tokenHeader(tokenC), tokenHeader(unknown)} {
		f.do(http.MethodGet, root+"/versions", auth, nil)
		f.do(http.MethodGet, root+"/2.3.0/credentials", auth, nil)
		f.do(http.MethodPost, root+"/2.3.0/echo", auth, strings.NewReader("{"))
		f.do(http.MethodGet, root+"/nowhere", auth, nil)
	}

	out := logs.String()
	if !strings.Contains(out, "ocpi request") {
		t.Fatalf("expected debug request lines, got:\n%s", out)
	}
	for _, secret := range []string{tokenA, tokenC, unknown} {
		for _, form := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret))} {
			if strings.Contains(out, form) {
				t.Errorf("log output contains token %q:\n%s", form, out)
			}
		}
	}
}
