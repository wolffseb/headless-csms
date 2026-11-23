package cli

import (
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// configChecks reads the EVSEs of the probed charger only: another charger's
// EVSEs neither count towards the connector total nor show up as strays.
func TestConfigChecksAreScopedToTheProbedCharger(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Chargers: []config.Charger{{ID: "A"}, {ID: "B"}},
		Location: config.Location{EVSEs: []config.EVSE{
			{UID: "A-1", Charger: "A", OCPPConnectorID: 1},
			{UID: "B-1", Charger: "B", OCPPConnectorID: 1},
			{UID: "B-2", Charger: "B", OCPPConnectorID: 3},
		}},
	}
	two, one := 2, 1
	tests := []struct {
		name       string
		charger    string
		connectors int
		want       []string // substrings, one per check, in order
	}{
		{"B with two connectors", "B", two, []string{
			"location.evses[2] (B-2) maps to ocpp_connector_id 3, but the station reports 2 connectors",
		}},
		{"A with one connector", "A", one, []string{"1 connector, matching the 1 EVSE in config"}},
		{"B with one connector", "B", one, []string{"the station reports 1 connector, but config has 2 EVSEs"}},
	}
	for _, tt := range tests {
		r := socket.ProbeResult{Connectors: &tt.connectors}
		r.ChargePoint.ID = tt.charger
		var got []string
		for _, c := range configChecks(cfg, r) {
			if strings.Contains(c.text, "connector") {
				got = append(got, c.text)
			}
		}
		if len(got) != len(tt.want) {
			t.Errorf("%s: connector checks = %q, want %d", tt.name, got, len(tt.want))
			continue
		}
		for i, w := range tt.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: check %d = %q, want it to contain %q", tt.name, i, got[i], w)
			}
		}
		for _, c := range got {
			if strings.Contains(c, "A-1") || strings.Contains(c, "B-1") && tt.charger != "B" {
				t.Errorf("%s: check %q names an EVSE of another charger", tt.name, c)
			}
		}
	}
}
