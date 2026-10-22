package locations_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi/locations"
)

var update = flag.Bool("update", false, "rewrite golden files")

// loadConfig loads a config file with its ModTime pinned, so last_updated
// values do not depend on when the file was checked out.
func loadConfig(t *testing.T, path string, modTime time.Time) *config.Config {
	t.Helper()

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ModTime = modTime
	return cfg
}

// fixedSnapshot puts the first EVSE CHARGING since after the config changed,
// and every other one AVAILABLE since before it, so both branches of the EVSE
// last_updated rule show in the output. The fraction checks that it is
// dropped from the wire format.
func fixedSnapshot(cfg *config.Config, staticAt time.Time) core.Snapshot {
	cp := core.ChargePointSnapshot{ID: cfg.Charger.ID, Online: true}
	for i, e := range cfg.Location.EVSEs {
		s := core.EVSESnapshot{UID: e.UID, ConnectorID: e.OCPPConnectorID,
			Status: core.StatusAvailable, LastUpdated: staticAt.Add(-time.Hour)}
		if i == 0 {
			s.Status = core.StatusCharging
			s.LastUpdated = staticAt.Add(90*time.Minute + 500*time.Millisecond)
		}
		cp.EVSEs = append(cp.EVSEs, s)
	}
	return core.Snapshot{ChargePoints: []core.ChargePointSnapshot{cp}}
}

var staticAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

// TestGoldenLocationForTheExampleConfig is the serialisation everything else
// reuses: the full Location for config.example.yaml.
func TestGoldenLocationForTheExampleConfig(t *testing.T) {
	t.Parallel()

	cfg := loadConfig(t, filepath.Join("..", "..", "..", "config.example.yaml"), staticAt)
	loc := locations.Build(cfg, staticAt, fixedSnapshot(cfg, staticAt))
	got, err := json.MarshalIndent(loc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "location.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Location JSON differs from %s (rerun with -update if intended):\n%s", golden, got)
	}

	checkWireFormat(t, got)
}

// The fields OCPI 2.3.0 marks required (cardinality 1 or +) on each object,
// from mod_locations.asciidoc.
var (
	requiredLocation  = []string{"country_code", "party_id", "id", "publish", "address", "city", "country", "coordinates", "time_zone", "last_updated"}
	requiredEVSE      = []string{"uid", "status", "connectors", "last_updated"}
	requiredConnector = []string{"id", "standard", "format", "power_type", "max_voltage", "max_amperage", "last_updated"}
)

// checkWireFormat walks a Location's JSON: every required field is present,
// and every last_updated is a whole-second UTC time ending in "Z".
func checkWireFormat(t *testing.T, raw []byte) {
	t.Helper()

	var loc map[string]any
	if err := json.Unmarshal(raw, &loc); err != nil {
		t.Fatal(err)
	}
	requireFields(t, "location", loc, requiredLocation)
	if c, ok := loc["coordinates"].(map[string]any); ok {
		requireFields(t, "coordinates", c, []string{"latitude", "longitude"})
	}
	evses, _ := loc["evses"].([]any)
	if len(evses) == 0 {
		t.Fatal("no evses")
	}
	for _, e := range evses {
		evse := e.(map[string]any)
		requireFields(t, "evse "+str(evse["uid"]), evse, requiredEVSE)
		conns, _ := evse["connectors"].([]any)
		if len(conns) == 0 {
			t.Errorf("evse %s has no connectors; OCPI requires at least one", str(evse["uid"]))
		}
		for _, c := range conns {
			requireFields(t, "connector", c.(map[string]any), requiredConnector)
		}
	}
	walkTimestamps(t, "", loc)
}

func requireFields(t *testing.T, what string, obj map[string]any, fields []string) {
	t.Helper()
	for _, f := range fields {
		if _, ok := obj[f]; !ok {
			t.Errorf("%s: required field %q missing", what, f)
		}
	}
}

func walkTimestamps(t *testing.T, path string, v any) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "last_updated" {
				s := str(child)
				if _, err := time.Parse("2006-01-02T15:04:05Z", s); err != nil || !strings.HasSuffix(s, "Z") {
					t.Errorf("%s.last_updated = %q, want RFC 3339 UTC ending in Z", path, s)
				}
			}
			walkTimestamps(t, path+"."+k, child)
		}
	case []any:
		for _, child := range v {
			walkTimestamps(t, path+"[]", child)
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func TestLastUpdatedRules(t *testing.T) {
	t.Parallel()

	cfg := loadConfig(t, filepath.Join("testdata", "config.yaml"), staticAt)
	snap := fixedSnapshot(cfg, staticAt)
	loc := locations.Build(cfg, staticAt, snap)

	changed := snap.ChargePoints[0].EVSEs[0].LastUpdated
	// The first EVSE changed after config: its status change wins.
	if got := loc.EVSEs[0].LastUpdated.Time(); !got.Equal(changed) {
		t.Errorf("EVSE 0 last_updated = %v, want its status change %v", got, changed)
	}
	// The second last changed before config was written: config wins.
	if got := loc.EVSEs[1].LastUpdated.Time(); !got.Equal(staticAt) {
		t.Errorf("EVSE 1 last_updated = %v, want config's %v", got, staticAt)
	}
	for _, e := range loc.EVSEs {
		for _, c := range e.Connectors {
			if !c.LastUpdated.Time().Equal(staticAt) {
				t.Errorf("connector %s/%s last_updated = %v, want config's %v", e.UID, c.ID, c.LastUpdated.Time(), staticAt)
			}
		}
	}
	// The Location is the latest of everything.
	if got := loc.LastUpdated.Time(); !got.Equal(changed) {
		t.Errorf("location last_updated = %v, want %v", got, changed)
	}
}

func TestBuildPassesStaticDataThrough(t *testing.T) {
	t.Parallel()

	cfg := loadConfig(t, filepath.Join("testdata", "config.yaml"), staticAt)
	loc := locations.Build(cfg, staticAt, fixedSnapshot(cfg, staticAt))

	// Coordinates keep their digits: "52.0907400" is not "52.09074".
	if loc.Coordinates.Latitude != cfg.Location.Coordinates.Latitude ||
		loc.Coordinates.Longitude != cfg.Location.Coordinates.Longitude {
		t.Errorf("coordinates = %+v, want %+v", loc.Coordinates, cfg.Location.Coordinates)
	}
	if loc.TimeZone != cfg.Location.TimeZone || loc.Publish != cfg.Location.IsPublished() {
		t.Errorf("time_zone, publish = %q, %v; want %q, %v", loc.TimeZone, loc.Publish, cfg.Location.TimeZone, cfg.Location.IsPublished())
	}
	if loc.CountryCode != cfg.OCPI.CountryCode || loc.PartyID != cfg.OCPI.PartyID {
		t.Errorf("party = %s*%s, want %s*%s", loc.CountryCode, loc.PartyID, cfg.OCPI.CountryCode, cfg.OCPI.PartyID)
	}

	// RESERVABLE follows config; the rest are always there.
	for i, e := range loc.EVSEs {
		want := []locations.Capability{locations.CapabilityRemoteStartStop, locations.CapabilityUnlock}
		if cfg.Location.EVSEs[i].IsReservable() {
			want = append(want, locations.CapabilityReservable)
		}
		if !equal(e.Capabilities, want) {
			t.Errorf("%s capabilities = %v, want %v", e.UID, e.Capabilities, want)
		}
	}
}

func equal(a, b []locations.Capability) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReservationsDoNotOverrideTheStationsStatus guards the decision in the
// package comment: a reservation in any state, cancelling included, leaves
// the status the station reported.
func TestReservationsDoNotOverrideTheStationsStatus(t *testing.T) {
	t.Parallel()

	cfg := loadConfig(t, filepath.Join("testdata", "config.yaml"), staticAt)
	snap := fixedSnapshot(cfg, staticAt)
	cp := &snap.ChargePoints[0]
	for i, st := range []core.ReservationStatus{core.ReservationPending, core.ReservationCancelling} {
		cp.Reservations = append(cp.Reservations, core.Reservation{
			ID: i + 1, ChargePointID: cp.ID, EVSEUID: cp.EVSEs[i].UID, Status: st,
		})
	}

	loc := locations.Build(cfg, staticAt, snap)
	for i, e := range loc.EVSEs {
		if want := cp.EVSEs[i].Status; e.Status != want {
			t.Errorf("%s with a %s reservation = %s, want the reported %s", e.UID, cp.Reservations[i].Status, e.Status, want)
		}
	}
}

func TestEVSEMissingFromTheSnapshotIsUnknown(t *testing.T) {
	t.Parallel()

	cfg := loadConfig(t, filepath.Join("testdata", "config.yaml"), staticAt)
	loc := locations.Build(cfg, staticAt, core.Snapshot{})
	for _, e := range loc.EVSEs {
		if e.Status != core.StatusUnknown || !e.LastUpdated.Time().Equal(staticAt) {
			t.Errorf("%s = %s at %v, want UNKNOWN at %v", e.UID, e.Status, e.LastUpdated.Time(), staticAt)
		}
	}
}
