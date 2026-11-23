package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/wolffseb/cli-cpms/internal/core"
)

// golden compares a frame with testdata/<name>.golden. Run with -update to
// rewrite them, and read the diff: a golden is only as good as its review.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if updating() {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("frame differs from %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// assertFits fails if the frame is not exactly w x h: nothing wider than the
// terminal, nothing taller, so nothing can overlap after a resize.
func assertFits(t *testing.T, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != h {
		t.Errorf("frame has %d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got > w {
			t.Errorf("line %d is %d columns wide, terminal is %d: %q", i+1, got, w, l)
		}
	}
}

func TestInitialFrame(t *testing.T) {
	r := newRig(t)
	golden(t, "initial", r.view())
	assertFits(t, r.view(), 100, 30)
}

// two stations, one gone away: the shape of the spec's mock-up.
func (r *rig) twoStations() {
	r.onlineStation(cp1, "Alpitronic", "HYC300", "1.4.2", core.StatusAvailable, core.StatusAvailable)
	r.onlineStation(cp2, "Alpitronic", "HYC300", "1.4.2", core.StatusAvailable, core.StatusAvailable)
	r.clk.Advance(time.Minute)
	r.core.Disconnected(cp2, "heartbeat timeout")
	r.pump()
}

func TestAfterConnect(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	golden(t, "connected", r.view())
	assertFits(t, r.view(), 100, 30)
}

func TestReservationShownInDetail(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	reservation, err := r.core.BeginReservation(core.Reservation{
		ChargePointID: cp1, EVSEUID: evse11, IDTag: defaultTag, ExpiresAt: r.clk.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Still pending: the detail says so.
	r.pump()
	r.send(tickMsg{})
	if got := r.view(); !strings.Contains(got, "#1 04A1B2C3D4 until 09:46 (pending)") {
		t.Errorf("a pending reservation is not marked:\n%s", got)
	}
	r.core.ActivateReservation(reservation.ID)
	r.pump()
	r.send(tickMsg{})
	golden(t, "reservation", r.view())
}

func TestSessionShownInDetail(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.core.SetConnectorStatus(cp1, 2, core.StatusCharging, "NoError")
	r.core.RecordMeterValues(cp1, 2, "12", core.MeterReading{PowerW: watts(48200)})
	r.pump()
	golden(t, "session", r.view())

	// The log keeps events but not meter values or heartbeats.
	r.core.Heartbeat(cp1)
	r.pump()
	if got := r.view(); strings.Contains(got, "001 heartbeat") || strings.Contains(got, "meter values") {
		t.Errorf("heartbeats and meter values must stay out of the log:\n%s", got)
	}
}

func TestErrorFrame(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.ctl.err = errors.New("the station is not there")
	r.send(keyMsg("down"))
	r.send(keyMsg("down"))
	r.deliver(r.press("r"))
	golden(t, "error", r.view())
}

func TestInFlightFrame(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.ctl.gate = make(chan struct{})
	t.Cleanup(func() { close(r.ctl.gate) })
	r.send(keyMsg("down"))
	r.pressAsync("s")
	golden(t, "inflight", r.view())
}

func TestStopConfirmFrame(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.pump()
	r.send(keyMsg("down"))
	r.send(keyMsg("x"))
	golden(t, "stopconfirm", r.view())
}

func TestHelpFrame(t *testing.T) {
	r := newRig(t)
	r.send(keyMsg("?"))
	golden(t, "help", r.view())
	assertFits(t, r.view(), 100, 30)
}

func TestNarrowFrame(t *testing.T) {
	r := newRig(t)
	r.size(50, 30)
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.core.SetConnectorStatus(cp1, 2, core.StatusCharging, "NoError")
	r.core.RecordMeterValues(cp1, 2, "12", core.MeterReading{PowerW: watts(48200)})
	r.pump()
	golden(t, "narrow", r.view())
	assertFits(t, r.view(), 50, 30)
	if strings.Contains(r.view(), "kW") {
		t.Error("the kW figure is for wide terminals only")
	}
}

func TestResizeNeverOverlaps(t *testing.T) {
	r := newRig(t)
	r.twoStations()
	r.core.StartTransaction(cp1, core.TransactionStart{ID: "12", ConnectorID: 2, IDTag: defaultTag, At: r.clk.Now()})
	r.pump()
	assertFits(t, r.view(), 100, 30)

	for _, size := range [][2]int{{50, 30}, {59, 30}, {60, 30}, {100, 30}, {20, 10}, {80, 8}, {3, 2}, {120, 50}} {
		r.size(size[0], size[1])
		assertFits(t, r.view(), size[0], size[1])
		r.send(keyMsg("?"))
		assertFits(t, r.view(), size[0], size[1])
		r.send(keyMsg("?"))
	}

	r.size(100, 30)
	r.size(50, 30)
	golden(t, "resized", r.view())
}

func TestLongErrorStaysInItsTwoLines(t *testing.T) {
	r := newRig(t)
	r.ctl.err = errors.New(strings.Repeat("word ", 80))
	r.deliver(r.press("t"))
	assertFits(t, r.view(), 100, 30)
	lines := strings.Split(r.view(), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "↑↓ select") {
		t.Errorf("the hint bar is not the last line: %q", last)
	}
	if cut := lines[len(lines)-2]; !strings.HasSuffix(cut, "…") {
		t.Errorf("a message that does not fit should end in an ellipsis: %q", cut)
	}
}

func TestStatusWordsLikeTheCLI(t *testing.T) {
	r := newRig(t)
	r.onlineStation(cp1, "Alpitronic", "HYC300", "1.4.2", core.StatusAvailable)
	r.core.SetConnectorStatus(cp1, 2, core.StatusBlocked, "ConnectorLockFailure")
	r.pump()
	// In the row, not just in the log line the change also made.
	row := ""
	for _, l := range strings.Split(r.view(), "\n") {
		if strings.HasPrefix(l, "     ALP-HYC-001-2 ") {
			row = l
		}
	}
	if !strings.Contains(row, "  BLOCKED (ConnectorLockFailure)  ") {
		t.Errorf("the error code belongs after the status in the row; row = %q", row)
	}
}

func TestPushHeader(t *testing.T) {
	// ocpiHeader is covered in header_test.go; this only pins that the
	// header counts chargers.
	r := newRig(t)
	r.twoStations()
	first := strings.SplitN(r.view(), "\n", 2)[0]
	if !strings.HasPrefix(first, "cpms · 1/2 chargers online · OCPI not registered") {
		t.Errorf("header = %q", first)
	}
}

// The pane is a fixed tail: when there is more than fits, the newest lines
// stay and the oldest go.
func TestLogShowsTheNewestLines(t *testing.T) {
	r := newRig(t)
	r.onlineStation(cp1, "Alpitronic", "HYC300", "1.4.2", core.StatusAvailable, core.StatusAvailable)
	for i := 0; i < 40; i++ {
		r.core.SetConnectorStatus(cp1, 1, core.StatusCharging, "NoError")
		r.core.SetConnectorStatus(cp1, 1, core.StatusAvailable, "NoError")
	}
	r.core.Disconnected(cp1, "the last thing that happened")
	r.pump()

	lines := strings.Split(r.view(), "\n")
	newest := lines[len(lines)-1-messageLines-1]
	if !strings.HasSuffix(newest, "ALP-HYC-001-2 AVAILABLE→UNKNOWN") {
		t.Errorf("the line just above the message is not the newest event: %q", newest)
	}
	if strings.Contains(r.view(), "ALP-HYC-001 connected") {
		t.Errorf("the oldest line is still on screen:\n%s", r.view())
	}
	assertFits(t, r.view(), 100, 30)
}

// A station's own text can hold double-width runes. At a width where not even
// one fits after the indent, wrap used to take nothing off the word and spin,
// and with it Bubble Tea's event loop: keys and quit were never read again.
func TestViewTerminatesAtEverySizeWithWideRunes(t *testing.T) {
	for _, help := range []bool{false, true} {
		for w := 0; w <= 12; w++ {
			for h := 0; h <= 6; h++ {
				r := newRig(t, func(o *Options) { o.LogPath = "/充電器/cpms.log" })
				r.m.msg = message{msgErr, "✗ 充電器 充電器充電器 reported 充電器"}
				r.m.help = help
				r.size(w, h)

				done := make(chan string, 1)
				go func() { done <- r.view() }()
				select {
				case frame := <-done:
					if h == 0 {
						if frame != "" {
							t.Errorf("a zero-height screen drew %q", frame)
						}
						continue
					}
					assertFits(t, frame, w, h)
				case <-time.After(time.Second):
					t.Fatalf("View did not return at %dx%d (help %v)", w, h, help)
				}
			}
		}
	}
}

// The log is on local time, so a reservation's expiry is too: core's own line
// carries it in RFC 3339 UTC, an hour (or more) off the time before it.
func TestReservationEventShowsLocalTime(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	r := newRig(t, func(o *Options) { o.Location = cest })
	r.onlineStation(cp1, "Alpitronic", "HYC300", "1.4.2", core.StatusAvailable, core.StatusAvailable)
	reservation, err := r.core.BeginReservation(core.Reservation{
		ChargePointID: cp1, EVSEUID: evse11, IDTag: defaultTag, ExpiresAt: r.clk.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.core.ActivateReservation(reservation.ID)
	r.pump()
	// 09:30 UTC is 11:30 on the screen; the reservation ends at 11:45:00.
	if got := r.view(); !strings.Contains(got, "11:30:00 ALP-HYC-001-1 reserved for tag 04A1B2C3D4 until 11:45:00 (reservation 1)") {
		t.Errorf("the expiry is not in local time:\n%s", got)
	}
}

// The charger line gives a short reason; the transport's own words (a close
// code, "unexpected EOF") are in cpms.log, and at 50 columns they would be cut
// to nothing useful.
func TestOfflineReasonIsShort(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"websocket: close 1006 (abnormal closure): unexpected EOF", "(connection lost)"},
		{"read tcp 127.0.0.1:9000->127.0.0.1:5: i/o timeout", "(connection lost)"},
		{"heartbeat timeout", "(heartbeat timeout)"},
		{"closed by charge point", "(closed by charge point)"},
	} {
		r := newRig(t)
		r.onlineStation(cp1, "Alpitronic", "HYC300", "1.4.2", core.StatusAvailable)
		r.core.Disconnected(cp1, tc.reason)
		r.pump()
		r.send(tickMsg{})
		want := "ALP-HYC-001  offline  last seen 09:30:00 " + tc.want
		if got := r.view(); !strings.Contains(got, want) {
			t.Errorf("reason %q: want the charger line %q in\n%s", tc.reason, want, got)
		}
		if strings.Contains(strings.SplitN(r.view(), "\n", 5)[2], "EOF") {
			t.Errorf("reason %q leaks into the charger line", tc.reason)
		}
	}

	// Never connected: no reason at all, as before.
	r := newRig(t)
	if got := r.view(); !strings.Contains(got, "ALP-HYC-001  offline  never seen") {
		t.Errorf("never seen:\n%s", got)
	}
}
