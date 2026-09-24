package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSimulateChargerAgainstRun is the two-terminal demo, run as one test:
// `cpms run` in one process and `cpms simulate charger` in another, with no
// flags beyond the shared config.
func TestSimulateChargerAgainstRun(t *testing.T) {
	t.Parallel()

	const addr = "127.0.0.1:19200"
	path := runConfig(t, 19200)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	csmsOut, csmsDone := startRun(t, ctx, "run", "-c", path)
	waitForOutput(t, csmsOut, "ws://"+addr)

	simOut, simDone := startRun(t, ctx, "simulate", "charger", "-c", path, "--log-level", "debug")

	// The simulator derives everything it needs from the same config file.
	waitForOutput(t, simOut, "simulating ALP-HYC-001")
	waitForOutput(t, simOut, "ws://"+addr)
	waitForOutput(t, simOut, "booted")

	// And the CSMS sees a real station: connected, booted, connector status.
	waitForOutput(t, csmsOut, "charge point connected")
	waitForOutput(t, csmsOut, "booted (Alpitronic")
	waitForOutput(t, csmsOut, "EVSE-1 UNKNOWN→AVAILABLE")

	cancel()

	for _, done := range []<-chan error{simDone, csmsDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("command returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a command did not shut down after its context was cancelled")
		}
	}
}

func TestSimulateChargerRejectsAnUnknownScenario(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19201)

	_, _, err := run(t, "simulate", "charger", "-c", path, "--scenario", "chaos")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "chaos") {
		t.Errorf("error %q should name the bad scenario", err)
	}
}

func TestSimulateChargerNeedsATargetWithoutAConfig(t *testing.T) {
	t.Parallel()

	// Without a readable config there are no defaults to fall back on, so the
	// command has to say what it is missing rather than dial nothing.
	_, _, err := run(t, "simulate", "charger", "-c", "no-such-config.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--csms") || !strings.Contains(err.Error(), "--id") {
		t.Errorf("error %q should name the flags that would fix it", err)
	}
}
