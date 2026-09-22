package cli_test

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

// syncBuffer lets the test read output while the command is still writing it.
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

// runConfig writes a config file bound to the given OCPP port, with any
// further substitutions applied, and returns its path.
//
// Ports are fixed rather than 0 because cpms rejects port 0 on purpose: a
// listener a charger has to dial cannot live on a random port.
func runConfig(t *testing.T, ocppPort int, replacements ...string) string {
	t.Helper()

	base := `
charger:
  id: ALP-HYC-001
  ocpp_version: "1.6"
  heartbeat_interval: 60s
  heartbeat_timeout: 90s
server:
  ocpp_bind: 127.0.0.1:OCPP_PORT
  ocpi_bind: 127.0.0.1:OCPI_PORT
auth:
  default_id_tag: "04A1B2C3D4"
ocpi:
  country_code: DE
  party_id: FRY
  public_base_url: http://127.0.0.1:8080/ocpi
  token_a: "token-a-secret"
  token_c: "token-c-secret"
location:
  id: OFFICE-01
  address: Musterstrasse 1
  city: Berlin
  country: DEU
  coordinates:
    latitude: "52.520008"
    longitude: "13.404954"
  evses:
    - uid: EVSE-1
      evse_id: DE*FRY*E001*1
      ocpp_connector_id: 1
      connectors:
        - id: "1"
          standard: IEC_62196_T2_COMBO
          format: CABLE
          power_type: DC
          max_voltage: 920
          max_amperage: 500
`
	base = strings.Replace(base, "OCPP_PORT", strconv.Itoa(ocppPort), 1)
	// The two listeners may not share a port; the OCPI one is unused until a
	// later step, so it only has to be distinct.
	base = strings.Replace(base, "OCPI_PORT", strconv.Itoa(ocppPort+1000), 1)

	for i := 0; i+1 < len(replacements); i += 2 {
		base = strings.Replace(base, replacements[i], replacements[i+1], 1)
	}

	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// startRun launches `cpms run` in the background and returns its output buffer
// and a channel carrying its exit error.
func startRun(t *testing.T, ctx context.Context, args ...string) (*syncBuffer, <-chan error) {
	t.Helper()

	out := &syncBuffer{}
	done := make(chan error, 1)

	cmd := cli.NewRootCommand()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)

	go func() { done <- cmd.ExecuteContext(ctx) }()
	return out, done
}

// waitForOutput blocks until the command has printed the given text.
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output:\n%s", want, out.String())
}

func TestRunServesAndShutsDownCleanly(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19101)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, done := startRun(t, ctx, "run", "-c", path)

	// The startup line has to name the URL the station must be pointed at:
	// getting that wrong is the most likely reason a charger never appears.
	waitForOutput(t, out, "ws://127.0.0.1:")
	waitForOutput(t, out, "/ocpp/ALP-HYC-001")

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not shut down after its context was cancelled")
	}
	if !strings.Contains(out.String(), "shutting down") {
		t.Errorf("expected a shutdown message, got:\n%s", out.String())
	}
}

func TestRunAcceptsAChargePointConnection(t *testing.T) {
	t.Parallel()

	const addr = "127.0.0.1:19100"
	path := runConfig(t, 19100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, done := startRun(t, ctx, "run", "-c", path, "--log-level", "debug")
	waitForOutput(t, out, "ws://"+addr)

	client := ocpptest.MustDial(t, "ws://"+addr+"/ocpp/ALP-HYC-001")
	client.MustCall("BootNotification", map[string]string{
		"chargePointVendor": "Alpitronic",
		"chargePointModel":  "HYC300",
	})
	client.MustCall("StatusNotification", map[string]any{
		"connectorId": 1, "errorCode": "NoError", "status": "Charging",
	})

	// The event log is what an operator watches while wiring up a station.
	waitForOutput(t, out, "EVSE-1 UNKNOWN→CHARGING")

	client.Close()
	cancel()
	<-done
}

func TestRunRejectsAnUnknownLogLevel(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19102)
	_, _, err := run(t, "run", "-c", path, "--log-level", "chatty")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "chatty") {
		t.Errorf("error %q should name the bad level", err)
	}
}

func TestRunRejectsAnUnimplementedOCPPVersion(t *testing.T) {
	t.Parallel()

	// Failing here beats accepting the config and then rejecting the
	// charger's handshake with a confusing "no common version".
	path := runConfig(t, 19103, `ocpp_version: "1.6"`, `ocpp_version: "2.0.1"`)

	_, _, err := run(t, "run", "-c", path)
	if err == nil {
		t.Fatal("expected an error for an unimplemented version")
	}
	if !strings.Contains(err.Error(), "2.0.1") || !strings.Contains(err.Error(), "1.6") {
		t.Errorf("error %q should name both the requested and the supported version", err)
	}
}

func TestRunReportsConfigProblems(t *testing.T) {
	t.Parallel()

	_, stderr, err := run(t, "run", "-c", "no-such-config.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if stderr != "" && !strings.Contains(stderr, "no-such-config.yaml") {
		t.Errorf("stderr = %q", stderr)
	}
}
