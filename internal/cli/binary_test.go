package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// buildCPMS compiles cmd/cpms into a temporary directory and returns its path.
func buildCPMS(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "cpms")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/cpms")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// execCPMS runs the built binary and returns its two streams separately.
func execCPMS(t *testing.T, bin string, args ...string) (stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	c := exec.Command(bin, args...)
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		t.Fatalf("cpms %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String(), errOut.String()
}

// TestReportsGoToStdout runs the real binary, because the in-process tests
// call SetOut and so cannot see which stream cobra's cmd.Print* picks: it
// is stderr unless SetOut was called. Scripts pipe stdout (`cpms probe
// --json | jq`), so every report has to land there, and stderr stays empty
// on success.
func TestReportsGoToStdout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	t.Parallel()

	bin := buildCPMS(t)
	const port = 19610
	s := &station{
		t:          t,
		port:       port,
		configPath: runConfig(t, port),
		socketPath: filepath.Join(shortDir(t), "cpms.sock"),
	}

	stdout, stderr := execCPMS(t, bin, "config", "validate", "-c", s.configPath)
	if !strings.Contains(stdout, s.configPath+" is valid.") {
		t.Errorf("config validate: stdout lacks the verdict:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("config validate wrote to stderr:\n%s", stderr)
	}

	stdout, stderr = execCPMS(t, bin, "version")
	if strings.TrimSpace(stdout) == "" || stderr != "" {
		t.Errorf("version: stdout %q, stderr %q", stdout, stderr)
	}

	// `cpms run` itself is the binary too, so its startup lines are checked
	// on the real streams. Its log goes to stderr, which is fine.
	runOut, runErr := &syncBuffer{}, &syncBuffer{}
	run := exec.Command(bin, "run", "-c", s.configPath, "--socket", s.socketPath)
	run.Stdout, run.Stderr = runOut, runErr
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- run.Wait() }()
	t.Cleanup(func() {
		_ = run.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = run.Process.Kill()
			t.Error("cpms run did not stop on interrupt")
		}
		if !strings.Contains(runOut.String(), "shutting down") {
			t.Errorf("cpms run: \"shutting down\" is not on stdout:\n%s", runOut.String())
		}
	})
	waitForOutput(t, runOut, "control socket "+s.socketPath)
	for _, want := range []string{"cpms listening for ALP-HYC-001", "OCPI versions endpoint", "waiting for the charge point"} {
		if !strings.Contains(runOut.String(), want) {
			t.Errorf("cpms run: %q is not on stdout:\n%s\nstderr:\n%s", want, runOut.String(), runErr.String())
		}
	}

	s.connect(fixedBoot)
	sock := []string{"-c", s.configPath, "--socket", s.socketPath}

	stdout, stderr = execCPMS(t, bin, append([]string{"probe", "--json"}, sock...)...)
	var report struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.OK {
		t.Errorf("probe --json: stdout is not an ok report (%v):\n%s", err, stdout)
	}
	if stderr != "" {
		t.Errorf("probe --json wrote to stderr:\n%s", stderr)
	}

	stdout, stderr = execCPMS(t, bin, append([]string{"probe"}, sock...)...)
	for _, want := range []string{"ALP-HYC-001  OCPP 1.6", "Features", "Every capability cpms requires is present."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("probe: %q is not on stdout:\n%s", want, stdout)
		}
	}
	if stderr != "" {
		t.Errorf("probe wrote to stderr:\n%s", stderr)
	}

	stdout, stderr = execCPMS(t, bin, append([]string{"status", "--json"}, sock...)...)
	if !json.Valid([]byte(stdout)) || stderr != "" {
		t.Errorf("status --json: stdout %q, stderr %q", stdout, stderr)
	}

	stdout, stderr = execCPMS(t, bin, append([]string{"status"}, sock...)...)
	if !strings.HasPrefix(stdout, "ALP-HYC-001  online") || stderr != "" {
		t.Errorf("status: stdout %q, stderr %q", stdout, stderr)
	}

	stdout, stderr = execCPMS(t, bin, append([]string{"reserve", "--expires", "1m"}, sock...)...)
	if !strings.Contains(stdout, "reserved for") || stderr != "" {
		t.Errorf("reserve: stdout %q, stderr %q", stdout, stderr)
	}

	stdout, stderr = execCPMS(t, bin, append([]string{"cancel"}, sock...)...)
	if strings.TrimSpace(stdout) == "" || stderr != "" {
		t.Errorf("cancel: stdout %q, stderr %q", stdout, stderr)
	}
}

// TestNoCobraPrint keeps cobra's Print* and OutOrStderr out of the command
// code, whatever the receiver is called: both write to stderr in the real
// binary (see TestReportsGoToStdout), and the in-process tests would not
// notice. fmt.Print* is banned too, since it bypasses the writer the tests
// capture. Report output goes through fmt.Fprint*(cmd.OutOrStdout(), ...);
// messages meant for stderr use cmd.ErrOrStderr() explicitly.
func TestNoCobraPrint(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	cobraPrint := regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\.(Print(ln|f)?|OutOrStderr)\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if cobraPrint.MatchString(line) {
				t.Errorf("%s:%d: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
