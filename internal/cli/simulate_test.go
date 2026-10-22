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

func TestSimulateChargerRejectsANegativeMeterInterval(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19201)

	_, _, err := run(t, "simulate", "charger", "-c", path, "--meter-interval", "-10s")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "meter interval") {
		t.Errorf("error %q should name the meter interval", err)
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

// TestSimulateChargerOfflineTx is the scenario as a downstream harness runs
// it: `cpms run` is killed under a charging station and started again, and
// the restarted CSMS receives the first session's stop and the whole second
// session, which happened while it was gone.
func TestSimulateChargerOfflineTx(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19951)

	simCtx, stopSim := context.WithCancel(context.Background())
	defer stopSim()
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()

	csmsOut, csmsDone := startRun(t, runCtx, "run", "-c", path)
	waitForOutput(t, csmsOut, "ws://127.0.0.1:19951")

	simOut, simDone := startRun(t, simCtx, "simulate", "charger", "-c", path,
		"--scenario", "offline-tx", "--meter-interval", "200ms")
	waitForOutput(t, csmsOut, "transaction 1 started on EVSE-1")

	kill()
	if err := <-csmsDone; err != nil {
		t.Fatalf("cpms run returned %v", err)
	}
	waitForOutput(t, simOut, "offline part done")

	runCtx, kill = context.WithCancel(context.Background())
	defer kill()
	csmsOut, csmsDone = startRun(t, runCtx, "run", "-c", path)
	waitForOutput(t, csmsOut, "transaction 1 stopped")
	waitForOutput(t, csmsOut, "transaction 2 started on EVSE-1")
	waitForOutput(t, csmsOut, "transaction 2 stopped")

	stopSim()
	kill()
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

// TestSimulateChargerReconnectsWhenRunComesBack kills `cpms run` under a
// `cpms simulate charger --reconnect` and starts it again on the same port:
// the simulator must connect again, boot again and report its connectors
// again, with the real backoff (the first redial comes within 1 s).
func TestSimulateChargerReconnectsWhenRunComesBack(t *testing.T) {
	t.Parallel()

	const addr = "127.0.0.1:19950"
	path := runConfig(t, 19950)

	simCtx, stopSim := context.WithCancel(context.Background())
	defer stopSim()
	runCtx, kill := context.WithCancel(context.Background())
	defer kill()

	csmsOut, csmsDone := startRun(t, runCtx, "run", "-c", path)
	waitForOutput(t, csmsOut, "ws://"+addr)

	simOut, simDone := startRun(t, simCtx, "simulate", "charger", "-c", path, "--reconnect")
	waitForOutput(t, csmsOut, "EVSE-1 UNKNOWN→AVAILABLE")

	kill()
	select {
	case err := <-csmsDone:
		if err != nil {
			t.Fatalf("cpms run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cpms run did not shut down")
	}
	waitForOutput(t, simOut, "reconnecting")

	// The same config, so the same port and state file.
	runCtx, kill = context.WithCancel(context.Background())
	defer kill()
	csmsOut, csmsDone = startRun(t, runCtx, "run", "-c", path)
	waitForOutput(t, csmsOut, "charge point connected")
	waitForOutput(t, csmsOut, "booted (Alpitronic")
	waitForOutput(t, csmsOut, "EVSE-1 UNKNOWN→AVAILABLE")

	select {
	case err := <-simDone:
		t.Fatalf("the simulator exited across the restart: %v", err)
	default:
	}

	stopSim()
	kill()
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
