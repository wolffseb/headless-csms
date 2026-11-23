package control_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/control"
)

// With several chargers the log line for a station configuration change says
// which charger took it.
func TestChangeConfigurationLogNamesTheCharger(t *testing.T) {
	t.Parallel()

	r := newMultiRig(t)
	var logged bytes.Buffer
	ctl := control.New(control.Options{
		Core: r.core, Commands: r.fleet, Config: r.cfg,
		Log: slog.New(slog.NewTextHandler(&logged, nil)),
	})
	t.Cleanup(ctl.Close)

	if _, err := ctl.ChangeConfiguration(context.Background(), cpB, "HeartbeatInterval", "60"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "charge_point="+cpB) {
		t.Errorf("log = %q, want charge_point=%s", logged.String(), cpB)
	}
}
