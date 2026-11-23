package cli_test

import (
	"bytes"
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
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// commandsEMSP is a partner that registers, takes the Locations PATCHes, and
// takes the CommandResults posted to the response_urls it handed out.
type commandsEMSP struct {
	srv *httptest.Server

	mu      sync.Mutex
	patches []map[string]any
	results map[string][]commandResult
}

type commandResult struct {
	at   time.Time
	body map[string]any
	// auth is the Authorization header the result came with.
	auth string
}

func newCommandsEMSP(t *testing.T) *commandsEMSP {
	t.Helper()
	f := &commandsEMSP{results: map[string][]commandResult{}}
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
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			f.mu.Lock()
			f.patches = append(f.patches, body)
			f.mu.Unlock()
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/result/"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			name := strings.TrimPrefix(r.URL.Path, "/result/")
			f.mu.Lock()
			f.results[name] = append(f.results[name], commandResult{at: time.Now(), body: body, auth: r.Header.Get("Authorization")})
			f.mu.Unlock()
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "status_code": 1000, "timestamp": "2030-01-02T03:04:05Z"})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *commandsEMSP) resultsAt(name string) []commandResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]commandResult(nil), f.results[name]...)
}

func (f *commandsEMSP) allResults() map[string][]commandResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]commandResult{}
	for name, rs := range f.results {
		out[name] = append([]commandResult(nil), rs...)
	}
	return out
}

func (f *commandsEMSP) patchStatuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.patches {
		s, _ := p["status"].(string)
		out = append(out, s)
	}
	return out
}

// waitResult waits for the first CommandResult at name.
func (f *commandsEMSP) waitResult(t *testing.T, name string) commandResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := f.resultsAt(name); len(got) > 0 {
			return got[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no CommandResult at %s; the partner got %+v", name, f.results)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitPatch waits for a PATCH with the status, after the first n.
func (f *commandsEMSP) waitPatch(t *testing.T, after int, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.patchStatuses()
		for _, s := range got[min(after, len(got)):] {
			if s == status {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no PATCH with status %s after the first %d; the partner got %v", status, after, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// commandsRun starts `cpms run` with the OCPI port chosen, and registers the
// fake partner through POST /credentials. It returns the station and a func
// that POSTs a command with TOKEN_C and returns the decoded envelope.
func commandsRun(t *testing.T, ocppPort, ocpiPort int, emsp *commandsEMSP) (*station, *config.Config, func(command string, body map[string]any) (int, map[string]any)) {
	t.Helper()

	s := &station{
		t: t, port: ocppPort,
		configPath: runConfigPorts(t, ocppPort, ocpiPort),
		socketPath: filepath.Join(shortDir(t), "cpms.sock"),
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s.startRun()
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort)) + "/ocpi/2.3.0/"

	registerPartner(t, base, cfg, emsp, "emsp-token-b")

	post := func(command string, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, base+"commands/"+command, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(cfg.OCPI.TokenC)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var env map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decoding the answer to %s: %v", command, err)
		}
		return resp.StatusCode, env
	}
	return s, cfg, post
}

// registerPartner registers the fake partner through POST /credentials, which
// hands it TOKEN_C, and gives it tokenB for the calls cpms makes to it.
func registerPartner(t *testing.T, base string, cfg *config.Config, emsp *commandsEMSP, tokenB string) {
	t.Helper()

	var reg bytes.Buffer
	_ = json.NewEncoder(&reg).Encode(map[string]any{
		"token": tokenB, "url": emsp.srv.URL + "/versions",
		"roles": []map[string]any{{"role": "EMSP", "party_id": "ABC", "country_code": "NL",
			"business_details": map[string]any{"name": "Fake"}}},
	})
	req, err := http.NewRequest(http.MethodPost, base+"credentials", &reg)
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
}

func reserveNowBody(cfg *config.Config, emsp *commandsEMSP, name, uid string) map[string]any {
	return map[string]any{
		"response_url":   emsp.srv.URL + "/result/" + name,
		"token":          map[string]any{"uid": uid, "type": "RFID"},
		"expiry_date":    ocpi.FormatDateTime(time.Now().Add(30 * time.Minute)),
		"reservation_id": "RES-" + name,
		"location_id":    cfg.Location.ID,
		"evse_uid":       cfg.Location.EVSEs[0].UID,
	}
}

// TestReserveNowOverOCPIReservesTheStation is the second milestone, Fryte to
// cpms to charger, in one run of cpms: a partner registers and sends
// RESERVE_NOW with its own token.
//
//  1. The POST is answered at once, ACCEPTED with a timeout, while the
//     station is still holding the ReserveNow (the simulator sits on every
//     answer for 1.5 s).
//  2. The simulator's connector goes Reserved with the requested token.
//  3. The CommandResult, ACCEPTED, arrives at the response_url.
//  4. A PATCH with RESERVED arrives at the partner's Locations receiver.
//
// The reservation is then redeemed at the station with that token, which is
// not auth.default_id_tag.
func TestReserveNowOverOCPIReservesTheStation(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19630, 19631
	const uid = "NL-ABC-C00012345"
	emsp := newCommandsEMSP(t)
	s, cfg, post := commandsRun(t, ocppPort, ocpiPort, emsp)
	s.connect(func(o *simulator.Options) {
		o.IDTag = uid
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 1500 * time.Millisecond
	})
	patchesBefore := len(emsp.patchStatuses())

	start := time.Now()
	httpStatus, env := post("RESERVE_NOW", reserveNowBody(cfg, emsp, "r1", uid))
	answered := time.Since(start)
	data, _ := env["data"].(map[string]any)
	if httpStatus != 200 || env["status_code"] != float64(1000) || data["result"] != "ACCEPTED" {
		t.Fatalf("1. answer = HTTP %d %v, want 200, 1000, ACCEPTED", httpStatus, env)
	}
	if timeout, _ := data["timeout"].(float64); timeout != 2*30+5 {
		t.Errorf("1. timeout = %v, want 65 (twice the default 30s call_timeout, plus 5)", data["timeout"])
	}
	if answered > 1200*time.Millisecond {
		t.Errorf("1. the answer took %v: it waited for the station", answered)
	}
	if got := s.sim.ReservationID(1); got != 0 {
		t.Errorf("1. the simulator already holds reservation %d: it should still be thinking", got)
	}

	res := emsp.waitResult(t, "r1")
	if res.body["result"] != "ACCEPTED" {
		t.Errorf("3. CommandResult = %v, want ACCEPTED", res.body)
	}
	if res.at.Sub(start) < 1200*time.Millisecond {
		t.Errorf("3. the CommandResult came %v after the command, before the station can have answered", res.at.Sub(start))
	}
	if id := s.sim.ReservationID(1); id == 0 {
		t.Error("2. the simulator holds no reservation")
	}
	emsp.waitPatch(t, patchesBefore, "RESERVED") // 4.

	// Redeem it with the OCPI token: accepted, although it is not the default.
	if err := s.sim.StartTransaction(t.Context(), 1); err != nil {
		t.Fatalf("redeeming the reservation: %v", err)
	}
	if s.sim.TransactionID(1) == 0 {
		t.Fatal("the CSMS refused the reserved token")
	}
	s.waitStatus("the reservation to be consumed by a session", func(st socket.Status) bool {
		e := st.EVSEs[0]
		return e.Transaction != nil && e.Reservation == nil
	})
}

// A reservation made over OCPI that the CPO then cancels in the terminal is
// reported to the partner as CANCELED_RESERVATION on its response_url.
func TestCpmsCancelOfAnOCPIReservationIsReportedToThePartner(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19632, 19633
	emsp := newCommandsEMSP(t)
	s, cfg, post := commandsRun(t, ocppPort, ocpiPort, emsp)
	s.connect()

	if _, env := post("RESERVE_NOW", reserveNowBody(cfg, emsp, "r1", "NL-ABC-C00012345")); env["status_code"] != float64(1000) {
		t.Fatalf("RESERVE_NOW: %v", env)
	}
	if res := emsp.waitResult(t, "r1"); res.body["result"] != "ACCEPTED" {
		t.Fatalf("CommandResult = %v", res.body)
	}

	s.mustCPMS("cancel")

	deadline := time.Now().Add(5 * time.Second)
	for len(emsp.resultsAt("r1")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no CANCELED_RESERVATION; the partner got %+v", emsp.resultsAt("r1"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := emsp.resultsAt("r1")[1].body["result"]; got != "CANCELED_RESERVATION" {
		t.Errorf("second CommandResult = %v, want CANCELED_RESERVATION", got)
	}
}

// A result is for the partner that sent the command. When the operator
// unregisters it and another partner registers, nothing that belongs to the
// first is sent to its response_url with the second's TOKEN_B, and nothing of it
// is sent to the second.
//
// Partner A reserves, cpms unregisters it, B registers, and `cpms cancel` ends
// A's reservation: the CANCELED_RESERVATION that would tell A goes nowhere.
func TestAFormerPartnerNeverGetsTheCurrentPartnersToken(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19634, 19635
	const tokenB = "PARTNER-B-SECRET"
	emspA, emspB := newCommandsEMSP(t), newCommandsEMSP(t)
	s, cfg, post := commandsRun(t, ocppPort, ocpiPort, emspA)
	s.connect()

	if _, env := post("RESERVE_NOW", reserveNowBody(cfg, emspA, "r1", "NL-ABC-C00012345")); env["status_code"] != float64(1000) {
		t.Fatalf("RESERVE_NOW: %v", env)
	}
	if res := emspA.waitResult(t, "r1"); res.body["result"] != "ACCEPTED" {
		t.Fatalf("CommandResult = %v", res.body)
	}

	s.mustCPMS("ocpi", "unregister")
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort)) + "/ocpi/2.3.0/"
	registerPartner(t, base, cfg, emspB, tokenB)

	s.mustCPMS("cancel")

	// The delivery is dropped, and the log says so: waiting for that line is
	// what makes the absence below meaningful.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.runOut.String(), "registration it was for has changed") {
		if time.Now().After(deadline) {
			t.Fatalf("the dropped delivery was never logged; run output:\n%s", s.runOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := emspA.resultsAt("r1"); len(got) != 1 {
		t.Errorf("the former partner got %d results at r1, want only the original ACCEPTED: %+v", len(got), got)
	}
	wantB := "Token " + base64.StdEncoding.EncodeToString([]byte(tokenB))
	for name, results := range emspA.allResults() {
		for _, r := range results {
			if r.auth == wantB {
				t.Errorf("the former partner got a result at %s with the current partner's TOKEN_B", name)
			}
		}
	}
	if got := emspB.allResults(); len(got) != 0 {
		t.Errorf("the current partner got results for a reservation that was not its own: %+v", got)
	}
}
