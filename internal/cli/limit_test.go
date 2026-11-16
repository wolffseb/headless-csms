package cli_test

import (
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// Ports 19660–19669 (smart charging C2, #50).

// powerW reads the first EVSE's power_w from status --json.
func (s *station) powerW() (float64, bool) {
	s.t.Helper()

	var st socket.Status
	if err := json.Unmarshal([]byte(s.mustCPMS("status", "--json")), &st); err != nil {
		s.t.Fatalf("decoding status --json: %v", err)
	}
	if len(st.EVSEs) == 0 || st.EVSEs[0].PowerW == nil {
		return 0, false
	}
	return *st.EVSEs[0].PowerW, true
}

// waitPowerAtMost waits for a reading of more than nothing and at most max
// W. The simulator draws 50 kW uncapped, so a reading under the limit is the
// limit at work.
func (s *station) waitPowerAtMost(max float64) {
	s.t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	last, seen := 0.0, false
	for time.Now().Before(deadline) {
		if p, ok := s.powerW(); ok {
			last, seen = p, true
			if p > 0 && p <= max {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("power never came down to %.0f W; last reading %.0f W (seen: %v)", max, last, seen)
}

func profilesOf(e socket.EVSEStatus) []socket.ChargingProfileStatus { return e.ChargingProfiles }

// TestLimitWhileReserved is the headline: the reserved-mode test from the
// README runbook, against the simulator.
func TestLimitWhileReserved(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19660)
	s.connect(func(o *simulator.Options) { o.MeterInterval = 100 * time.Millisecond })

	s.mustCPMS("reserve")
	s.waitStatus("EVSE-1 to be RESERVED", func(st socket.Status) bool { return st.EVSEs[0].Status == "RESERVED" })

	if out := s.mustCPMS("limit", "set", "--kw", "20"); !strings.Contains(out, "EVSE-1: charging profile #1 accepted: tx-default stack 0, 20.0 kW") {
		t.Errorf("limit set printed %q", out)
	}
	st := s.status()
	if st.EVSEs[0].Status != "RESERVED" || st.EVSEs[0].Reservation == nil {
		t.Errorf("setting a limit disturbed the reservation: %+v", st.EVSEs[0])
	}
	ps := profilesOf(st.EVSEs[0])
	if len(ps) != 1 || ps[0].ID != 1 || ps[0].Purpose != "tx-default" || ps[0].Unit != "W" ||
		len(ps[0].Periods) != 1 || ps[0].Periods[0].Limit != 20000 || ps[0].Source != "cli" {
		t.Fatalf("status profiles = %+v, want the 20 kW tx-default", ps)
	}
	if human := s.mustCPMS("status"); !strings.Contains(human, "as sent") || !strings.Contains(human, "#1  tx-default stack 0, 20.0 kW") {
		t.Errorf("status shows:\n%s\nwant the profile, marked as sent", human)
	}

	if out := s.mustCPMS("limit", "show"); !strings.Contains(out, "20.0 kW") || !strings.Contains(out, "FROM") {
		t.Errorf("limit show printed:\n%s\nwant a period table with 20.0 kW", out)
	}

	s.mustCPMS("start")
	st = s.waitStatus("the session to redeem the reservation", func(st socket.Status) bool {
		return st.EVSEs[0].Transaction != nil && st.EVSEs[0].Reservation == nil
	})
	txID := st.EVSEs[0].Transaction.ID
	s.waitPowerAtMost(20000)

	s.mustCPMS("limit", "set", "--purpose", "tx", "--kw", "10")
	ps = profilesOf(s.status().EVSEs[0])
	if len(ps) != 2 || ps[1].Purpose != "tx" || ps[1].TransactionID != txID || ps[1].Periods[0].Limit != 10000 {
		t.Fatalf("profiles after the tx limit = %+v, want the tx-default and a tx for %s", ps, txID)
	}
	s.waitPowerAtMost(10000)

	s.mustCPMS("stop")
	st = s.waitStatus("the session to end", func(st socket.Status) bool { return st.EVSEs[0].Transaction == nil })
	if ps := profilesOf(st.EVSEs[0]); len(ps) != 1 || ps[0].Purpose != "tx-default" {
		t.Errorf("profiles after the stop = %+v, want only the tx-default", ps)
	}

	if out := s.mustCPMS("limit", "clear", "--all"); !strings.Contains(out, "#1") {
		t.Errorf("limit clear printed %q, want the dropped record", out)
	}
	if ps := profilesOf(s.status().EVSEs[0]); len(ps) != 0 {
		t.Errorf("profiles after clear --all = %+v, want none", ps)
	}
}

func TestTxLimitWithoutASession(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19661)
	logs := &syncBuffer{}
	s.connect(func(o *simulator.Options) { o.Log = slog.New(slog.NewTextHandler(logs, nil)) })

	_, stderr, code := s.cpms("limit", "set", "--purpose", "tx", "--kw", "10")
	expectExit(t, "tx limit without a session", code, cli.ExitRefused, stderr, "no active transaction", "--force")
	if strings.Contains(logs.String(), "charging profile") {
		t.Errorf("a refused-locally profile reached the station; simulator log:\n%s", logs)
	}

	_, stderr, code = s.cpms("limit", "set", "--purpose", "tx", "--kw", "10", "--force")
	expectExit(t, "forced tx limit", code, cli.ExitRejected, stderr, "rejected the charging profile", "Rejected")
	if !strings.Contains(logs.String(), "TxProfile without a running transaction") {
		t.Errorf("the forced profile did not reach the station; simulator log:\n%s", logs)
	}
	if ps := profilesOf(s.status().EVSEs[0]); len(ps) != 0 {
		t.Errorf("a rejected profile was recorded: %+v", ps)
	}
}

func TestRejectedProfileIsNotRecorded(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19662)
	s.connect(func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectProfile })

	_, stderr, code := s.cpms("limit", "set", "--kw", "20")
	expectExit(t, "limit set", code, cli.ExitRejected, stderr, "EVSE-1", "Rejected")
	if ps := profilesOf(s.status().EVSEs[0]); len(ps) != 0 {
		t.Errorf("a rejected profile was recorded: %+v", ps)
	}
}

func TestProfilesSurviveARestart(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19663)
	s.connect()

	s.mustCPMS("limit", "set", "--kw", "20", "--stack", "3", "--for", "2h")
	s.mustCPMS("limit", "set", "--station", "--purpose", "station-max", "--amps", "100")
	before := s.status()

	s.stopRun()
	s.startRun()

	after := s.status()
	if !reflect.DeepEqual(after.EVSEs[0].ChargingProfiles, before.EVSEs[0].ChargingProfiles) ||
		!reflect.DeepEqual(after.ChargePoint.ChargingProfiles, before.ChargePoint.ChargingProfiles) {
		t.Fatalf("after restart:\n%+v\n%+v\nwant\n%+v\n%+v", after.EVSEs[0].ChargingProfiles, after.ChargePoint.ChargingProfiles,
			before.EVSEs[0].ChargingProfiles, before.ChargePoint.ChargingProfiles)
	}
	if ps := after.EVSEs[0].ChargingProfiles; len(ps) != 1 || ps[0].StackLevel != 3 || ps[0].ValidTo == nil {
		t.Errorf("EVSE profile after restart = %+v", ps)
	}

	// The next id carries on past the two handed out.
	data, err := os.ReadFile(filepath.Join(filepath.Dir(s.configPath), cli.DefaultStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Next int `json:"next_charging_profile_id"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.Next != 3 {
		t.Errorf("next_charging_profile_id = %d (%v), want 3", st.Next, err)
	}
}

func TestProfileSetFromAFile(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19664)
	s.connect()

	file := filepath.Join(t.TempDir(), "profile.json")
	writeFile(t, file, `{
  "purpose": "tx-default",
  "stack_level": 1,
  "kind": "relative",
  "schedule": {
    "unit": "A",
    "periods": [{"start_s": 0, "limit": 32, "phases": 3}, {"start_s": 1800, "limit": 16}]
  }
}`)
	if out := s.mustCPMS("profile", "set", "-f", file); !strings.Contains(out, "#1 accepted: tx-default stack 1, 32.0 A (+1 periods)") {
		t.Errorf("profile set printed %q", out)
	}
	ps := profilesOf(s.status().EVSEs[0])
	if len(ps) != 1 || len(ps[0].Periods) != 2 || ps[0].Periods[1].StartSeconds != 1800 || *ps[0].Periods[0].Phases != 3 {
		t.Errorf("recorded %+v", ps)
	}
	if out := s.mustCPMS("limit", "show", "--unit", "A"); !strings.Contains(out, "32.0 A") {
		t.Errorf("limit show printed:\n%s", out)
	}

	_, stderr, code := s.cpms("limit", "clear", "--id", "42")
	expectExit(t, "clearing an id the station does not hold", code, cli.ExitRejected, stderr, "no matching charging profile", "Unknown")
}

// Bad flags are refused from the command line alone: the socket never sees
// a connection.
func TestLimitFlagValidation(t *testing.T) {
	t.Parallel()

	path := runConfig(t, 19669)
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

	badFile := filepath.Join(t.TempDir(), "bad.json")
	writeFile(t, badFile, `{"purpose": "tx-default", "schedule": {"unit": "kW", "periods": [{"start_s": 0, "limit": 11}]}}`)
	unknownField := filepath.Join(t.TempDir(), "unknown.json")
	writeFile(t, unknownField, `{"purpose": "tx-default", "limit_kw": 11}`)

	cases := map[string][]string{
		"both units":             {"limit", "set", "--kw", "10", "--amps", "16"},
		"no unit":                {"limit", "set"},
		"station and tx-default": {"limit", "set", "--station", "--kw", "10"},
		"negative for":           {"limit", "set", "--kw", "10", "--for", "-1m"},
		"station-max on EVSE":    {"limit", "set", "--purpose", "station-max", "--kw", "10"},
		"unknown purpose":        {"limit", "set", "--purpose", "max", "--kw", "10"},
		"evse and station":       {"limit", "set", "--evse", "EVSE-1", "--station", "--purpose", "station-max", "--kw", "10"},
		"force without tx":       {"limit", "set", "--kw", "10", "--force"},
		"clear without what":     {"limit", "clear"},
		"clear id and all":       {"limit", "clear", "--id", "1", "--all"},
		"show zero duration":     {"limit", "show", "--duration", "0s"},
		"show in kW":             {"limit", "show", "--unit", "kW"},
		"profile without file":   {"profile", "set"},
		"profile in kW":          {"profile", "set", "-f", badFile},
		"profile unknown field":  {"profile", "set", "-f", unknownField},
		"unknown evse":           {"limit", "set", "--evse", "NOPE", "--kw", "10"},
	}
	for name, args := range cases {
		_, _, err := run(t, append(args, "-c", path, "--socket", sock)...)
		if code := cli.ExitCode(err); code != cli.ExitError {
			t.Errorf("%s: exit %d, want %d (err %v)", name, code, cli.ExitError, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Errorf("the socket saw %d connections, want none", n)
	}
}
