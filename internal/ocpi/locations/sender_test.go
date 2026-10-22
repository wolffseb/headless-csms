package locations_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/locations"
)

// harness is an OCPI server with the Locations module, on a free port, in
// front of a core whose clock the test moves.
type harness struct {
	t     *testing.T
	cfg   *config.Config
	core  *core.Service
	clock *clocktest.Clock
	srv   *ocpi.Server
	// registered flips what TOKEN_C grants, standing in for registration
	// until the credentials module's Authenticator exists.
	registered atomic.Bool
}

// stubAuth grants what the registration-aware Authenticator will: TOKEN_A
// reaches registration, TOKEN_C reaches modules once registered.
func (h *harness) stubAuth() ocpi.Authenticator {
	a, c := ocpi.NewSecret(h.cfg.OCPI.TokenA), ocpi.NewSecret(h.cfg.OCPI.TokenC)
	return ocpi.AuthenticatorFunc(func(token string) ocpi.Access {
		switch {
		case a.Matches(token):
			return ocpi.AccessRegistration
		case c.Matches(token) && h.registered.Load():
			return ocpi.AccessModules
		}
		return ocpi.AccessNone
	})
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		t:   t,
		cfg: loadConfig(t, filepath.Join("testdata", "config.yaml"), staticAt),
		// A fraction of a second past the hour, which the wire format drops.
		clock: clocktest.New(staticAt.Add(time.Hour + 400*time.Millisecond)),
	}
	h.registered.Store(true)
	h.core = core.New(h.cfg, core.WithClock(h.clock.Now))

	srv, err := ocpi.New(ocpi.Options{
		Bind:          "127.0.0.1:0",
		PublicBaseURL: h.cfg.OCPI.PublicBaseURL,
		Auth:          h.stubAuth(),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := locations.NewSender(locations.Options{
		Config: h.cfg, Core: h.core, URL: srv.VersionURL() + "/locations",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Register(sender.Module()); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	h.srv = srv
	return h
}

// envelope is an OCPI response with its data left raw.
type envelope struct {
	StatusCode int             `json:"status_code"`
	Data       json.RawMessage `json:"data"`

	HTTPStatus int         `json:"-"`
	Header     http.Header `json:"-"`
}

// get requests a path below the version root ("/locations/LOC-1") with a
// token ("" for none), the way a peer would: at the bound address, under the
// advertised path.
func (h *harness) get(path, token string) envelope {
	h.t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://"+h.srv.Addr()+"/ocpi/2.3.0"+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		h.t.Fatalf("GET %s: decoding: %v", path, err)
	}
	env.HTTPStatus, env.Header = resp.StatusCode, resp.Header
	return env
}

// ok GETs with TOKEN_C, requires 200/1000, decodes data into v and returns
// the headers.
func (h *harness) ok(path string, v any) http.Header {
	h.t.Helper()

	env := h.get(path, h.cfg.OCPI.TokenC)
	if env.HTTPStatus != http.StatusOK || env.StatusCode != int(ocpi.StatusSuccess) {
		h.t.Fatalf("GET %s: HTTP %d, status_code %d", path, env.HTTPStatus, env.StatusCode)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		h.t.Fatalf("GET %s: data: %v", path, err)
	}
	return env.Header
}

// jsonLocation is what a client decodes; the package's own types only marshal.
type jsonLocation struct {
	ID          string     `json:"id"`
	LastUpdated string     `json:"last_updated"`
	EVSEs       []jsonEVSE `json:"evses"`
}

type jsonEVSE struct {
	UID         string          `json:"uid"`
	Status      string          `json:"status"`
	LastUpdated string          `json:"last_updated"`
	Connectors  []jsonConnector `json:"connectors"`
}

type jsonConnector struct {
	ID       string `json:"id"`
	Standard string `json:"standard"`
}

func TestModuleAccessIsRequired(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.registered.Store(false)
	path := "/locations"

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"TOKEN_A", h.cfg.OCPI.TokenA},
		{"TOKEN_C before registration", h.cfg.OCPI.TokenC},
	} {
		if env := h.get(path, tc.token); env.HTTPStatus != http.StatusUnauthorized {
			t.Errorf("%s: HTTP %d, want 401", tc.name, env.HTTPStatus)
		}
	}

	h.registered.Store(true)
	var list []jsonLocation
	h.ok(path, &list)
	if len(list) != 1 {
		t.Errorf("TOKEN_C after registration: %d locations, want 1", len(list))
	}
}

func TestVersionDetailsListLocationsAsSender(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var details struct {
		Endpoints []struct {
			Identifier, Role, URL string
		} `json:"endpoints"`
	}
	env := h.get("", h.cfg.OCPI.TokenA)
	if env.HTTPStatus != http.StatusOK {
		t.Fatalf("version details: HTTP %d", env.HTTPStatus)
	}
	if err := json.Unmarshal(env.Data, &details); err != nil {
		t.Fatal(err)
	}
	for _, e := range details.Endpoints {
		if e.Identifier == "locations" && e.Role == "SENDER" && e.URL == h.srv.VersionURL()+"/locations" {
			return
		}
	}
	t.Errorf("locations/SENDER not in version details: %+v", details.Endpoints)
}

func TestSubObjectEndpoints(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	l := h.cfg.Location
	evse := l.EVSEs[1]
	conn := evse.Connectors[1]

	var loc jsonLocation
	h.ok("/locations/"+l.ID, &loc)
	if loc.ID != l.ID || len(loc.EVSEs) != len(l.EVSEs) {
		t.Errorf("location = %s with %d EVSEs, want %s with %d", loc.ID, len(loc.EVSEs), l.ID, len(l.EVSEs))
	}

	var e jsonEVSE
	h.ok("/locations/"+l.ID+"/"+evse.UID, &e)
	if e.UID != evse.UID || len(e.Connectors) != len(evse.Connectors) {
		t.Errorf("evse = %+v, want %s", e, evse.UID)
	}

	var c jsonConnector
	h.ok("/locations/"+l.ID+"/"+evse.UID+"/"+conn.ID, &c)
	if c.ID != conn.ID || c.Standard != conn.Standard {
		t.Errorf("connector = %+v, want %s %s", c, conn.ID, conn.Standard)
	}

	// Ids are CiStrings: case does not matter.
	h.ok("/locations/"+lower(l.ID)+"/"+lower(evse.UID), &e)
	if e.UID != evse.UID {
		t.Errorf("lowercase lookup found %q, want %q", e.UID, evse.UID)
	}

	for _, path := range []string{
		"/locations/NOPE",
		"/locations/" + l.ID + "/NOPE",
		"/locations/" + l.ID + "/" + evse.UID + "/NOPE",
		// A connector id from another EVSE is still unknown under this one.
		"/locations/" + l.ID + "/" + l.EVSEs[0].UID + "/" + conn.ID,
	} {
		env := h.get(path, h.cfg.OCPI.TokenC)
		if env.HTTPStatus != http.StatusNotFound || env.StatusCode != int(ocpi.StatusUnknownLocation) {
			t.Errorf("GET %s: HTTP %d, status_code %d; want 404, 2003", path, env.HTTPStatus, env.StatusCode)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func TestPaginationOverTheOneLocation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	check := func(query string, wantLen int, wantLimit string) {
		t.Helper()
		var list []jsonLocation
		hdr := h.ok("/locations"+query, &list)
		if len(list) != wantLen {
			t.Errorf("%s: %d locations, want %d", query, len(list), wantLen)
		}
		if got := hdr.Get("X-Total-Count"); got != "1" {
			t.Errorf("%s: X-Total-Count = %q, want 1", query, got)
		}
		if got := hdr.Get("X-Limit"); got != wantLimit {
			t.Errorf("%s: X-Limit = %q, want %s", query, got, wantLimit)
		}
		if got := hdr.Get("Link"); got != "" {
			t.Errorf("%s: Link = %q, want none on the last page", query, got)
		}
	}

	check("?offset=0&limit=1", 1, "1")
	check("?offset=1&limit=1", 0, "1")
	check("?limit=1000", 1, strconv.Itoa(locations.MaxLimit))
	check("", 1, strconv.Itoa(locations.MaxLimit))
	check("?offset=50", 0, strconv.Itoa(locations.MaxLimit))

	// An empty page is still a list, not a missing field: past the end, and
	// when the filter matches nothing at all.
	for _, q := range []string{"?offset=1", "?date_from=2999-01-01T00:00:00Z"} {
		if env := h.get("/locations"+q, h.cfg.OCPI.TokenC); string(env.Data) != "[]" {
			t.Errorf("%s: data = %s, want []", q, env.Data)
		}
	}
}

func TestDateFilters(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var loc jsonLocation
	h.ok("/locations/"+h.cfg.Location.ID, &loc)
	updated, err := time.Parse(time.RFC3339, loc.LastUpdated)
	if err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) string { return ocpi.FormatDateTime(updated.Add(d)) }

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?date_from=" + at(0), 1},           // inclusive
		{"?date_from=" + at(time.Second), 0}, // after it
		{"?date_to=" + at(0), 0},             // exclusive
		{"?date_to=" + at(time.Second), 1},
		{"?date_from=" + at(-time.Hour) + "&date_to=" + at(time.Hour), 1},
		// The core clock stands 400ms past the second the client sees. The
		// filter uses what the client sees, so a date_to just past that
		// second, with a fraction, includes the Location.
		{"?date_to=" + updated.Add(200*time.Millisecond).Format("2006-01-02T15:04:05.000Z"), 1},
		{"?date_from=" + updated.Add(200*time.Millisecond).Format("2006-01-02T15:04:05.000Z"), 0},
		// No zone designator means UTC.
		{"?date_to=" + updated.UTC().Format("2006-01-02T15:04:05"), 0},
	} {
		var list []jsonLocation
		hdr := h.ok("/locations"+tc.query, &list)
		if len(list) != tc.want {
			t.Errorf("%s: %d locations, want %d", tc.query, len(list), tc.want)
		}
		if got := hdr.Get("X-Total-Count"); got != strconv.Itoa(tc.want) {
			t.Errorf("%s: X-Total-Count = %q, want %d", tc.query, got, tc.want)
		}
	}
}

func TestMalformedQueryIs2001(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for _, q := range []string{
		"?date_from=yesterday",
		"?date_to=2026-13-01T00:00:00Z",
		"?offset=-1",
		"?offset=two",
		"?limit=0",
		"?limit=-5",
	} {
		env := h.get("/locations"+q, h.cfg.OCPI.TokenC)
		if env.HTTPStatus != http.StatusBadRequest || env.StatusCode != int(ocpi.StatusInvalidParameters) {
			t.Errorf("%s: HTTP %d, status_code %d; want 400, 2001", q, env.HTTPStatus, env.StatusCode)
		}
	}
}

// TestDisconnectReadsUnknownOverHTTP asserts at the HTTP layer what core
// decides: a station we cannot reach has every EVSE UNKNOWN.
func TestDisconnectReadsUnknownOverHTTP(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	cpID := h.cfg.Charger.ID
	h.core.Connected(cpID)
	for _, e := range h.cfg.Location.EVSEs {
		h.core.SetConnectorStatus(cpID, e.OCPPConnectorID, core.StatusAvailable, "NoError")
	}
	statuses := func() []string {
		var loc jsonLocation
		h.ok("/locations/"+h.cfg.Location.ID, &loc)
		out := make([]string, 0, len(loc.EVSEs))
		for _, e := range loc.EVSEs {
			out = append(out, e.Status)
		}
		return out
	}
	for _, s := range statuses() {
		if s != string(core.StatusAvailable) {
			t.Fatalf("connected: statuses %v, want all AVAILABLE", statuses())
		}
	}

	h.clock.Advance(time.Minute)
	h.core.Disconnected(cpID, "test")
	for _, s := range statuses() {
		if s != string(core.StatusUnknown) {
			t.Fatalf("disconnected: statuses %v, want all UNKNOWN", statuses())
		}
	}
}
