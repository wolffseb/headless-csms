package cli_test

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// The TUI tests run the real binary: only it has a terminal to draw on, and
// only a process can show that its ports and socket are released. Ports
// 19650-19659 and 19690-19691 are this file's (each test takes a pair: the
// OCPP port and the next one). Its neighbours use 19660-19669 (limit_test.go)
// and 19670-19679 (station_meter_test.go); keep clear of them.

// ptyRun is `cpms run` on a pseudo-terminal.
type ptyRun struct {
	t      *testing.T
	cmd    *exec.Cmd
	ptmx   *os.File
	out    *syncBuffer      // what it drew, escape sequences and all
	emu    *vt.SafeEmulator // the screen those sequences make
	stderr *syncBuffer
	exited chan error
}

func startPTY(t *testing.T, bin string, args ...string) *ptyRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a pseudo-terminal")
	}

	r := &ptyRun{t: t, out: &syncBuffer{}, emu: vt.NewSafeEmulator(100, 30), stderr: &syncBuffer{}, exited: make(chan error, 1)}
	r.cmd = exec.Command(bin, args...)
	r.cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	// pty.Start would give stderr the terminal too; the point is to see that
	// nothing is written there.
	r.cmd.Stderr = r.stderr
	ptmx, err := pty.StartWithSize(r.cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatalf("starting on a pty: %v", err)
	}
	r.ptmx = ptmx
	go r.copyOut()
	// The emulator answers the terminal queries Bubble Tea sends, as a real
	// terminal does, and what it writes goes back to the process. It is never
	// closed: Close races with Read in the emulator. The goroutine ends with the
	// test binary.
	go func() { _, _ = io.Copy(ptmx, r.emu) }()
	go func() { r.exited <- r.cmd.Wait() }()
	t.Cleanup(func() {
		_ = ptmx.Close()
		select {
		case <-r.exited:
		default:
			_ = r.cmd.Process.Kill()
			<-r.exited
		}
	})
	return r
}

// copyOut records what the process draws, raw and on the emulated screen.
// Bubble Tea redraws only the cells that changed, so the text of a frame is
// never contiguous in the raw bytes: tests read the screen.
func (r *ptyRun) copyOut() {
	buf := make([]byte, 4096)
	for {
		n, err := r.ptmx.Read(buf)
		if n > 0 {
			_, _ = r.out.Write(buf[:n])
			_, _ = r.emu.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// screen is the text on the terminal now.
func (r *ptyRun) screen() string { return ansi.Strip(r.emu.Render()) }

// waitForScreen blocks until the text is on the screen.
func (r *ptyRun) waitForScreen(want string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.screen(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %q on the screen:\n%s", want, r.screen())
}

func (r *ptyRun) press(keys string) {
	r.t.Helper()
	if _, err := r.ptmx.WriteString(keys); err != nil {
		r.t.Fatalf("typing %q: %v", keys, err)
	}
}

// waitExit waits for the process to end and returns its exit code.
func (r *ptyRun) waitExit() int {
	r.t.Helper()
	select {
	case err := <-r.exited:
		r.exited <- err // for the cleanup
		if err == nil {
			return 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		r.t.Fatalf("waiting: %v", err)
	case <-time.After(15 * time.Second):
		// SIGQUIT makes the Go runtime dump every goroutine to stderr, which
		// says where it is stuck.
		_ = r.cmd.Process.Signal(syscall.SIGQUIT)
		time.Sleep(time.Second)
		r.t.Fatalf("cpms run did not exit; stderr:\n%s\nit drew:\n%s", r.stderr.String(), r.out.String())
	}
	return -1
}

func tuiStation(t *testing.T, ocppPort int) *station {
	t.Helper()
	return &station{
		t:          t,
		port:       ocppPort,
		configPath: runConfigPorts(t, ocppPort, ocppPort+1),
		socketPath: filepath.Join(shortDir(t), "cpms.sock"),
	}
}

// requireReleased checks what a clean shutdown leaves behind: both listeners
// free to bind again, and no control socket file.
func requireReleased(t *testing.T, s *station, ocppPort int) {
	t.Helper()
	for _, port := range []int{ocppPort, ocppPort + 1} {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Errorf("port %d is still held after shutdown: %v", port, err)
			continue
		}
		_ = l.Close()
	}
	if _, err := os.Stat(s.socketPath); !os.IsNotExist(err) {
		t.Errorf("the control socket file is still there (stat: %v)", err)
	}
}

func TestTUIQuitShutsDownLikeSIGINT(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	const port = 19650
	s := tuiStation(t, port)
	r := startPTY(t, buildCPMS(t), "run", "-c", s.configPath, "--socket", s.socketPath)

	r.waitForScreen("cpms · 0/1 chargers online")
	// The UI says how to leave it, not how headless is stopped.
	r.waitForScreen("waiting for the charge point to connect; press q to quit")

	// A key reaches control in-process: with no station connected, the error
	// is the one-shot CLI's own sentence.
	r.press("r")
	r.waitForScreen("EVSE-1: the charge point is not connected to cpms. Check that the station's CSMS URL points")

	// And the screen follows the station.
	s.connect()
	r.waitForScreen("cpms · 1/1 chargers online")

	r.press("q")
	if code := r.waitExit(); code != 0 {
		t.Fatalf("exit code %d, want 0; it drew:\n%s", code, r.out.String())
	}
	if !strings.Contains(r.out.String(), "shutting down") {
		t.Errorf("no \"shutting down\" after the screen was released:\n%s", r.out.String())
	}
	requireReleased(t, s, port)

	// Nothing reaches stderr while the TUI owns the screen; the log is in a
	// file next to the state file, from the startup lines on.
	if got := r.stderr.String(); got != "" {
		t.Errorf("stderr is not empty:\n%s", got)
	}
	logged, err := os.ReadFile(filepath.Join(filepath.Dir(s.configPath), "cpms.log"))
	if err != nil {
		t.Fatalf("reading cpms.log: %v", err)
	}
	for _, want := range []string{"cpms listening for ALP-HYC-001 on ws://127.0.0.1:19650/ocpp/ALP-HYC-001", "ALP-HYC-001 connected"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("cpms.log lacks %q:\n%s", want, logged)
		}
	}
}

func TestTUIShutsDownOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	const port = 19652
	s := tuiStation(t, port)
	r := startPTY(t, buildCPMS(t), "run", "-c", s.configPath, "--socket", s.socketPath)
	r.waitForScreen("cpms · 0/1 chargers online")

	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := r.waitExit(); code != 0 {
		t.Fatalf("exit code %d, want 0; it drew:\n%s", code, r.out.String())
	}
	if !strings.Contains(r.out.String(), "shutting down") {
		t.Errorf("no \"shutting down\":\n%s", r.out.String())
	}
	requireReleased(t, s, port)
	if got := r.stderr.String(); got != "" {
		t.Errorf("stderr is not empty:\n%s", got)
	}
}

// Without a terminal, or with --headless, `cpms run` is what it was before
// the TUI: log lines on stderr. It serves, and SIGINT stops it cleanly.
func TestHeadlessServesAndStopsOnSIGINT(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	const notice = "stdout is not a terminal; running headless"
	for _, tc := range []struct {
		name       string
		port       int
		args       []string
		wantNotice bool
	}{
		{"flag", 19654, []string{"--headless"}, false},
		{"no terminal", 19656, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tuiStation(t, tc.port)
			bin := buildCPMS(t)

			out, errOut := &syncBuffer{}, &syncBuffer{}
			c := exec.Command(bin, append([]string{"run", "-c", s.configPath, "--socket", s.socketPath}, tc.args...)...)
			c.Stdout, c.Stderr = out, errOut
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- c.Wait() }()
			t.Cleanup(func() { _ = c.Process.Kill() })
			waitForOutput(t, out, "control socket "+s.socketPath)

			s.connect()
			stdout, _ := execCPMS(t, bin, "status", "-c", s.configPath, "--socket", s.socketPath)
			if !strings.HasPrefix(stdout, "ALP-HYC-001  online") {
				t.Errorf("status = %q", stdout)
			}

			if err := c.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-exited:
				if err != nil {
					t.Fatalf("cpms run: %v\nstderr:\n%s", err, errOut.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("cpms run did not stop on SIGINT")
			}

			if !strings.Contains(out.String(), "shutting down") {
				t.Errorf("stdout lacks \"shutting down\":\n%s", out.String())
			}
			if !strings.Contains(out.String(), "waiting for the charge point to connect; press Ctrl-C to stop") {
				t.Errorf("headless keeps the Ctrl-C wording:\n%s", out.String())
			}
			if !strings.Contains(errOut.String(), "ALP-HYC-001 connected") {
				t.Errorf("the event log is not on stderr:\n%s", errOut.String())
			}
			if n := strings.Count(errOut.String(), notice); (n == 1) != tc.wantNotice || n > 1 {
				t.Errorf("the headless notice was printed %d times, want %v:\n%s", n, tc.wantNotice, errOut.String())
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(s.configPath), "cpms.log")); !os.IsNotExist(err) {
				t.Errorf("headless wrote cpms.log (stat: %v)", err)
			}
			requireReleased(t, s, tc.port)
		})
	}
}

// A first run on a new machine: the state directory is not there yet, and the
// TUI must create it, as headless always has.
func TestTUIRunsWithAStateDirectoryThatDoesNotExist(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	const port = 19658
	s := tuiStation(t, port)
	state := filepath.Join(shortDir(t), "new", "state.json")
	r := startPTY(t, buildCPMS(t), "run", "-c", s.configPath, "--state", state, "--socket", s.socketPath)

	r.waitForScreen("cpms · 0/1 chargers online")
	r.press("q")
	if code := r.waitExit(); code != 0 {
		t.Fatalf("exit code %d, want 0; it drew:\n%s", code, r.out.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state), "cpms.log")); err != nil {
		t.Errorf("cpms.log: %v", err)
	}
	requireReleased(t, s, port)
}

// Quitting the TUI must leave signal handling as headless does until the
// shutdown is over: a second Ctrl-C while cpms waits for a Reserve and its
// cancel is swallowed, not the default action that kills the process and
// leaves the station holding a reservation cpms has forgotten.
func TestTUISecondSIGINTDuringShutdownIsSwallowed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and waits out a call timeout")
	}
	t.Parallel()

	const port = 19690
	s := &station{
		t:    t,
		port: port,
		configPath: runConfigPorts(t, port, port+1,
			"heartbeat_timeout: 90s", "heartbeat_timeout: 90s\n  call_timeout: 2s"),
		socketPath: filepath.Join(shortDir(t), "cpms.sock"),
	}
	r := startPTY(t, buildCPMS(t), "run", "-c", s.configPath, "--socket", s.socketPath)
	r.waitForScreen("cpms · 0/1 chargers online")

	// A station that answers after the call timeout keeps the Reserve (and
	// the cancel that follows it) pending for most of the shutdown.
	s.connect(func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 3 * time.Second
	})
	r.press("r")
	r.waitForScreen("reserve EVSE-1: waiting for the station")
	r.press("q")
	waitForOutput(t, r.out, "shutting down")

	if err := r.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := r.waitExit(); code != 0 {
		t.Fatalf("exit code %d after a second SIGINT during shutdown, want 0 (it was killed by the signal); stderr:\n%s",
			code, r.stderr.String())
	}
	requireReleased(t, s, port)
}
