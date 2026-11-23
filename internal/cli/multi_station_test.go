package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// Test ports 19720-19729 belong to part B2 of #85.

// multiStation is `cpms run` serving the two coneva chargers, with a
// simulator per charger dialled in.
type multiStation struct {
	*station
	sims map[string]*simulator.Simulator
}

func newMultiStation(t *testing.T, port int) *multiStation {
	t.Helper()

	cfg := strings.NewReplacer(
		"0.0.0.0:9000", "127.0.0.1:"+strconv.Itoa(port),
		"0.0.0.0:8080", "127.0.0.1:"+strconv.Itoa(port+1000),
	).Replace(multiChargerYAML)
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, cfg)

	s := &station{t: t, port: port, configPath: path, socketPath: filepath.Join(shortDir(t), "cpms.sock")}
	s.startRun()
	return &multiStation{station: s, sims: map[string]*simulator.Simulator{}}
}

// connectCharger dials one simulated charger in and returns its simulator.
func (m *multiStation) connectCharger(id string, connectors ...int) *simulator.Simulator {
	m.t.Helper()

	n := 1
	if len(connectors) > 0 {
		n = connectors[0]
	}
	sim, err := simulator.New(simulator.Options{
		URL:        "ws://127.0.0.1:" + strconv.Itoa(m.port),
		ID:         id,
		Connectors: n,
		IDTag:      "RFID_PRUESTEL_000",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		m.t.Fatalf("simulator.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.t.Cleanup(func() {
		cancel()
		sim.Close()
		sim.Wait()
	})
	if err := sim.Connect(ctx); err != nil {
		m.t.Fatalf("simulator %s connect: %v", id, err)
	}
	m.sims[id] = sim
	return sim
}

// charger is one charger's section of the status.
func (m *multiStation) charger(st socket.Status, id string) socket.ChargerStatus {
	m.t.Helper()
	for _, ch := range st.Chargers {
		if ch.ChargePoint.ID == id {
			return ch
		}
	}
	m.t.Fatalf("status has no charger %s: %+v", id, st)
	return socket.ChargerStatus{}
}

func (m *multiStation) waitCharger(what, id string, cond func(socket.ChargerStatus) bool) socket.ChargerStatus {
	m.t.Helper()
	var got socket.ChargerStatus
	m.waitStatus(what, func(st socket.Status) bool {
		for _, ch := range st.Chargers {
			if ch.ChargePoint.ID == id {
				got = ch
				return cond(ch)
			}
		}
		return false
	})
	return got
}

func availableAndOnline(ch socket.ChargerStatus) bool {
	return ch.ChargePoint.Online && len(ch.EVSEs) == 1 && ch.EVSEs[0].Status == "AVAILABLE"
}

// TestTwoChargersOnOneRun is the coneva bench: start and stop reach the
// charger of the EVSE they name, and both RFIDs start sessions on both.
func TestTwoChargersOnOneRun(t *testing.T) {
	t.Parallel()
	m := newMultiStation(t, 19720)
	m.connectCharger("PRUESTEL_000")
	m.connectCharger("PRUESTEL_001")
	m.waitCharger("PRUESTEL_000 to be available", "PRUESTEL_000", availableAndOnline)
	m.waitCharger("PRUESTEL_001 to be available", "PRUESTEL_001", availableAndOnline)

	// Both tags on both chargers.
	for _, c := range []struct{ evse, charger, tag string }{
		{"PRUESTEL_000-1", "PRUESTEL_000", "RFID_PRUESTEL_000"},
		{"PRUESTEL_000-1", "PRUESTEL_000", "RFID_PRUESTEL_001"},
		{"PRUESTEL_001-1", "PRUESTEL_001", "RFID_PRUESTEL_000"},
		{"PRUESTEL_001-1", "PRUESTEL_001", "RFID_PRUESTEL_001"},
	} {
		m.mustCPMS("start", "--evse", c.evse, "--tag", c.tag)
		ch := m.waitCharger(c.evse+" to charge for "+c.tag, c.charger, func(ch socket.ChargerStatus) bool {
			return ch.EVSEs[0].Transaction != nil
		})
		if tx := ch.EVSEs[0].Transaction; tx.IDTag != c.tag || ch.EVSEs[0].UID != c.evse {
			t.Errorf("%s: session %+v on %s, want tag %s", c.charger, tx, ch.EVSEs[0].UID, c.tag)
		}

		// Stopping on one EVSE leaves the other charger's session alone.
		if c.charger == "PRUESTEL_000" {
			m.mustCPMS("start", "--evse", "PRUESTEL_001-1")
			m.waitCharger("PRUESTEL_001-1 to charge", "PRUESTEL_001", func(ch socket.ChargerStatus) bool {
				return ch.EVSEs[0].Transaction != nil
			})
			m.mustCPMS("stop", "--evse", c.evse)
			m.waitCharger("the session to end", c.charger, func(ch socket.ChargerStatus) bool {
				return ch.EVSEs[0].Transaction == nil
			})
			other := m.charger(m.status(), "PRUESTEL_001")
			if other.EVSEs[0].Transaction == nil {
				t.Fatal("stopping PRUESTEL_000-1 ended the session on PRUESTEL_001")
			}
			m.mustCPMS("stop", "--evse", "PRUESTEL_001-1")
			m.waitCharger("the session to end", "PRUESTEL_001", func(ch socket.ChargerStatus) bool {
				return ch.EVSEs[0].Transaction == nil
			})
			continue
		}
		m.mustCPMS("stop", "--evse", c.evse)
		m.waitCharger("the session to end", c.charger, func(ch socket.ChargerStatus) bool {
			return ch.EVSEs[0].Transaction == nil
		})
	}

	// An EVSE is still needed to name a station's connector.
	_, stderr, code := m.cpms("start")
	expectExit(t, "start without --evse", code, cli.ExitError, stderr, "--evse is required", "PRUESTEL_000-1", "PRUESTEL_001-1")
}

// TestStatusOfTwoChargers: a section per charger in the text, a chargers
// array in the JSON, and --charger narrows to the single-charger shape.
func TestStatusOfTwoChargers(t *testing.T) {
	t.Parallel()
	m := newMultiStation(t, 19721)
	m.connectCharger("PRUESTEL_000")
	m.waitCharger("PRUESTEL_000 to be available", "PRUESTEL_000", availableAndOnline)

	// PRUESTEL_001 never connects.
	m.mustCPMS("reserve", "--evse", "PRUESTEL_000-1", "--tag", "RFID_PRUESTEL_001")
	human := m.mustCPMS("status")
	i0, i1 := strings.Index(human, "PRUESTEL_000  online"), strings.Index(human, "PRUESTEL_001  offline")
	if i0 < 0 || i1 < i0 {
		t.Fatalf("status should have a section per charger, in config order:\n%s", human)
	}
	if res := strings.Index(human, "#1 RFID_PRUESTEL_001"); res < i0 || res > i1 {
		t.Errorf("the reservation should show in the section of the charger holding it:\n%s", human)
	}
	if !strings.Contains(human[i1:], "never seen") || strings.Count(human, "OCPI  not registered") != 1 {
		t.Errorf("status should say PRUESTEL_001 was never seen, and show OCPI once:\n%s", human)
	}

	var shape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.mustCPMS("status", "--json")), &shape); err != nil {
		t.Fatal(err)
	}
	if _, ok := shape["chargers"]; !ok {
		t.Errorf("multi-charger status --json has no chargers: %v", shape)
	}
	for _, k := range []string{"charge_point", "evses"} {
		if _, ok := shape[k]; ok {
			t.Errorf("multi-charger status --json has a top-level %s, which would pass for one charger of two", k)
		}
	}

	var one socket.Status
	if err := json.Unmarshal([]byte(m.mustCPMS("status", "--json", "--charger", "PRUESTEL_001")), &one); err != nil {
		t.Fatal(err)
	}
	if one.ChargePoint.ID != "PRUESTEL_001" || len(one.EVSEs) != 1 || one.EVSEs[0].UID != "PRUESTEL_001-1" || len(one.Chargers) != 0 {
		t.Errorf("status --charger PRUESTEL_001 = %+v, want that charger in the single shape", one)
	}
	_, stderr, code := m.cpms("status", "--charger", "NOBODY")
	expectExit(t, "status --charger NOBODY", code, cli.ExitError, stderr, "NOBODY", "PRUESTEL_000, PRUESTEL_001")
}

// TestProbeNeedsAChargerWithTwo: probe and config set are about a station, so
// they have to be told which.
func TestProbeNeedsAChargerWithTwo(t *testing.T) {
	t.Parallel()
	m := newMultiStation(t, 19722)
	// The two stations differ in their connector count, which is what tells
	// whose report a probe printed.
	m.connectCharger("PRUESTEL_000")
	m.connectCharger("PRUESTEL_001", 2)
	m.waitCharger("PRUESTEL_001 to be online", "PRUESTEL_001", func(ch socket.ChargerStatus) bool {
		return ch.ChargePoint.Online && len(ch.EVSEs) >= 2 // core adds the connector config lacks
	})

	_, stderr, code := m.cpms("probe")
	expectExit(t, "probe", code, cli.ExitError, stderr, "--charger", "PRUESTEL_000, PRUESTEL_001")

	stdout := m.mustCPMS("probe", "--json", "--charger", "PRUESTEL_001")
	var out struct {
		ChargePoint struct{ ID string } `json:"charge_point"`
		Connectors  *int                `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.ChargePoint.ID != "PRUESTEL_001" || out.Connectors == nil || *out.Connectors != 2 {
		t.Errorf("probe --charger PRUESTEL_001 reported %+v with connectors %v, want PRUESTEL_001's 2", out.ChargePoint, out.Connectors)
	}

	_, stderr, code = m.cpms("probe", "--charger", "NOBODY")
	expectExit(t, "probe --charger NOBODY", code, cli.ExitError, stderr, "NOBODY", "PRUESTEL_000, PRUESTEL_001")

	_, stderr, code = m.cpms("config", "set", "ConnectionTimeOut", "60")
	expectExit(t, "config set", code, cli.ExitError, stderr, "--charger")
	out2 := m.mustCPMS("config", "set", "ConnectionTimeOut", "60", "--charger", "PRUESTEL_000")
	if !strings.Contains(out2, "ConnectionTimeOut") || !strings.Contains(out2, "on PRUESTEL_000 (Accepted)") {
		t.Errorf("config set printed %q, want it to name PRUESTEL_000", out2)
	}
	if run := m.runOut.String(); !strings.Contains(run, "waiting for 2 charge points to connect") {
		t.Errorf("cpms run printed %q, want the several-chargers waiting line", run)
	}

	_, stderr, code = m.cpms("limit", "set", "--station", "--purpose", "station-max", "--kw", "10")
	expectExit(t, "limit set --station", code, cli.ExitError, stderr, "--charger")
	_, stderr, code = m.cpms("limit", "show", "--evse", "PRUESTEL_000-1", "--charger", "PRUESTEL_001")
	expectExit(t, "limit show with the wrong charger", code, cli.ExitError, stderr, "PRUESTEL_000-1", "PRUESTEL_001")
}

// TestSimulateChargerPicksTheChargerByID is how end-to-end testers bring up
// the bench: one `cpms simulate charger --id ...` per charger.
func TestSimulateChargerPicksTheChargerByID(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, cfgPath, strings.NewReplacer(
		"0.0.0.0:9000", "127.0.0.1:19723",
		"0.0.0.0:8080", "127.0.0.1:20723",
	).Replace(multiChargerYAML))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	csmsOut, csmsDone := startRun(t, ctx, "run", "-c", cfgPath, "--socket", filepath.Join(shortDir(t), "cpms.sock"))
	waitForOutput(t, csmsOut, "ws://127.0.0.1:19723")

	// Without --id there is no default charger.
	_, _, err := run(t, "simulate", "charger", "-c", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "PRUESTEL_000, PRUESTEL_001") {
		t.Errorf("simulate charger without --id: err = %v, want the charger ids listed", err)
	}
	if _, _, err = run(t, "simulate", "charger", "-c", cfgPath, "--id", "NOBODY"); err == nil || !strings.Contains(err.Error(), "NOBODY") {
		t.Errorf("simulate charger --id NOBODY: err = %v, want it named", err)
	}

	simOut, simDone := startRun(t, ctx, "simulate", "charger", "-c", cfgPath, "--id", "PRUESTEL_001")
	waitForOutput(t, simOut, "simulating PRUESTEL_001 (1 connectors")
	waitForOutput(t, csmsOut, "PRUESTEL_001 connected")
	waitForOutput(t, csmsOut, "PRUESTEL_001-1 UNKNOWN→AVAILABLE")
	if strings.Contains(csmsOut.String(), "PRUESTEL_000 connected") {
		t.Errorf("--id PRUESTEL_001 brought up PRUESTEL_000:\n%s", csmsOut.String())
	}

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
