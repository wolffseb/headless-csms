package cli_test

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// Smart charging B (#48) uses ports 19640–19649.

// TestStatusShowsThePowerTheStationReports: the simulator charges at 50 kW,
// and status shows it while the session runs and drops it once it stops.
func TestStatusShowsThePowerTheStationReports(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19640)
	s.connect(func(o *simulator.Options) { o.MeterInterval = 50 * time.Millisecond })

	s.mustCPMS("start")
	st := s.waitStatus("a 50 kW reading", func(st socket.Status) bool {
		return st.EVSEs[0].PowerW != nil && *st.EVSEs[0].PowerW == 50_000
	})
	e := st.EVSEs[0]
	if e.CurrentA == nil || *e.CurrentA != 125 || e.ReadingAt == nil {
		t.Errorf("reading = current %v at %v, want 125 A and a time", e.CurrentA, e.ReadingAt)
	}
	if e.PowerOfferedW != nil || e.CurrentOfferedA != nil {
		t.Errorf("offered %v W / %v A, want none: the simulator does not send them", e.PowerOfferedW, e.CurrentOfferedA)
	}
	human := s.mustCPMS("status")
	if !regexp.MustCompile(`METER`).MatchString(human) ||
		!regexp.MustCompile(`50\.0 kW 125\.0 A, \d+s ago`).MatchString(human) {
		t.Errorf("status shows:\n%s\nwant the reading in kW and A with its age", human)
	}

	s.mustCPMS("stop")
	st = s.waitStatus("the transaction to end", func(st socket.Status) bool { return st.EVSEs[0].Transaction == nil })
	if e := st.EVSEs[0]; e.PowerW != nil || e.CurrentA != nil || e.ReadingAt != nil {
		t.Errorf("after stop: power %v, current %v, reading at %v; want none", e.PowerW, e.CurrentA, e.ReadingAt)
	}
	raw := s.mustCPMS("status", "--json")
	for _, field := range []string{"power_w", "current_a", "reading_at"} {
		if strings.Contains(raw, field) {
			t.Errorf("status --json after stop still has %s:\n%s", field, raw)
		}
	}
}

func TestConfigSet(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19641)
		s.connect()

		out := s.mustCPMS("config", "set", "ConnectionTimeOut", "95")
		if !strings.Contains(out, "ConnectionTimeOut") || !strings.Contains(out, "Accepted") {
			t.Errorf("config set printed %q, want the key and the station's answer", out)
		}

		// The station reports the new value when asked again.
		var report struct {
			Raw []socket.ProbeRawKey `json:"raw"`
		}
		if err := json.Unmarshal([]byte(s.mustCPMS("probe", "--json")), &report); err != nil {
			t.Fatal(err)
		}
		var got string
		for _, k := range report.Raw {
			if k.Key == "ConnectionTimeOut" {
				got = k.Value
			}
		}
		if got != "95" {
			t.Errorf("GetConfiguration reports ConnectionTimeOut = %q, want 95", got)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19642)
		s.connect()
		_, stderr, code := s.cpms("config", "set", "NoSuchKey", "1")
		expectExit(t, "config set", code, cli.ExitRejected, stderr,
			"NoSuchKey", "rejected the configuration change", "NotSupported", "does not know this key")
	})

	t.Run("read-only key", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19643)
		s.connect()
		_, stderr, code := s.cpms("config", "set", "NumberOfConnectors", "4")
		expectExit(t, "config set", code, cli.ExitRejected, stderr, "NumberOfConnectors", "Rejected")
	})

	t.Run("not connected", func(t *testing.T) {
		t.Parallel()
		s := newStation(t, 19644)
		_, stderr, code := s.cpms("config", "set", "HeartbeatInterval", "60")
		expectExit(t, "config set", code, cli.ExitNotConnected, stderr, "HeartbeatInterval", "not connected")
	})
}

// TestConfigSetRebootRequired needs a station that answers RebootRequired,
// which the simulator never does, so a stand-in for `cpms run` answers on
// the socket instead.
func TestConfigSetRebootRequired(t *testing.T) {
	t.Parallel()

	path := filepath.Join(shortDir(t), "cpms.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"ok":true,"result":{"key":"MeterValuesSampledData","value":"Power.Active.Import","status":"RebootRequired"}}` + "\n"))
	}()

	stdout, stderr, err := run(t, "config", "set", "MeterValuesSampledData", "Power.Active.Import",
		"-c", runConfig(t, 19645), "--socket", path)
	if err != nil {
		t.Fatalf("config set: %v, want exit 0 for RebootRequired", err)
	}
	if !strings.Contains(stdout, "MeterValuesSampledData") || !strings.Contains(stdout, "RebootRequired") {
		t.Errorf("stdout = %q, want the key and the answer", stdout)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "reboot") {
		t.Errorf("stderr = %q, want a reboot warning", stderr)
	}
}
