package cli

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
)

func TestChargerPolicyIsPerCharger(t *testing.T) {
	cfg := &config.Config{Chargers: []config.Charger{
		{
			ID:               "PRUESTEL_000",
			HeartbeatTimeout: config.NewDuration(90 * time.Second),
			CallTimeout:      config.NewDuration(10 * time.Second),
		},
		{
			ID:               "PRUESTEL_001",
			HeartbeatTimeout: config.NewDuration(240 * time.Second),
			CallTimeout:      config.NewDuration(25 * time.Second),
		},
	}}
	p := chargerPolicy{cfg}

	if !p.accept("PRUESTEL_000") || !p.accept("PRUESTEL_001") {
		t.Error("a configured charger was rejected")
	}
	if p.accept("nobody") {
		t.Error("an unknown id was accepted")
	}

	for _, tc := range []struct {
		id         string
		call, idle time.Duration
	}{
		{"PRUESTEL_000", 10 * time.Second, 90 * time.Second},
		{"PRUESTEL_001", 25 * time.Second, 240 * time.Second},
		{"nobody", 0, 0}, // 0 lets the server fall back to its default
	} {
		if got := p.callTimeout(tc.id); got != tc.call {
			t.Errorf("callTimeout(%s) = %v, want %v", tc.id, got, tc.call)
		}
		if got := p.idleTimeout(tc.id); got != tc.idle {
			t.Errorf("idleTimeout(%s) = %v, want %v", tc.id, got, tc.idle)
		}
	}
}
