package cli_test

import (
	"bytes"
	"io"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A one-shot command must not talk to the terminal. Bubble Tea v1 asked for
// the background colour (OSC 11, then a cursor position report) from a
// package init, in every cpms process with a terminal for stdout, and waited
// up to 5 s for a reply: a serial console, an expect script or `script` that
// never answers stalled every command. The pty here never answers.
func TestOneShotCommandsDoNotQueryTheTerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("needs a pseudo-terminal")
	}
	t.Parallel()

	bin := buildCPMS(t)
	for _, args := range [][]string{{"--help"}, {"version"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()

			c := exec.Command(bin, args...)
			ptmx, err := pty.Start(c)
			if err != nil {
				t.Fatalf("starting on a pty: %v", err)
			}
			defer ptmx.Close()

			drawn := make(chan []byte, 1)
			go func() {
				b, _ := io.ReadAll(ptmx) // ends with EIO once the child is gone
				drawn <- b
			}()

			start := time.Now()
			exited := make(chan error, 1)
			go func() { exited <- c.Wait() }()
			select {
			case err := <-exited:
				if err != nil {
					t.Fatalf("cpms %v: %v", args, err)
				}
			case <-time.After(3 * time.Second):
				_ = c.Process.Kill()
				<-exited
				t.Fatalf("cpms %v still running after 3 s on a terminal that never answers", args)
			}
			// 5 s is what a query costs; anything near it is the bug. The
			// bound leaves room for a slow CI machine.
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("cpms %v took %v on a silent terminal", args, took)
			}

			out := <-drawn
			for _, query := range []string{"\x1b]11;?", "\x1b[6n", "\x1b]10;?"} {
				if bytes.Contains(out, []byte(query)) {
					t.Errorf("cpms %v wrote the terminal query %q:\n%q", args, query, out)
				}
			}
		})
	}
}
