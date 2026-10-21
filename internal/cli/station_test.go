package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

var update = flag.Bool("update", false, "rewrite golden files")

// shortDir returns a temporary directory with a short path. Unix socket paths
// are limited to about 104 bytes on macOS, and t.TempDir() there lives under
// a long /var/folders/... path plus the test's name.
func shortDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "cpms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// station is `cpms run` on a fixed port plus, optionally, the simulator
// dialled into it: the three-terminal setup in one test.
type station struct {
	t          *testing.T
	port       int
	configPath string
	socketPath string

	runCancel context.CancelFunc
	runDone   <-chan error
	runOut    *syncBuffer

	sim *simulator.Simulator
}

// newStation starts `cpms run`. Config replacements are applied as in
// runConfig.
func newStation(t *testing.T, port int, replacements ...string) *station {
	t.Helper()

	s := &station{
		t:          t,
		port:       port,
		configPath: runConfig(t, port, replacements...),
		socketPath: filepath.Join(shortDir(t), "cpms.sock"),
	}
	s.startRun()
	return s
}

func (s *station) startRun() {
	s.t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	out, done := startRun(s.t, ctx, "run", "-c", s.configPath, "--socket", s.socketPath)
	s.runCancel, s.runDone, s.runOut = cancel, done, out
	s.t.Cleanup(s.stopRun)
	waitForOutput(s.t, out, "control socket "+s.socketPath)
}

// stopRun stops `cpms run` and waits for it to return. It is safe to call
// more than once.
func (s *station) stopRun() {
	s.t.Helper()

	if s.runCancel == nil {
		return
	}
	s.runCancel()
	s.runCancel = nil
	select {
	case err := <-s.runDone:
		if err != nil {
			s.t.Errorf("cpms run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		s.t.Fatal("cpms run did not shut down")
	}
}

// connect dials the simulator in and waits until `cpms status` sees the
// station online with its EVSE available.
func (s *station) connect(mutate ...func(*simulator.Options)) {
	s.t.Helper()

	opts := simulator.Options{
		URL:        "ws://127.0.0.1:" + strconv.Itoa(s.port),
		ID:         "ALP-HYC-001",
		Connectors: 1,
		IDTag:      "04A1B2C3D4",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range mutate {
		m(&opts)
	}
	sim, err := simulator.New(opts)
	if err != nil {
		s.t.Fatalf("simulator.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.t.Cleanup(func() {
		cancel()
		sim.Close()
		sim.Wait()
	})
	if err := sim.Connect(ctx); err != nil {
		s.t.Fatalf("simulator connect: %v", err)
	}
	s.sim = sim
	s.waitStatus("the station to be online and AVAILABLE", func(st socket.Status) bool {
		return st.ChargePoint.Online && len(st.EVSEs) >= 1 && st.EVSEs[0].Status == "AVAILABLE"
	})
}

// cpms runs a one-shot command against this station and returns its output
// and exit code.
func (s *station) cpms(args ...string) (stdout, stderr string, code int) {
	s.t.Helper()

	args = append(args, "-c", s.configPath, "--socket", s.socketPath)
	stdout, stderr, err := run(s.t, args...)
	if err != nil {
		// What main would print.
		stderr += "cpms: " + err.Error() + "\n"
	}
	return stdout, stderr, cli.ExitCode(err)
}

// mustCPMS runs a command that has to succeed.
func (s *station) mustCPMS(args ...string) string {
	s.t.Helper()

	stdout, stderr, code := s.cpms(args...)
	if code != 0 {
		s.t.Fatalf("cpms %s exited %d: %s", strings.Join(args, " "), code, stderr)
	}
	return stdout
}

func (s *station) status() socket.Status {
	s.t.Helper()

	var st socket.Status
	if err := json.Unmarshal([]byte(s.mustCPMS("status", "--json")), &st); err != nil {
		s.t.Fatalf("decoding status --json: %v", err)
	}
	return st
}

func (s *station) waitStatus(what string, cond func(socket.Status) bool) socket.Status {
	s.t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := s.status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s; last status: %+v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func expectExit(t *testing.T, what string, code, want int, stderr string, mention ...string) {
	t.Helper()

	if code != want {
		t.Fatalf("%s exited %d, want %d; stderr: %s", what, code, want, stderr)
	}
	for _, m := range mention {
		if !strings.Contains(stderr, m) {
			t.Errorf("%s: message %q should mention %q", what, stderr, m)
		}
	}
}

// TestReserveStartStopThroughTheSocket is the first milestone: the terminal
// drives the station end to end.
func TestReserveStartStopThroughTheSocket(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19300)
	s.connect()

	out := s.mustCPMS("reserve")
	if !strings.Contains(out, "EVSE-1 reserved for 04A1B2C3D4") || !strings.Contains(out, "(reservation 1)") {
		t.Errorf("reserve printed %q, want the EVSE, tag and reservation id", out)
	}

	st := s.waitStatus("EVSE-1 to be RESERVED", func(st socket.Status) bool { return st.EVSEs[0].Status == "RESERVED" })
	r := st.EVSEs[0].Reservation
	if r == nil || r.ID != 1 || r.IDTag != "04A1B2C3D4" || r.State != "active" {
		t.Fatalf("status reservation = %+v, want #1 for 04A1B2C3D4", r)
	}
	if left := time.Until(r.ExpiresAt); left < 14*time.Minute || left > 16*time.Minute {
		t.Errorf("reservation expires in %s, want about 15m", left)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "RESERVED") || !strings.Contains(human, "#1 04A1B2C3D4 until") {
		t.Errorf("status shows:\n%s\nwant the reservation and its expiry", human)
	}

	// No --tag: the configured default tag redeems the reservation.
	s.mustCPMS("start")
	st = s.waitStatus("EVSE-1 to be CHARGING", func(st socket.Status) bool {
		return st.EVSEs[0].Status == "CHARGING" && st.EVSEs[0].Transaction != nil
	})
	if tx := st.EVSEs[0].Transaction; tx.IDTag != "04A1B2C3D4" {
		t.Errorf("transaction tag = %q, want the default tag", tx.IDTag)
	}
	if st.EVSEs[0].Reservation != nil {
		t.Errorf("reservation %+v survived the session it was redeemed by", st.EVSEs[0].Reservation)
	}

	s.mustCPMS("stop")
	s.waitStatus("the transaction to end", func(st socket.Status) bool { return st.EVSEs[0].Transaction == nil })
}

func TestUnlock(t *testing.T) {
	t.Parallel()

	t.Run("unlocked", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19301)
		s.connect()
		if out := s.mustCPMS("unlock"); !strings.Contains(out, "EVSE-1 unlocked") {
			t.Errorf("unlock printed %q", out)
		}
	})

	t.Run("unlock fails", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19302)
		s.connect(func(o *simulator.Options) { o.Scenario = simulator.ScenarioUnlockFails })
		_, stderr, code := s.cpms("unlock")
		expectExit(t, "unlock", code, cli.ExitUnlockFailed, stderr, "EVSE-1", "UnlockFailed")
	})
}

func TestStationRefusalsHaveDistinctExitCodes(t *testing.T) {
	t.Parallel()

	t.Run("rejected", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19303)
		s.connect(func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectReserve })
		_, stderr, code := s.cpms("reserve")
		expectExit(t, "reserve", code, cli.ExitRejected, stderr, "rejected", "Rejected", "cpms probe")
	})

	t.Run("occupied", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19304)
		s.connect(func(o *simulator.Options) { o.Scenario = simulator.ScenarioOccupied })
		_, stderr, code := s.cpms("reserve")
		expectExit(t, "reserve", code, cli.ExitOccupied, stderr, "Occupied")
	})

	t.Run("faulted", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19305)
		s.connect()
		if err := s.sim.SetConnectorStatus(context.Background(), 1, "Faulted"); err != nil {
			t.Fatal(err)
		}
		s.waitStatus("EVSE-1 to be OUTOFORDER", func(st socket.Status) bool { return st.EVSEs[0].Status == "OUTOFORDER" })
		_, stderr, code := s.cpms("reserve")
		expectExit(t, "reserve", code, cli.ExitFaulted, stderr, "Faulted")
	})
}

// TestTimeoutExitCode is not parallel: goleak compares against the goroutines
// alive when it starts, and a parallel sibling would add its own.
func TestTimeoutExitCode(t *testing.T) {
	// Cleanups run last-in first-out, so this runs after the station is gone.
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	s := newStation(t, 19306, "heartbeat_timeout: 90s", "heartbeat_timeout: 90s\n  call_timeout: 1s")
	s.connect(func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 3 * time.Second
	})

	start := time.Now()
	_, stderr, code := s.cpms("reserve")
	expectExit(t, "reserve", code, cli.ExitTimeout, stderr, "did not answer")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %s, want about the 1s call timeout", elapsed)
	}
	// The timed-out reservation must not linger.
	if r := s.status().EVSEs[0].Reservation; r != nil {
		t.Errorf("a timed-out reservation is held: %+v", r)
	}
}

func TestNoRunningCPMSExits2(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19399)
	sock := filepath.Join(shortDir(t), "cpms.sock")
	_, _, err := run(t, "status", "-c", path, "--socket", sock)
	if code := cli.ExitCode(err); code != cli.ExitNotRunning {
		t.Fatalf("exit %d, want %d (err %v)", code, cli.ExitNotRunning, err)
	}
	if !strings.Contains(err.Error(), "Is `cpms run` running") {
		t.Errorf("message %q should ask whether cpms run is running", err)
	}
}

func TestStationNotConnectedExits3(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19307)

	_, stderr, code := s.cpms("reserve")
	expectExit(t, "reserve", code, cli.ExitNotConnected, stderr, "not connected", "CSMS URL")

	// Status still works and says so.
	st := s.status()
	if st.ChargePoint.Online || st.EVSEs[0].Status != "UNKNOWN" {
		t.Errorf("status = %+v, want offline and UNKNOWN", st)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "offline") {
		t.Errorf("status shows:\n%s\nwant it to say offline", human)
	}
}

func TestLocalRefusalsExit9(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19308)
	s.connect()

	_, stderr, code := s.cpms("stop")
	expectExit(t, "stop with nothing running", code, cli.ExitRefused, stderr, "no active transaction")

	_, stderr, code = s.cpms("cancel")
	expectExit(t, "cancel with nothing reserved", code, cli.ExitRefused, stderr, "no active reservation")

	s.mustCPMS("reserve")
	_, stderr, code = s.cpms("reserve")
	expectExit(t, "second reserve", code, cli.ExitRefused, stderr, "already has a reservation")

	s.mustCPMS("cancel")
	s.waitStatus("the reservation to be gone", func(st socket.Status) bool { return st.EVSEs[0].Reservation == nil })
}

// An EVSE typo is caught from config alone: no running cpms is needed to be
// told, and nothing is sent.
func TestUnknownEVSEMakesNoConnection(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19398)
	sock := filepath.Join(shortDir(t), "cpms.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	for _, cmd := range []string{"reserve", "cancel", "start", "stop", "unlock"} {
		_, _, err := run(t, cmd, "--evse", "NOPE", "-c", path, "--socket", sock)
		if code := cli.ExitCode(err); code != cli.ExitError {
			t.Errorf("%s --evse NOPE exited %d, want %d", cmd, code, cli.ExitError)
			continue
		}
		if !strings.Contains(err.Error(), "NOPE") || !strings.Contains(err.Error(), "EVSE-1") {
			t.Errorf("%s: message %q should name the bad uid and list the configured ones", cmd, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Errorf("the socket saw %d connections, want none", n)
	}
}

func TestSecondRunIsRefused(t *testing.T) {
	t.Parallel()
	first := newStation(t, 19309)

	// Different ports, same socket: the socket is what makes it the same
	// instance.
	other := runConfig(t, 19310)
	_, _, err := run(t, "run", "-c", other, "--socket", first.socketPath)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second run returned %v, want an already-running error", err)
	}
	if cli.ExitCode(err) != cli.ExitError {
		t.Errorf("second run exit code %d, want 1", cli.ExitCode(err))
	}

	// And the first one is unaffected.
	if st := first.status(); st.ChargePoint.ID != "ALP-HYC-001" {
		t.Errorf("first instance status = %+v", st)
	}
}

// The guard is the state file, not the socket path: two runs with different
// --socket values would otherwise write the same file.
func TestSecondRunOnTheSameStateFileIsRefused(t *testing.T) {
	t.Parallel()
	first := newStation(t, 19315)
	state := filepath.Join(filepath.Dir(first.configPath), cli.DefaultStateFile)

	other := runConfig(t, 19316)
	sock := filepath.Join(shortDir(t), "other.sock")
	_, _, err := run(t, "run", "-c", other, "--state", state, "--socket", sock)
	if err == nil || !strings.Contains(err.Error(), "in use by another cpms") {
		t.Fatalf("second run returned %v, want an in-use error", err)
	}
	if cli.ExitCode(err) != cli.ExitError {
		t.Errorf("second run exit code %d, want 1", cli.ExitCode(err))
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	t.Parallel()

	sock := filepath.Join(shortDir(t), "cpms.sock")
	// A socket file nobody listens on, as left by a cpms that was killed.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket file not there: %v", err)
	}

	s := &station{t: t, port: 19311, configPath: runConfig(t, 19311), socketPath: sock}
	s.startRun()
	if st := s.status(); st.ChargePoint.ID != "ALP-HYC-001" {
		t.Errorf("status = %+v", st)
	}
}

// TestReservationSurvivesRestart is the roadmap criterion: reserve, restart
// cpms, and the reservation is still there.
func TestReservationSurvivesRestart(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19312)
	s.connect()

	// Non-default tag and length, so the restart has to keep what was asked
	// for, not just fall back to defaults.
	s.mustCPMS("reserve", "--tag", "CARD-9", "--expires", "30m")
	before := s.waitStatus("the reservation", func(st socket.Status) bool { return st.EVSEs[0].Reservation != nil }).EVSEs[0].Reservation
	if before.IDTag != "CARD-9" {
		t.Errorf("reservation tag = %q, want CARD-9", before.IDTag)
	}
	if left := time.Until(before.ExpiresAt); left < 29*time.Minute || left > 31*time.Minute {
		t.Errorf("reservation expires in %s, want about 30m", left)
	}

	s.stopRun()
	s.startRun()

	after := s.status().EVSEs[0].Reservation
	if after == nil || after.ID != before.ID || after.IDTag != before.IDTag || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("after restart: reservation %+v, want %+v", after, before)
	}
}

var timestamp = regexp.MustCompile(`"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z"`)

// The JSON is an interface other tools parse, so its shape is pinned.
func TestStatusJSONGolden(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19313)
	s.connect()
	s.mustCPMS("reserve")
	s.waitStatus("the reservation", func(st socket.Status) bool { return st.EVSEs[0].Status == "RESERVED" })

	got := timestamp.ReplaceAllString(s.mustCPMS("status", "--json"), `"<timestamp>"`)

	golden := filepath.Join("testdata", "status.golden.json")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("status --json changed.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestExitCodeOfPlainErrors(t *testing.T) {
	t.Parallel()

	if cli.ExitCode(nil) != 0 {
		t.Error("nil should exit 0")
	}
	if cli.ExitCode(errors.New("boom")) != 1 {
		t.Error("an unclassified error should exit 1")
	}
}

// A config a few directories deep is enough to push cpms.sock past the
// 103-byte Unix socket limit, which is how macOS CI first broke. The default
// then falls back to a short path that `run` and the commands both derive
// from the state file, so neither needs --socket.
func TestDefaultSocketFallsBackWhenThePathIsTooLong(t *testing.T) {
	t.Parallel()

	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(runConfig(t, 19314))
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(deep, "config.yaml")
	if err := os.WriteFile(cfg, src, 0o600); err != nil {
		t.Fatal(err)
	}
	if next := filepath.Join(deep, "cpms.sock"); len(next) <= socket.MaxPathLen {
		t.Fatalf("test setup: %s is only %d bytes, not too long", next, len(next))
	}

	ctx, cancel := context.WithCancel(context.Background())
	out, done := startRun(t, ctx, "run", "-c", cfg)
	t.Cleanup(func() { cancel(); <-done })
	waitForOutput(t, out, "control socket ")

	stdout, _, err := run(t, "status", "--json", "-c", cfg)
	if err != nil {
		t.Fatalf("status without --socket: %v\nrun output:\n%s", err, out.String())
	}
	if !strings.Contains(stdout, `"id": "ALP-HYC-001"`) {
		t.Errorf("status printed %s", stdout)
	}

	// An explicit path is not second-guessed: too long is an error that says
	// how to fix it.
	long := filepath.Join(deep, "cpms.sock")
	_, _, err = run(t, "status", "-c", cfg, "--socket", long)
	if err == nil || !strings.Contains(err.Error(), "--socket") {
		t.Errorf("explicit long --socket: err = %v, want one pointing at --socket", err)
	}
}
