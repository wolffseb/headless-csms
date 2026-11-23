package locations_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi/locations"
)

// twoChargerConfig is the example config with its first EVSE on charger 1 and
// every other EVSE on charger 2.
func twoChargerConfig(t *testing.T) (*config.Config, string, string) {
	t.Helper()
	cfg := loadConfig(t, filepath.Join("..", "..", "..", "config.example.yaml"), staticAt)
	second := cfg.Chargers[0]
	second.ID = "ALP-HYC-002"
	cfg.Chargers = append(cfg.Chargers, second)
	for i := 1; i < len(cfg.Location.EVSEs); i++ {
		cfg.Location.EVSEs[i].Charger = second.ID
	}
	return cfg, cfg.Chargers[0].ID, second.ID
}

func chargerSnap(cfg *config.Config, id string, online bool, st core.EVSEStatus, at time.Time) core.ChargePointSnapshot {
	cp := core.ChargePointSnapshot{ID: id, Online: online}
	for _, e := range cfg.EVSEsOf(id) {
		cp.EVSEs = append(cp.EVSEs, core.EVSESnapshot{UID: e.UID, ConnectorID: e.OCPPConnectorID, Status: st, LastUpdated: at})
	}
	return cp
}

// Every EVSE of the one Location shows the live status of its own charger.
func TestBuildTakesEachEVSEStatusFromItsCharger(t *testing.T) {
	t.Parallel()
	cfg, c1, c2 := twoChargerConfig(t)
	at := staticAt.Add(time.Hour)
	snap := core.Snapshot{ChargePoints: []core.ChargePointSnapshot{
		chargerSnap(cfg, c1, true, core.StatusCharging, at),
		chargerSnap(cfg, c2, true, core.StatusBlocked, at),
	}}
	loc := locations.Build(cfg, staticAt, snap)
	for i, e := range loc.EVSEs {
		want := core.StatusBlocked
		if i == 0 {
			want = core.StatusCharging
		}
		if e.Status != want {
			t.Errorf("EVSE %s status = %s, want %s", e.UID, e.Status, want)
		}
	}
}

// A charger that is offline gives its EVSEs the offline status (core's
// effective status, UNKNOWN here) while the other charger's stay live. A
// charger missing from the snapshot altogether is UNKNOWN too.
func TestBuildOfflineChargerDoesNotAffectTheOther(t *testing.T) {
	t.Parallel()
	cfg, c1, c2 := twoChargerConfig(t)
	at := staticAt.Add(time.Hour)
	snap := core.Snapshot{ChargePoints: []core.ChargePointSnapshot{
		chargerSnap(cfg, c1, true, core.StatusCharging, at),
		chargerSnap(cfg, c2, false, core.StatusUnknown, at),
	}}
	loc := locations.Build(cfg, staticAt, snap)
	if got := loc.EVSEs[0].Status; got != core.StatusCharging {
		t.Errorf("online charger's EVSE = %s, want CHARGING", got)
	}
	for _, e := range loc.EVSEs[1:] {
		if e.Status != core.StatusUnknown {
			t.Errorf("offline charger's EVSE %s = %s, want UNKNOWN", e.UID, e.Status)
		}
	}

	only1 := core.Snapshot{ChargePoints: []core.ChargePointSnapshot{chargerSnap(cfg, c1, true, core.StatusCharging, at)}}
	loc = locations.Build(cfg, staticAt, only1)
	for _, e := range loc.EVSEs[1:] {
		if e.Status != core.StatusUnknown {
			t.Errorf("EVSE %s of an absent charger = %s, want UNKNOWN", e.UID, e.Status)
		}
	}
}
