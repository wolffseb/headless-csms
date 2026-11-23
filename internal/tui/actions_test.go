package tui

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi/push"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/state"
)

func TestReserveUsesTheDefaultTagAndFifteenMinutes(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.ctl.gate = make(chan struct{})

	done := r.pressAsync("r")
	waitFor(t, "the call", func() bool { return len(r.ctl.Calls()) == 1 })

	r.ctl.mu.Lock()
	p := r.ctl.reserveParams
	r.ctl.mu.Unlock()
	if p.EVSEUID != "ALP-HYC-001-1" || p.IDTag != "04A1B2C3D4" || p.ExpiresIn != 15*time.Minute || p.Source != "tui" {
		t.Errorf("Reserve got %+v, want ALP-HYC-001-1 for the default tag, 15m, source tui", p)
	}

	// The spinner sits on the row while the station is asked, and the
	// message line says what is going on.
	frame := r.view()
	if !strings.Contains(frame, "▸ ⠋ ALP-HYC-001-1") {
		t.Errorf("no spinner on the row in flight:\n%s", frame)
	}
	if !strings.Contains(frame, "reserve ALP-HYC-001-1: waiting for the station…") {
		t.Errorf("the message line does not say we are waiting:\n%s", frame)
	}

	close(r.ctl.gate)
	r.deliver(<-done)

	frame = r.view()
	if strings.Contains(frame, "⠋") {
		t.Errorf("the spinner stays after the answer:\n%s", frame)
	}
	const want = "✓ ALP-HYC-001-1 reserved for 04A1B2C3D4 until 09:45:00 (reservation 7)"
	if !strings.Contains(frame, want) {
		t.Errorf("no %q in\n%s", want, frame)
	}
	// Only in the message line: the log has the event that says the same, and
	// a failure, which the message line would lose, is the only result it keeps.
	if n := strings.Count(frame, want); n != 1 {
		t.Errorf("the result should be in the message line only, found %d times", n)
	}
}

func TestSecondKeyOnTheSameEVSEWhileInFlightSendsNothing(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.ctl.gate = make(chan struct{})
	first := r.pressAsync("r")
	waitFor(t, "the call", func() bool { return len(r.ctl.Calls()) == 1 })

	for _, key := range []string{"r", "s", "u", "c", "t"} {
		if a := r.press(key); a != nil {
			t.Errorf("%q started a second command on a busy EVSE", key)
		}
	}
	if got := r.ctl.Calls(); len(got) != 1 {
		t.Errorf("calls = %v, want only the first reserve", got)
	}
	if !strings.Contains(r.view(), "ALP-HYC-001-1 is still waiting for reserve; try again when it answers") {
		t.Errorf("no hint:\n%s", r.view())
	}

	// Another EVSE is not held up by it.
	r.send(keyMsg("down"))
	second := r.pressAsync("u")
	waitFor(t, "the second EVSE's call", func() bool { return len(r.ctl.Calls()) == 2 })
	if got := r.ctl.Calls()[1]; got != "unlock ALP-HYC-001-2" {
		t.Errorf("second call = %q", got)
	}
	close(r.ctl.gate)
	r.deliver(<-first)
	r.deliver(<-second)

	// After the answer the EVSE takes keys again.
	r.ctl.gate = nil
	r.send(keyMsg("up"))
	r.deliver(r.press("c"))
	if got := r.ctl.Calls(); got[len(got)-1] != "cancel ALP-HYC-001-1" {
		t.Errorf("calls = %v", got)
	}
}

// Describe gets the op, the EVSE and the error control returned, and what it
// says is shown as is: the CLI's wording is the CLI's to keep.
func TestErrorsGoThroughDescribe(t *testing.T) {
	var gotOp socket.Op
	var gotSubject string
	var gotErr error
	r := newRig(t, func(o *Options) {
		o.Describe = func(op socket.Op, subject string, err error) string {
			gotOp, gotSubject, gotErr = op, subject, err
			return "DESCRIBED"
		}
	})
	r.twoStations()
	refused := &control.RefusedError{Op: control.OpReserve, EVSEUID: "ALP-HYC-001-1", Status: ocpp.CommandRejected, Raw: "Rejected"}
	r.ctl.err = refused
	r.deliver(r.press("r"))
	if gotOp != socket.OpReserve || gotSubject != "ALP-HYC-001-1" || gotErr != error(refused) {
		t.Errorf("Describe(%q, %q, %v)", gotOp, gotSubject, gotErr)
	}
	if n := strings.Count(r.view(), "✗ DESCRIBED"); n != 2 {
		t.Errorf("the error belongs in the log and the message line, found %d times:\n%s", n, r.view())
	}
}

func TestEachKeyCallsItsControlMethod(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.send(keyMsg("down"))
	for key, want := range map[string]string{
		"c": "cancel ALP-HYC-001-2",
		"s": "start ALP-HYC-001-2 04A1B2C3D4",
		"u": "unlock ALP-HYC-001-2",
		"t": "refresh ALP-HYC-001-2",
	} {
		before := len(r.ctl.Calls())
		r.deliver(r.press(key))
		got := r.ctl.Calls()
		if len(got) != before+1 || got[before] != want {
			t.Errorf("key %q made calls %v, want ...%q", key, got[before:], want)
		}
	}
	for key, want := range map[string]string{
		"c": "✓ ALP-HYC-001-2 reservation cancelled",
		"s": "✓ ALP-HYC-001-2 remote start accepted",
		"u": "✓ ALP-HYC-001-2 unlocked",
		"t": "✓ ALP-HYC-001-2 status requested",
	} {
		r.deliver(r.press(key))
		if !strings.Contains(r.view(), want) {
			t.Errorf("key %q: no %q in\n%s", key, want, r.view())
		}
	}
}

func TestSelectionClampsAtTheEnds(t *testing.T) {
	r := newRig(t)
	cursorOn := func() string {
		for _, l := range strings.Split(r.view(), "\n") {
			if strings.Contains(l, "▸") {
				return strings.Fields(l)[1]
			}
		}
		return ""
	}
	if got := cursorOn(); got != "ALP-HYC-001-1" {
		t.Fatalf("starts on %q", got)
	}
	r.send(keyMsg("up"))
	r.send(keyMsg("k"))
	if got := cursorOn(); got != "ALP-HYC-001-1" {
		t.Errorf("up at the top moved to %q", got)
	}
	var seen []string
	for range 6 {
		r.send(keyMsg("down"))
		seen = append(seen, cursorOn())
	}
	want := []string{"ALP-HYC-001-2", "ALP-HYC-002-1", "ALP-HYC-002-2", "ALP-HYC-002-2", "ALP-HYC-002-2", "ALP-HYC-002-2"}
	if !slices.Equal(seen, want) {
		t.Errorf("down x6 visited %v, want %v (it crosses chargers and stops at the end)", seen, want)
	}
	r.send(keyMsg("k"))
	r.send(keyMsg("j"))
	r.send(keyMsg("j"))
	if got := cursorOn(); got != "ALP-HYC-002-2" {
		t.Errorf("k j j ended on %q", got)
	}
}

// The selection is an EVSE, not a row: events that change the rows leave it
// where it is.
func TestSelectionFollowsTheEVSE(t *testing.T) {
	r := newRig(t)
	r.send(keyMsg("down"))
	r.send(keyMsg("down"))
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.pump()
	if !strings.Contains(r.view(), "▸   ALP-HYC-002-1") {
		t.Errorf("selection moved:\n%s", r.view())
	}
}

func TestStopAsksFirst(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.pump()
	r.send(keyMsg("down"))

	for _, other := range []string{"n", "x", "esc", "down", "r"} {
		if a := r.press("x"); a != nil {
			t.Fatal("x started the command before the confirmation")
		}
		if !strings.Contains(r.view(), "Stop session 12 on ALP-HYC-001-2? y to confirm, any other key cancels") {
			t.Fatalf("no question:\n%s", r.view())
		}
		if a := r.press(other); a != nil {
			t.Errorf("%q after x started a command", other)
		}
		if strings.Contains(r.view(), "Stop session") {
			t.Errorf("%q did not cancel the question", other)
		}
	}
	if got := r.ctl.Calls(); len(got) != 0 {
		t.Fatalf("calls after declining = %v, want none", got)
	}

	r.press("x")
	r.deliver(r.press("y"))
	if got := r.ctl.Calls(); !slices.Equal(got, []string{"stop ALP-HYC-001-2 tx=12"}) {
		t.Errorf("calls = %v", got)
	}
	if !strings.Contains(r.view(), "✓ ALP-HYC-001-2 remote stop accepted") {
		t.Errorf("no result:\n%s", r.view())
	}
}

// With no session there is nothing to confirm: control answers, in words.
func TestStopWithoutASessionAsksControl(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.ctl.err = control.ErrNoActiveTransaction
	r.deliver(r.press("x"))
	if got := r.ctl.Calls(); !slices.Equal(got, []string{"stop ALP-HYC-001-1 tx="}) {
		t.Errorf("calls = %v", got)
	}
	if !strings.Contains(r.view(), "no active transaction") {
		t.Errorf("control's answer is not shown:\n%s", r.view())
	}
}

func TestHelpTakesKeysAndGivesThemBack(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.send(keyMsg("?"))
	if !strings.Contains(r.view(), "? or esc closes") || strings.Contains(r.view(), "── log") {
		t.Fatalf("help does not replace the table and log:\n%s", r.view())
	}
	if a := r.press("r"); a != nil || len(r.ctl.Calls()) != 0 {
		t.Error("a key acted behind the help")
	}
	r.send(keyMsg("esc"))
	if strings.Contains(r.view(), "? or esc closes") {
		t.Error("esc did not close the help")
	}
	r.send(keyMsg("?"))
	r.send(keyMsg("?"))
	if strings.Contains(r.view(), "? or esc closes") {
		t.Error("? did not close the help")
	}
}

func TestMessageStaysUntilEscOrTheNextOne(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.deliver(r.press("u"))
	r.clk.Advance(time.Hour)
	r.send(tickMsg{})
	lines := strings.Split(r.view(), "\n")
	if got := lines[len(lines)-3]; got != "✓ ALP-HYC-001-1 unlocked" {
		t.Errorf("the message line = %q, want the result to stay", got)
	}
	r.send(keyMsg("esc"))
	lines = strings.Split(r.view(), "\n")
	if strings.TrimSpace(lines[len(lines)-3]) != "" || strings.TrimSpace(lines[len(lines)-2]) != "" {
		t.Errorf("esc did not clear the message:\n%s", r.view())
	}
}

func TestQuitCancelsTheSharedContext(t *testing.T) {
	for _, key := range []string{"q", "ctrl+c"} {
		stopped := 0
		r := newRig(t, func(o *Options) { o.Stop = func() { stopped++ } })
		cmd := r.send(keyMsg(key))
		if stopped != 1 {
			t.Errorf("%q: Stop called %d times, want 1", key, stopped)
		}
		if cmd == nil {
			t.Fatalf("%q: no command", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%q does not quit the program", key)
		}
	}
}

func TestQuitStaysAvailableWhileAskingToStop(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.pump()
	r.send(keyMsg("down"))
	r.send(keyMsg("x"))
	if cmd := r.send(keyMsg("q")); cmd == nil {
		t.Error("q did nothing during the stop question")
	}
}

type fakeReg struct{ reg *state.OCPIRegistration }

func (f fakeReg) Registration() *state.OCPIRegistration { return f.reg }

type fakePush struct{ st push.Status }

func (f fakePush) Status() push.Status { return f.st }

func TestHeaderOCPIWording(t *testing.T) {
	at := time.Date(2026, 11, 23, 10, 14, 55, 0, time.UTC)
	registered := &state.OCPIRegistration{
		Roles:     []state.PartyRole{{CountryCode: "DE", PartyID: "ABC", Role: "EMSP"}},
		Endpoints: []state.Endpoint{{Identifier: "locations", Role: "RECEIVER", URL: "https://emsp.example/locations"}},
	}
	noReceiver := &state.OCPIRegistration{Roles: registered.Roles}

	for _, tc := range []struct {
		name string
		reg  *state.OCPIRegistration
		push push.Status
		want string
		red  bool
	}{
		{"not registered", nil, push.Status{}, "OCPI not registered", false},
		{"no receiver", noReceiver, push.Status{}, "OCPI registered with DE*ABC · push: no locations receiver", false},
		{"nothing yet", registered, push.Status{}, "OCPI registered with DE*ABC · nothing pushed yet", false},
		{"pushed", registered, push.Status{LastSuccess: at}, "OCPI registered with DE*ABC · last push 10:14:55", false},
		{"failed", registered, push.Status{LastSuccess: at.Add(-time.Hour), LastErrorAt: at, LastError: "503 Service Unavailable"},
			"OCPI registered with DE*ABC · last push failed 10:14:55: 503 Service Unavailable", true},
		{"recovered", registered, push.Status{LastErrorAt: at.Add(-time.Hour), LastError: "boom", LastSuccess: at},
			"OCPI registered with DE*ABC · last push 10:14:55", false},
		{"long reason is cut", registered, push.Status{LastErrorAt: at, LastError: strings.Repeat("x", 60)},
			"OCPI registered with DE*ABC · last push failed 10:14:55: " + strings.Repeat("x", 39) + "…", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(o *Options) {
				o.Registration = fakeReg{tc.reg}
				o.Push = fakePush{tc.push}
			})
			r.size(200, 30) // wide enough that nothing is cut
			first := strings.SplitN(r.view(), "\n", 2)[0]
			if want := "cpms · 0/2 chargers online · " + tc.want; first != want {
				t.Errorf("header = %q, want %q", first, want)
			}
			if tc.red != r.m.ocpiFailed {
				t.Errorf("failed = %v, want %v", r.m.ocpiFailed, tc.red)
			}
		})
	}
}

// The push status is read on the tick, not once.
func TestHeaderFollowsThePushStatusOnTheTick(t *testing.T) {
	reg := &state.OCPIRegistration{
		Roles:     []state.PartyRole{{CountryCode: "DE", PartyID: "ABC", Role: "EMSP"}},
		Endpoints: []state.Endpoint{{Identifier: "locations", Role: "RECEIVER", URL: "https://emsp.example/locations"}},
	}
	p := &livePush{}
	r := newRig(t, func(o *Options) { o.Registration = fakeReg{reg}; o.Push = p })
	if !strings.Contains(r.view(), "nothing pushed yet") {
		t.Fatalf("header: %q", strings.SplitN(r.view(), "\n", 2)[0])
	}
	p.st.LastSuccess = time.Date(2026, 11, 23, 11, 0, 1, 0, time.UTC)
	r.send(tickMsg{})
	if !strings.Contains(r.view(), "last push 11:00:01") {
		t.Errorf("header: %q", strings.SplitN(r.view(), "\n", 2)[0])
	}
}

type livePush struct{ st push.Status }

func (l *livePush) Status() push.Status { return l.st }

func TestNarrowHeaderSplitsInTwo(t *testing.T) {
	r := newRig(t)
	r.size(50, 30)
	lines := strings.Split(r.view(), "\n")
	if lines[0] != "cpms · 0/2 chargers online" || lines[1] != "OCPI not registered" {
		t.Errorf("header = %q / %q", lines[0], lines[1])
	}
}

func TestStartupLinesAndWarningsReachTheLog(t *testing.T) {
	sink := NewLogSink()
	r := newRig(t, func(o *Options) {
		o.Logs = sink
		o.Startup = []string{"cpms listening for ALP-HYC-001 on ws://127.0.0.1:9000/ocpp/ALP-HYC-001"}
	})
	if !strings.Contains(r.view(), "cpms listening for ALP-HYC-001 on ws://") {
		t.Errorf("startup line missing:\n%s", r.view())
	}
	logger := newTestLogger(sink)
	logger.Info("quiet")
	logger.Warn("OCPI push retrying", "evse", "ALP-HYC-002-1", "error", "503 Service Unavailable")
	l := <-sink.ch
	r.send(logMsg(l))
	// The subject first and no key=value tail: the pane is narrow, and the
	// tail is what got cut off. The file has the attributes.
	if !strings.Contains(r.view(), "WARN ALP-HYC-002-1: OCPI push retrying\n") {
		t.Errorf("warning missing, or not subject first:\n%s", r.view())
	}
	if strings.Contains(r.view(), "error=") || strings.Contains(r.view(), "503") {
		t.Errorf("the attribute tail is in the pane:\n%s", r.view())
	}
	select {
	case l := <-sink.ch:
		t.Errorf("an INFO record reached the pane: %q", l.text)
	default:
	}
}

// The subject is the evse attribute if there is one, else the charge point's;
// a record about neither has no prefix.
func TestWarningSubject(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  func(*slog.Logger)
		want string
	}{
		{"evse", func(l *slog.Logger) { l.Warn("could not cancel", "reservation_id", 2, "evse", "E-1", "error", "x y") }, "WARN E-1: could not cancel"},
		{"evse wins over the charge point", func(l *slog.Logger) { l.Warn("m", "charge_point", "CP", "evse", "E-1") }, "WARN E-1: m"},
		{"charge point", func(l *slog.Logger) { l.Warn("m", "error", "boom", "charge_point", "CP-1") }, "WARN CP-1: m"},
		{"from With", func(l *slog.Logger) { l.With("evse", "E-2").Warn("m", "k", "v") }, "WARN E-2: m"},
		{"neither", func(l *slog.Logger) { l.Error("shutdown did not complete cleanly", "error", "x") }, "ERROR shutdown did not complete cleanly"},
	} {
		sink := NewLogSink()
		tc.log(newTestLogger(sink))
		if got := (<-sink.ch).text; got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// "Check the station's CSMS URL" is for a station that never reached cpms. On
// screen, a charger that dropped has a last-seen time and is offline: say that.
func TestNotConnectedErrorNamesTheOfflineCharger(t *testing.T) {
	r := newRig(t)
	r.twoStations() // ALP-HYC-002 has gone away at 09:31:00
	r.ctl.err = fmt.Errorf("reserve: %w", ocpp.ErrNotConnected)
	r.send(keyMsg("down"))
	r.send(keyMsg("down"))
	r.deliver(r.press("r"))

	want := "✗ ALP-HYC-002 is offline (last seen 09:30:00)"
	if got := r.view(); !strings.Contains(got, want) {
		t.Errorf("want %q in\n%s", want, got)
	}
	if strings.Contains(r.view(), "CSMS URL") {
		t.Errorf("the URL hint is for a charger that was never seen:\n%s", r.view())
	}
}

func TestNotConnectedErrorForAChargerNeverSeenKeepsTheCLIWording(t *testing.T) {
	r := newRig(t)
	r.ctl.err = fmt.Errorf("reserve: %w", ocpp.ErrNotConnected)
	r.deliver(r.press("r"))
	want := "✗ ALP-HYC-001-1 reserve failed: reserve: " + ocpp.ErrNotConnected.Error()
	if got := r.view(); !strings.Contains(got, want) {
		t.Errorf("want Describe's wording %q in\n%s", want, got)
	}
}
