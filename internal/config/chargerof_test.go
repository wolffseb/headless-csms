package config_test

import (
	"testing"

	"github.com/wolffseb/cli-cpms/internal/config"
)

func TestChargerOfAppliesTheDefault(t *testing.T) {
	t.Parallel()

	one := &config.Config{Chargers: []config.Charger{{ID: "A"}}}
	two := &config.Config{Chargers: []config.Charger{{ID: "A"}, {ID: "B"}}}
	tests := []struct {
		name string
		cfg  *config.Config
		evse config.EVSE
		want string
	}{
		{"empty with one charger is that charger", one, config.EVSE{}, "A"},
		{"named stays as named, with one charger", one, config.EVSE{Charger: "A"}, "A"},
		{"named stays as named, with two", two, config.EVSE{Charger: "B"}, "B"},
		{"empty with two chargers stays empty", two, config.EVSE{}, ""},
	}
	for _, tt := range tests {
		if got := tt.cfg.ChargerOf(tt.evse); got != tt.want {
			t.Errorf("%s: ChargerOf = %q, want %q", tt.name, got, tt.want)
		}
	}
}
