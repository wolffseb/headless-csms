package commands_test

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/client"
	"github.com/wolffseb/cli-cpms/internal/ocpi/commands"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// The location has two chargers: A with three EVSEs (the third is not
// reservable) and B with one.
const (
	cpA        = "CP-A"
	cpB        = "CP-B"
	locationID = "SITE-01"
	// commandsToken is the partner's TOKEN_C, and tokenB ours to call it with.
	commandsToken = "partner-token-c"
	tokenB        = "partner-token-b"
	// tag is a token uid that is not auth.default_id_tag.
	tag = "NL-ABC-C00012345-X"
)

var t0 = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

func testConfig() *config.Config {
	no := false
	evse := func(uid, charger string, connector int) config.EVSE {
		return config.EVSE{
			UID: uid, Charger: charger, OCPPConnectorID: connector,
			Connectors: []config.Connector{{ID: "1"}},
		}
	}
	a3 := evse("EVSE-A3", cpA, 3)
	a3.Reservable = &no
	return &config.Config{
		Chargers: []config.Charger{
			{ID: cpA, CallTimeout: config.NewDuration(7 * time.Second)},
			{ID: cpB, CallTimeout: config.NewDuration(3 * time.Second)},
		},
		Auth: config.Auth{DefaultIDTag: "04A1B2C3D4"},
		OCPI: config.OCPI{Push: config.Push{MaxRetries: 3}},
		Location: config.Location{
			ID: locationID,
			EVSEs: []config.EVSE{
				evse("EVSE-A1", cpA, 1), evse("EVSE-A2", cpA, 2), a3,
				evse("EVSE-B1", cpB, 1),
			},
		},
	}
}

// station is a fake charge point. It answers every command Accepted (Unlocked
// for an unlock) unless the test says otherwise, and records what it was sent.
type station struct {
	ocpp.ChargePoint // the rest of the interface is never called
	id               string

	mu       sync.Mutex
	reserves []ocpp.ReserveRequest
	cancels  []int
	starts   []ocpp.RemoteStartRequest
	stops    []string
	unlocks  []string
	calls    map[string]int
	// answer overrides the reply to the nth call (1-based) of an op.
	answer map[string]func(n int) (ocpp.Result, error)
	// hold, per op, makes the call wait for the channel to be closed, after
	// telling entered.
	hold map[string]chan struct{}
	// entered receives the op of every call that arrives.
	entered chan string
}

func newStation(id string) *station {
	return &station{
		id: id, calls: map[string]int{}, answer: map[string]func(int) (ocpp.Result, error){},
		hold: map[string]chan struct{}{}, entered: make(chan string, 16),
	}
}

func (s *station) ID() string            { return s.id }
func (s *station) Version() ocpp.Version { return ocpp.Version16 }

func (s *station) do(ctx context.Context, op string, record func()) (ocpp.Result, error) {
	s.mu.Lock()
	s.calls[op]++
	n := s.calls[op]
	record()
	answer, gate := s.answer[op], s.hold[op]
	s.mu.Unlock()
	select {
	case s.entered <- op:
	default:
	}
	if gate != nil {
		<-gate
	}
	// A real connection gives up on a cancelled context.
	if err := ctx.Err(); err != nil {
		return ocpp.Result{}, err
	}
	if answer != nil {
		return answer(n)
	}
	if op == "unlock" {
		return ocpp.Result{Status: ocpp.CommandUnlocked, Raw: "Unlocked"}, nil
	}
	return ocpp.Result{Status: ocpp.CommandAccepted, Raw: "Accepted"}, nil
}

func (s *station) ReserveNow(ctx context.Context, r ocpp.ReserveRequest) (ocpp.Result, error) {
	return s.do(ctx, "reserve", func() { s.reserves = append(s.reserves, r) })
}

func (s *station) CancelReservation(ctx context.Context, id int) (ocpp.Result, error) {
	return s.do(ctx, "cancel", func() { s.cancels = append(s.cancels, id) })
}

func (s *station) RemoteStart(ctx context.Context, r ocpp.RemoteStartRequest) (ocpp.Result, error) {
	return s.do(ctx, "start", func() { s.starts = append(s.starts, r) })
}

func (s *station) RemoteStop(ctx context.Context, id string) (ocpp.Result, error) {
	return s.do(ctx, "stop", func() { s.stops = append(s.stops, id) })
}

func (s *station) UnlockConnector(ctx context.Context, evse string) (ocpp.Result, error) {
	return s.do(ctx, "unlock", func() { s.unlocks = append(s.unlocks, evse) })
}

func (s *station) setAnswer(op string, fn func(n int) (ocpp.Result, error)) {
	s.mu.Lock()
	s.answer[op] = fn
	s.mu.Unlock()
}

// holdOp makes the next calls of op wait; the returned func lets them go.
func (s *station) holdOp(op string) (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.hold[op] = gate
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// total is how many commands the station got, of any kind.
func (s *station) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

func (s *station) count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

func (s *station) reserved() []ocpp.ReserveRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ocpp.ReserveRequest(nil), s.reserves...)
}

func (s *station) cancelled() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.cancels...)
}

// fleet hands out whichever stations are connected.
type fleet struct {
	mu sync.Mutex
	by map[string]*station
}

func (f *fleet) Command(id string) (ocpp.ChargePoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.by[id]; ok {
		return st, nil
	}
	return nil, ocpp.ErrNotConnected
}

// emsp is the partner: it takes CommandResults on /result/<name>.
type emsp struct {
	srv *httptest.Server

	mu    sync.Mutex
	posts map[string][]post
	// status answers a name's POSTs with that HTTP status; 0 means success.
	status map[string]int
}

type post struct {
	auth string
	body map[string]any
}

func newEMSP(t *testing.T) *emsp {
	t.Helper()
	e := &emsp{posts: map[string][]post{}, status: map[string]int{}}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/result/")
		if !ok || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		p := post{auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &p.body)
		e.mu.Lock()
		e.posts[name] = append(e.posts[name], p)
		status := e.status[name]
		e.mu.Unlock()
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status_code":1000,"timestamp":"2030-01-02T03:04:05Z"}`))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *emsp) url(name string) string { return e.srv.URL + "/result/" + name }

func (e *emsp) got(name string) []post {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]post(nil), e.posts[name]...)
}

func (e *emsp) all() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, p := range e.posts {
		n += len(p)
	}
	return n
}

func (e *emsp) failWith(name string, status int) {
	e.mu.Lock()
	e.status[name] = status
	e.mu.Unlock()
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

// stage wires the stack under test around a set of stations: core, control,
// the Commands module on an OCPI server, and the partner.
type stage struct {
	partner *partnerState
	t       *testing.T
	cfg     *config.Config
	clk     *clocktest.Clock
	core    *core.Service
	ctl     *control.Service
	recv    *commands.Receiver
	srv     *ocpi.Server
	emsp    *emsp
	logs    *syncBuffer
	fleet   *fleet
	cmdURL  string
}

func (s *stage) station(id string) *station { return s.fleet.by[id] }

type stageOptions struct {
	// commander replaces the fake stations, for a stage on the simulator. It
	// is built once core exists, which the OCPP server needs.
	commander func(t *testing.T, cfg *config.Config, svc *core.Service, log *slog.Logger) control.Commander
	// backoff replaces the retry wait; the default is 1ms.
	backoff func(int) time.Duration
	// afterFunc replaces how the retry wait is scheduled, to hold retries
	// until the test advances a clock.
	afterFunc func(time.Duration, func()) commands.Stopper
	cfg       *config.Config
	// noConnect leaves the fake stations disconnected.
	noConnect bool
}

// newStage builds the stack with both chargers connected and their EVSEs
// AVAILABLE, and stations A and B behind them.
func newStage(t *testing.T, opts ...func(*stageOptions)) *stage {
	t.Helper()

	o := stageOptions{backoff: func(int) time.Duration { return time.Millisecond }, cfg: testConfig()}
	for _, f := range opts {
		f(&o)
	}
	cfg := o.cfg
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clk := clocktest.New(t0)
	stopper := func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) }

	svc := core.New(cfg, core.WithClock(clk.Now), core.WithAfterFunc(stopper), core.WithLogger(log))
	t.Cleanup(svc.Close)

	fl := &fleet{by: map[string]*station{cpA: newStation(cpA), cpB: newStation(cpB)}}
	var commander control.Commander = fl
	if o.commander != nil {
		commander = o.commander(t, cfg, svc, log)
	}
	ctl := control.New(control.Options{
		Core: svc, Commands: commander, Config: cfg, Now: clk.Now, Log: log, AfterFunc: stopper,
	})
	t.Cleanup(ctl.Close)

	em := newEMSP(t)
	pt := newPartner(log)
	recv, err := commands.New(commands.Options{
		Config: cfg, Core: svc, Control: ctl, Log: log, Now: clk.Now, Backoff: o.backoff, AfterFunc: o.afterFunc,
		Partner: pt,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ocpi.New(ocpi.Options{
		Bind: "127.0.0.1:0", PublicBaseURL: "http://127.0.0.1:1/ocpi", Log: log,
		Auth: ocpi.AuthenticatorFunc(func(token string) ocpi.Access {
			if token == commandsToken {
				return ocpi.AccessModules
			}
			return ocpi.AccessNone
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recv.Mount(srv); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = recv.Wait(ctx)
	})

	s := &stage{
		t: t, partner: pt.partnerState, cfg: cfg, clk: clk, core: svc, ctl: ctl, recv: recv, srv: srv, emsp: em, logs: logs, fleet: fl,
		cmdURL: "http://" + srv.Addr() + "/ocpi/2.3.0/commands/",
	}
	if o.commander == nil && !o.noConnect {
		for _, cp := range []string{cpA, cpB} {
			s.connect(cp)
		}
	}
	return s
}

// retryClock is a fake clock for the wait before a result is posted again.
type retryClock struct{ *clocktest.Clock }

func newClock() *retryClock { return &retryClock{clocktest.New(t0)} }

func (c *retryClock) afterFunc(d time.Duration, fn func()) commands.Stopper {
	return c.AfterFunc(d, fn)
}

// partner is the registered counterparty: one registration at a time, which
// a test replaces with set.
type partner struct{ *partnerState }

type partnerState struct {
	mu    sync.Mutex
	at    time.Time
	token string
	log   *slog.Logger
	// origin is the one scheme://host:port the partner has; empty allows any.
	origin string
}

func newPartner(log *slog.Logger) partner {
	return partner{&partnerState{at: t0.Add(-time.Hour), token: tokenB, log: log}}
}

// set makes another registration the current one.
func (p *partnerState) set(at time.Time, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at, p.token = at, token
}

// onlyAt restricts the partner's response URLs to those under origin.
func (p *partnerState) onlyAt(origin string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.origin = origin
}

func (p *partnerState) AllowsResponseURL(registration time.Time, responseURL string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return registration.Equal(p.at) && (p.origin == "" || strings.HasPrefix(responseURL, p.origin+"/"))
}

func (p *partnerState) Registered() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.at
}

func (p *partnerState) ClientFor(registration time.Time) (*client.Client, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !registration.Equal(p.at) {
		return nil, false
	}
	token := p.token
	return client.New(client.Options{Token: func() string { return token }, Log: p.log}), true
}

// connect brings a charger online with every EVSE AVAILABLE.
func (s *stage) connect(cp string) {
	s.core.Connected(cp)
	for _, e := range s.cfg.EVSEsOf(cp) {
		s.core.SetConnectorStatus(cp, e.OCPPConnectorID, core.StatusAvailable, "")
	}
}

// setStatus is a StatusNotification for an EVSE.
func (s *stage) setStatus(evse string, st core.EVSEStatus) {
	s.t.Helper()
	e, ok := s.cfg.EVSEByUID(evse)
	if !ok {
		s.t.Fatalf("no EVSE %s", evse)
	}
	s.core.SetConnectorStatus(s.cfg.ChargerOf(e), e.OCPPConnectorID, st, "")
}

// reply is an OCPI answer to a command: the HTTP status, the envelope's
// status_code, and the CommandResponse in its data.
type reply struct {
	http   int
	code   int
	result string
	// timeout is the CommandResponse's, -1 when the answer has none.
	timeout int
	message string
}

// post sends a command and decodes the answer.
func (s *stage) post(command string, body any) reply {
	s.t.Helper()

	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(http.MethodPost, s.cmdURL+command, bytes.NewReader(raw))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(commandsToken)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var env struct {
		Data struct {
			Result  string `json:"result"`
			Timeout *int   `json:"timeout"`
			Message []struct {
				Language string `json:"language"`
				Text     string `json:"text"`
			} `json:"message"`
		} `json:"data"`
		StatusCode int `json:"status_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		s.t.Fatalf("decoding the answer to %s: %v", command, err)
	}
	r := reply{http: resp.StatusCode, code: env.StatusCode, result: env.Data.Result, timeout: -1}
	if env.Data.Timeout != nil {
		r.timeout = *env.Data.Timeout
	}
	for _, m := range env.Data.Message {
		r.message += m.Text
	}
	return r
}

// reserveBody is a valid RESERVE_NOW for EVSE-A1 whose result goes to name,
// with overrides applied (a nil value drops the key).
func (s *stage) reserveBody(name string, override map[string]any) map[string]any {
	b := map[string]any{
		"response_url":   s.emsp.url(name),
		"token":          map[string]any{"uid": tag, "type": "RFID", "contract_id": "NL-ABC-C00012345"},
		"expiry_date":    ocpi.FormatDateTime(t0.Add(30 * time.Minute)),
		"reservation_id": "RES-1",
		"location_id":    locationID,
		"evse_uid":       "EVSE-A1",
	}
	return with(b, override)
}

func (s *stage) startBody(name string, override map[string]any) map[string]any {
	b := map[string]any{
		"response_url": s.emsp.url(name),
		"token":        map[string]any{"uid": tag},
		"location_id":  locationID,
		"evse_uid":     "EVSE-A1",
	}
	return with(b, override)
}

func (s *stage) unlockBody(name string, override map[string]any) map[string]any {
	b := map[string]any{
		"response_url": s.emsp.url(name),
		"location_id":  locationID,
		"evse_uid":     "EVSE-A1",
		"connector_id": "1",
	}
	return with(b, override)
}

func with(b, override map[string]any) map[string]any {
	for k, v := range override {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	return b
}

// result waits for the CommandResult posted to name, and returns its result
// type and message.
func (s *stage) result(name string) (string, string) {
	s.t.Helper()
	eventually(s.t, "a CommandResult at "+name, func() bool { return len(s.emsp.got(name)) > 0 })
	p := s.emsp.got(name)[0]
	res, _ := p.body["result"].(string)
	var msg string
	if ms, ok := p.body["message"].([]any); ok {
		for _, m := range ms {
			if mm, ok := m.(map[string]any); ok {
				msg += fmt.Sprint(mm["text"])
			}
		}
	}
	return res, msg
}

// quiet fails if anything was POSTed to the partner or sent to a station,
// after giving a stray background goroutine time to do so.
func (s *stage) quiet(what string) {
	s.t.Helper()
	time.Sleep(150 * time.Millisecond)
	if n := s.emsp.all(); n != 0 {
		s.t.Errorf("%s: the partner got %d CommandResults, want none", what, n)
	}
	for id, st := range s.fleet.by {
		if n := st.total(); n != 0 {
			s.t.Errorf("%s: station %s got %d commands, want none", what, id, n)
		}
	}
}

// eventually polls cond in real time, for things that happen on another
// goroutine.
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
