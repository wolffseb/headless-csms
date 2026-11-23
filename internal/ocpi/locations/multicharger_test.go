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

// A charger that has never connected, or has disconnected, gives its EVSEs
// the offline status while the other charger's stay live. This runs core's
// own rule rather than a hand-built snapshot.
func TestBuildOfflineChargerDoesNotAffectTheOther(t *testing.T) {
	t.Parallel()
	cfg, c1, c2 := twoChargerConfig(t)
	svc := core.New(cfg)
	t.Cleanup(svc.Close)
	svc.Connected(c1)
	svc.SetConnectorStatus(c1, cfg.Location.EVSEs[0].OCPPConnectorID, core.StatusCharging, "NoError")

	check := func(step string) {
		t.Helper()
		loc := locations.Build(cfg, staticAt, svc.Snapshot())
		if got := loc.EVSEs[0].Status; got != core.StatusCharging {
			t.Errorf("%s: online charger's EVSE = %s, want CHARGING", step, got)
		}
		for _, e := range loc.EVSEs[1:] {
			if e.Status != core.StatusUnknown {
				t.Errorf("%s: offline charger's EVSE %s = %s, want UNKNOWN", step, e.UID, e.Status)
			}
		}
	}
	check("never connected")

	svc.Connected(c2)
	for _, e := range cfg.EVSEsOf(c2) {
		svc.SetConnectorStatus(c2, e.OCPPConnectorID, core.StatusAvailable, "NoError")
	}
	loc := locations.Build(cfg, staticAt, svc.Snapshot())
	if got := loc.EVSEs[1].Status; got != core.StatusAvailable {
		t.Fatalf("connected charger's EVSE = %s, want AVAILABLE", got)
	}
	svc.Disconnected(c2, "test")
	check("disconnected")
}

// A synthetic uid core makes for an unconfigured connector ("<charger>-<n>")
// may equal a configured uid on another charger. It must not overwrite that
// EVSE's status, whatever the order of cfg.Chargers.
func TestBuildSyntheticUIDOfOneChargerDoesNotHitAnother(t *testing.T) {
	t.Parallel()
	for _, reversed := range []bool{false, true} {
		cfg, c1, c2 := twoChargerConfig(t)
		cfg.Location.EVSEs[0].UID = c2 + "-9"
		if reversed {
			cfg.Chargers[0], cfg.Chargers[1] = cfg.Chargers[1], cfg.Chargers[0]
		}
		svc := core.New(cfg)
		t.Cleanup(svc.Close)
		svc.Connected(c1)
		svc.Connected(c2)
		svc.SetConnectorStatus(c1, cfg.Location.EVSEs[0].OCPPConnectorID, core.StatusAvailable, "NoError")
		svc.SetConnectorStatus(c2, 9, core.StatusOutOfOrder, "InternalError")
		loc := locations.Build(cfg, staticAt, svc.Snapshot())
		if got := loc.EVSEs[0].Status; got != core.StatusAvailable {
			t.Errorf("reversed=%v: EVSE %s (charger %s) = %s, want AVAILABLE", reversed, loc.EVSEs[0].UID, c1, got)
		}
	}
}
