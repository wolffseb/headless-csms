package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

func probeConfig() *config.Config {
	var cfg config.Config
	cfg.Location.EVSEs = []config.EVSE{{UID: "EVSE-1", OCPPConnectorID: 1}}
	cfg.Charger.HeartbeatInterval = config.NewDuration(60 * time.Second)
	cfg.Charger.HeartbeatTimeout = config.NewDuration(90 * time.Second)
	return &cfg
}

func probeResult(heartbeat int, support map[string]string) socket.ProbeResult {
	one := 1
	r := socket.ProbeResult{Connectors: &one, HeartbeatIntervalSeconds: &heartbeat}
	for _, n := range needs {
		s := "yes"
		if v, ok := support[string(n.capability)]; ok {
			s = v
		}
		r.Capabilities = append(r.Capabilities, socket.ProbeCapability{Name: string(n.capability), Support: s})
	}
	return r
}

func TestEvaluateProbeCapabilities(t *testing.T) {
	t.Parallel()
	cfg := probeConfig()

	cases := []struct {
		name    string
		support map[string]string
		missing []string
		// unconfirmed is what the station did not report either way.
		unconfirmed []string
	}{
		{"everything", nil, nil, nil},
		// RemoteTrigger is optional: without it cpms only refreshes less eagerly.
		{"no trigger", map[string]string{"trigger_status": "no"}, nil, nil},
		// Optional and unreported: nothing to say.
		{"unknown trigger", map[string]string{"trigger_status": "unknown"}, nil, nil},
		{"no reservation", map[string]string{"reservation": "no"}, []string{"reservation"}, nil},
		// Not reported is not absent: the station may support it. Exit 0.
		{"unknown reservation", map[string]string{"reservation": "unknown"}, nil, []string{"reservation"}},
		{"absent unlock, unknown reservation", map[string]string{"unlock": "no", "reservation": "unknown"},
			[]string{"unlock"}, []string{"reservation"}},
	}
	for _, c := range cases {
		out := evaluateProbe(cfg, probeResult(60, c.support))
		if got := strings.Join(out.Missing, ","); got != strings.Join(c.missing, ",") {
			t.Errorf("%s: missing = %v, want %v", c.name, out.Missing, c.missing)
		}
		if got := strings.Join(out.Unconfirmed, ","); got != strings.Join(c.unconfirmed, ",") {
			t.Errorf("%s: unconfirmed = %v, want %v", c.name, out.Unconfirmed, c.unconfirmed)
		}
		if out.Missing == nil || out.Unconfirmed == nil {
			t.Errorf("%s: lists must be empty, not nil, for JSON", c.name)
		}
		if out.OK != (len(c.missing) == 0) {
			t.Errorf("%s: ok = %v", c.name, out.OK)
		}
	}
}

func TestEvaluateProbeHeartbeat(t *testing.T) {
	t.Parallel()
	cfg := probeConfig()

	cases := []struct {
		seconds int
		warning string
	}{
		{60, ""},
		{30, "the station heartbeats every 30s, but charger.heartbeat_interval is 60s"},
		// At or past the timeout the watchdog fires between heartbeats.
		{90, "not before charger.heartbeat_timeout (90s) runs out"},
		{300, "not before charger.heartbeat_timeout (90s) runs out"},
	}
	for _, c := range cases {
		out := evaluateProbe(cfg, probeResult(c.seconds, nil))
		got := strings.Join(out.Warnings, "\n")
		if c.warning == "" && got != "" || !strings.Contains(got, c.warning) {
			t.Errorf("heartbeat %ds: warnings %q, want %q", c.seconds, got, c.warning)
		}
		if !out.OK {
			t.Errorf("heartbeat %ds: a warning must not fail the probe", c.seconds)
		}
	}
}

func TestEvaluateProbeConnectorIDs(t *testing.T) {
	t.Parallel()
	cfg := probeConfig()
	cfg.Location.EVSEs = []config.EVSE{
		{UID: "A", OCPPConnectorID: 1},
		{UID: "B", OCPPConnectorID: 3},
	}
	two := 2
	r := probeResult(60, nil)
	r.Connectors = &two

	out := evaluateProbe(cfg, r)
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "location.evses[1] (B) maps to ocpp_connector_id 3, but the station reports 2 connectors") {
		t.Errorf("warnings = %q, want one naming connector id 3", out.Warnings)
	}
	if !out.OK {
		t.Error("a warning must not fail the probe")
	}

	cfg.Location.EVSEs[1].OCPPConnectorID = 2
	if out := evaluateProbe(cfg, r); len(out.Warnings) != 0 {
		t.Errorf("in-range ids: warnings = %q, want none", out.Warnings)
	}
}
