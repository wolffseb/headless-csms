package tui

import (
	"context"
	"flag"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/core"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func updating() bool { return *update }

// Two chargers with two EVSEs each. Nothing here comes from
// config.example.yaml: the tests own their config.
const rigYAML = `
chargers:
  - id: ALP-HYC-001
  - id: ALP-HYC-002
server:
  ocpp_bind: 127.0.0.1:9000
  ocpi_bind: 127.0.0.1:8080
auth:
  default_id_tag: "04A1B2C3D4"
ocpi:
  country_code: DE
  party_id: ABC
  public_base_url: http://127.0.0.1:8080/ocpi
  token_a: "token-a-secret"
  token_c: "token-c-secret"
location:
  id: SITE-01
  address: Musterstrasse 1
  city: Berlin
  country: DEU
  coordinates: {latitude: "52.520008", longitude: "13.404954"}
  time_zone: Europe/Berlin
  evses:
    - {uid: ALP-HYC-001-1, charger: ALP-HYC-001, evse_id: "DE*ABC*E001*1", ocpp_connector_id: 1, connectors: [{id: "1", standard: IEC_62196_T2, format: SOCKET, power_type: AC_3_PHASE, max_voltage: 400, max_amperage: 32}]}
    - {uid: ALP-HYC-001-2, charger: ALP-HYC-001, evse_id: "DE*ABC*E001*2", ocpp_connector_id: 2, connectors: [{id: "1", standard: IEC_62196_T2, format: SOCKET, power_type: AC_3_PHASE, max_voltage: 400, max_amperage: 32}]}
    - {uid: ALP-HYC-002-1, charger: ALP-HYC-002, evse_id: "DE*ABC*E002*1", ocpp_connector_id: 1, connectors: [{id: "1", standard: IEC_62196_T2, format: SOCKET, power_type: AC_3_PHASE, max_voltage: 400, max_amperage: 32}]}
    - {uid: ALP-HYC-002-2, charger: ALP-HYC-002, evse_id: "DE*ABC*E002*2", ocpp_connector_id: 2, connectors: [{id: "1", standard: IEC_62196_T2, format: SOCKET, power_type: AC_3_PHASE, max_voltage: 400, max_amperage: 32}]}
`

const (
	cp1, cp2       = "ALP-HYC-001", "ALP-HYC-002"
	evse11, evse12 = "ALP-HYC-001-1", "ALP-HYC-001-2"
	evse21         = "ALP-HYC-002-1"
	defaultTag     = "04A1B2C3D4"
)

// fakeControl records what the TUI asks for and answers as told.
type fakeControl struct {
	mu    sync.Mutex
	calls []string
	// gate, when set, holds every call until it is closed (or ctx ends).
	gate chan struct{}
	err  error
	// reserveParams is the last Reserve, whole.
	reserveParams control.ReserveParams
	reserveAt     time.Time
}

func (f *fakeControl) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeControl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeControl) wait(ctx context.Context) error {
	f.mu.Lock()
	gate, err := f.gate, f.err
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeControl) Reserve(ctx context.Context, p control.ReserveParams) (core.Reservation, error) {
	f.record("reserve " + p.EVSEUID)
	f.mu.Lock()
	f.reserveParams = p
	at := f.reserveAt
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return core.Reservation{}, err
	}
	return core.Reservation{ID: 7, EVSEUID: p.EVSEUID, IDTag: p.IDTag, ExpiresAt: at}, nil
}

func (f *fakeControl) Cancel(ctx context.Context, uid string) error {
	f.record("cancel " + uid)
	return f.wait(ctx)
}

func (f *fakeControl) Start(ctx context.Context, uid, tag string) error {
	f.record("start " + uid + " " + tag)
	return f.wait(ctx)
}

func (f *fakeControl) Stop(ctx context.Context, uid, tx string) error {
	f.record("stop " + uid + " tx=" + tx)
	return f.wait(ctx)
}

func (f *fakeControl) Unlock(ctx context.Context, uid string) error {
	f.record("unlock " + uid)
	return f.wait(ctx)
}

func (f *fakeControl) TriggerStatus(ctx context.Context, uid string) error {
	f.record("refresh " + uid)
	return f.wait(ctx)
}

// rig is a Model over a real core.Service on a fake clock.
type rig struct {
	t    *testing.T
	clk  *clocktest.Clock
	core *core.Service
	cfg  *config.Config
	ctl  *fakeControl
	m    *Model
}

func newRig(t *testing.T, mod ...func(*Options)) *rig {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(rigYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	clk := clocktest.New(time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC))
	svc := core.New(cfg, core.WithClock(clk.Now),
		core.WithAfterFunc(func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) }))
	t.Cleanup(svc.Close)

	r := &rig{t: t, clk: clk, core: svc, cfg: cfg, ctl: &fakeControl{reserveAt: clk.Now().Add(15 * time.Minute)}}
	opts := Options{
		Config: cfg, Core: svc, Control: r.ctl,
		Now: clk.Now, Location: time.UTC,
		LogPath: "/var/lib/cpms/cpms.log",
		Describe: func(op socket.Op, subject string, err error) string {
			return subject + " " + string(op) + " failed: " + err.Error()
		},
	}
	for _, f := range mod {
		f(&opts)
	}
	r.m = New(context.Background(), opts)
	t.Cleanup(r.m.Close)
	r.size(100, 30)
	return r
}

func (r *rig) size(w, h int) { r.send(tea.WindowSizeMsg{Width: w, Height: h}) }

func (r *rig) send(msg tea.Msg) tea.Cmd {
	r.t.Helper()
	_, cmd := r.m.Update(msg)
	return cmd
}

// pump delivers the events core has published to the model, as the bus
// bridge would.
func (r *rig) pump() {
	r.t.Helper()
	for {
		select {
		case e := <-r.m.events:
			r.send(eventMsg(e))
		default:
			return
		}
	}
}

// press sends a key and returns the actionMsg its command produces, once
// the call returns (nil when the key started nothing). A call that blocks
// makes press block, so tests that hold the fake use pressAsync.
func (r *rig) press(key string) *actionMsg {
	r.t.Helper()
	ch := r.pressAsync(key)
	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		r.t.Fatalf("press %q: the command did not return", key)
		return nil
	}
}

// pressAsync sends the key and runs what it started in the background.
// The channel yields its actionMsg (or nil at once if nothing started).
func (r *rig) pressAsync(key string) <-chan *actionMsg {
	r.t.Helper()
	out := make(chan *actionMsg, 1)
	cmd := r.send(keyMsg(key))
	if cmd == nil {
		out <- nil
		return out
	}
	// A key may start one command, batched with the spinner's tick. The tick
	// sleeps for its interval and its message is not interesting, so only an
	// actionMsg is kept. Every command runs on its own goroutine, because the
	// fake may hold it.
	found := make(chan *actionMsg, 4)
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			switch msg := c().(type) {
			case tea.BatchMsg:
				for _, sub := range msg {
					run(sub)
				}
			case actionMsg:
				found <- &msg
			}
		}()
	}
	run(cmd)
	go func() {
		select {
		case a := <-found:
			out <- a
		case <-time.After(10 * time.Second):
			out <- nil
		}
	}()
	return out
}

func keyMsg(key string) tea.KeyPressMsg {
	switch key {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: []rune(key)[0], Text: key}
}

// deliver hands a finished command's result to Update.
func (r *rig) deliver(a *actionMsg) {
	r.t.Helper()
	if a == nil {
		r.t.Fatal("no command result to deliver")
	}
	r.send(*a)
}

// view is the frame as text. Goldens hold no colour codes; styles are
// checked on raw (below).
func (r *rig) view() string { return ansi.Strip(r.m.render()) }

// raw is the frame with its escape sequences.
func (r *rig) raw() string { return r.m.render() }

// onlineStation brings a charger up the way a station does.
func (r *rig) onlineStation(cp, vendor, model, fw string, statuses ...core.EVSEStatus) {
	r.core.Connected(cp)
	r.core.Booted(cp, core.BootInfo{Vendor: vendor, Model: model, FirmwareVersion: fw})
	for i, s := range statuses {
		r.core.SetConnectorStatus(cp, i+1, s, "NoError")
	}
	r.pump()
}

func watts(w float64) *float64 { return &w }

// waitFor polls until cond holds; the calls under test run on goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func newTestLogger(sink *LogSink) *slog.Logger { return slog.New(sink.Handler()) }
