package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// fixedBoot pins what the simulator says about itself, so that the probe
// output can be compared byte for byte.
func fixedBoot(o *simulator.Options) {
	o.Vendor, o.Model, o.Firmware, o.SerialNumber = "Alpitronic", "HYC300", "1.2.3", "SIM-0001"
}

// compareGolden compares got with testdata/name, or rewrites it with -update.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()

	golden := filepath.Join("testdata", name)
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
		t.Errorf("output differs from %s (rerun with -update if the change is intended):\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}

// expectLines checks that each pattern matches somewhere in a line of out.
func expectLines(t *testing.T, out string, patterns ...string) {
	t.Helper()
	for _, p := range patterns {
		if !regexp.MustCompile(`(?m)^.*` + p).MatchString(out) {
			t.Errorf("output has no line matching %q:\n%s", p, out)
		}
	}
}

func TestProbeNormalStation(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19350)
	s.connect(fixedBoot)

	out := s.mustCPMS("probe")
	expectLines(t, out,
		`ALP-HYC-001  OCPP 1\.6`,
		`Alpitronic HYC300, serial SIM-0001, firmware 1\.2\.3`,
		`Core +✓ +required`,
		`Reservation +✓ +required`,
		`RemoteTrigger +✓ +optional`,
		`Every capability cpms requires is present\.`,
	)
	if strings.Contains(out, "All keys") {
		t.Error("the raw key list is shown without --raw")
	}
	compareGolden(t, "probe.golden.txt", out)
}

func TestProbeJSONAndRaw(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19351)
	s.connect(fixedBoot)

	out := s.mustCPMS("probe", "--json")
	compareGolden(t, "probe.golden.json", out)

	var report struct {
		OK  bool                 `json:"ok"`
		Raw []socket.ProbeRawKey `json:"raw"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("probe --json is not JSON: %v", err)
	}
	if !report.OK || len(report.Raw) != 10 {
		t.Fatalf("ok = %v with %d raw keys, want ok with the simulator's 10", report.OK, len(report.Raw))
	}

	// --raw lists every key the station reported, with its value.
	raw := s.mustCPMS("probe", "--raw")
	_, keys, found := strings.Cut(raw, "All keys\n")
	if !found {
		t.Fatalf("probe --raw has no key list:\n%s", raw)
	}
	for _, k := range report.Raw {
		if !regexp.MustCompile(`(?m)^  ` + k.Key + ` +r[ow]  ` + regexp.QuoteMeta(k.Value) + `$`).MatchString(keys) {
			t.Errorf("--raw does not list %s = %q:\n%s", k.Key, k.Value, keys)
		}
	}
}

func TestProbeWithoutReservationExits10(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19352)
	s.connect(func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectReserve })

	stdout, stderr, code := s.cpms("probe")
	expectExit(t, "probe", code, cli.ExitMissingCapability, stderr, "reservation")
	expectLines(t, stdout,
		`Reservation +✗ +required`,
		`Reservation is missing: OCPI RESERVE_NOW cannot be passed on to the station`,
		// What is there is still reported as there.
		`Core +✓ +required`,
	)
}

func TestProbeWarnsAboutConnectorCount(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19353)
	s.connect(func(o *simulator.Options) { o.Connectors = 2 })

	expectLines(t, s.mustCPMS("probe"), `! +the station reports 2 connectors, but config has 1 EVSE`)
}

// A station that does not report a key has not said no: the key is "not
// reported", and nothing else about the report changes.
func TestProbeUnreportedKeys(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19354)
	s.connect(func(o *simulator.Options) {
		o.UnreportedKeys = []string{v16.KeyLocalPreAuthorize, v16.KeyNumberOfConnectors}
	})

	expectLines(t, s.mustCPMS("probe"),
		`LocalPreAuthorize +not reported`,
		`NumberOfConnectors +not reported`,
		`LocalAuthorizeOffline +false`,
		`\? +the station did not report how many connectors it has`,
		`ok +heartbeat every 60s`,
		`Every capability cpms requires is present\.`,
	)
}

func TestProbeNotConnectedExits3(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19355)

	// The URL to configure is exactly the one `config validate` prints.
	summary := s.mustCPMS("config", "validate")
	m := regexp.MustCompile(`Station CSMS URL\s+(\S+)`).FindStringSubmatch(summary)
	if m == nil {
		t.Fatalf("no CSMS URL in the config summary:\n%s", summary)
	}

	_, stderr, code := s.cpms("probe")
	expectExit(t, "probe", code, cli.ExitNotConnected, stderr,
		"not connected", "  "+m[1]+"\n", "has not connected since cpms run started")

	// Once the station has been and gone, it says when and why.
	s.connect()
	s.sim.Close()
	s.sim.Wait()
	s.waitStatus("the station to go offline", func(st socket.Status) bool { return !st.ChargePoint.Online })

	_, stderr, code = s.cpms("probe")
	expectExit(t, "probe", code, cli.ExitNotConnected, stderr, m[1], "It last disconnected at")
}

func TestProbeTimeoutExits7(t *testing.T) {
	// Cleanups run last-in first-out, so this runs after the station is gone.
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	s := newStation(t, 19356, "heartbeat_timeout: 90s", "heartbeat_timeout: 90s\n  call_timeout: 1s")
	s.connect(func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 3 * time.Second
	})

	start := time.Now()
	_, stderr, code := s.cpms("probe")
	expectExit(t, "probe", code, cli.ExitTimeout, stderr, "did not answer")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %s, want about the 1s call timeout", elapsed)
	}
}
