package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// multiYAML is the coneva bench from #85: two stations, one extra tag, one
// EVSE per station. It is built from baseYAML so the shared parts stay valid.
func multiYAML() string {
	s := baseYAML
	i := strings.Index(s, "charger:")
	j := strings.Index(s, "server:")
	s = s[:i] + `chargers:
  - id: PRUESTEL_000
    ocpp_version: "1.6"
    heartbeat_interval: 60s
    heartbeat_timeout: 90s
    call_timeout: 30s
  - id: PRUESTEL_001
    heartbeat_interval: 30s
` + s[j:]
	s = strings.Replace(s, `  default_id_tag: "04A1B2C3D4"`, "  default_id_tag: RFID_PRUESTEL_000\n  id_tags:\n    - RFID_PRUESTEL_001", 1)
	s = strings.Replace(s, "uid: ALP-HYC-001-1\n", "uid: PRUESTEL_000-1\n      charger: PRUESTEL_000\n", 1)
	s = strings.Replace(s, "uid: ALP-HYC-001-2\n", "uid: PRUESTEL_001-1\n      charger: PRUESTEL_001\n", 1)
	s = strings.Replace(s, "ocpp_connector_id: 2", "ocpp_connector_id: 1", 1)
	return s
}

func parseMulti(t *testing.T, mutate func(string) string) (*config.Config, error) {
	t.Helper()
	return config.Parse(strings.NewReader(mutate(multiYAML())))
}

func TestMultiChargerTargetConfigLoads(t *testing.T) {
	t.Parallel()

	cfg, err := parseMulti(t, func(s string) string { return s })
	if err != nil {
		t.Fatalf("target config from #85 must load: %v", err)
	}
	if len(cfg.Chargers) != 2 {
		t.Fatalf("chargers = %d, want 2", len(cfg.Chargers))
	}
	// Defaults apply per charger: the second one gave only an interval.
	second := cfg.Chargers[1]
	if second.HeartbeatInterval.Duration() != 30*time.Second ||
		second.HeartbeatTimeout.Duration() != config.DefaultHeartbeatTimeout ||
		second.OCPPVersion != config.DefaultOCPPVersion {
		t.Errorf("second charger = %+v, want its own interval and default timeout and version", second)
	}

	if ch, ok := cfg.ChargerByID("PRUESTEL_001"); !ok || ch.ID != "PRUESTEL_001" {
		t.Errorf("ChargerByID(PRUESTEL_001) = %+v, %v", ch, ok)
	}
	if _, ok := cfg.ChargerByID("pruestel_001"); ok {
		t.Error("ChargerByID matched case-insensitively; charge point ids are exact")
	}
	if got := cfg.EVSEsOf("PRUESTEL_001"); len(got) != 1 || got[0].UID != "PRUESTEL_001-1" {
		t.Errorf("EVSEsOf(PRUESTEL_001) = %+v", got)
	}
	if got := cfg.EVSEsOf("nobody"); len(got) != 0 {
		t.Errorf("EVSEsOf(nobody) = %+v, want none", got)
	}
	// Same connector id on both chargers must resolve per charger.
	for _, id := range []string{"PRUESTEL_000", "PRUESTEL_001"} {
		e, ok := cfg.EVSEByConnector(id, 1)
		if !ok || e.UID != id+"-1" {
			t.Errorf("EVSEByConnector(%s, 1) = %+v, %v", id, e, ok)
		}
	}
	if _, ok := cfg.EVSEByConnector("PRUESTEL_000", 2); ok {
		t.Error("EVSEByConnector found an unmapped connector")
	}
	if e, ok := cfg.EVSEByUID("PRUESTEL_001-1"); !ok || e.Charger != "PRUESTEL_001" {
		t.Errorf("EVSEByUID = %+v, %v", e, ok)
	}

	for tag, want := range map[string]bool{
		"RFID_PRUESTEL_000": true, "RFID_PRUESTEL_001": true,
		"rfid_pruestel_001": true, "RFID_PRUESTEL_002": false, "": false,
	} {
		if got := cfg.AcceptsIDTag(tag); got != want {
			t.Errorf("AcceptsIDTag(%q) = %v, want %v", tag, got, want)
		}
	}
	if got := strings.Join(cfg.AcceptedIDTags(), ","); got != "RFID_PRUESTEL_000,RFID_PRUESTEL_001" {
		t.Errorf("AcceptedIDTags = %s", got)
	}
}

func TestSingularChargerIsNormalisedIntoChargers(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(strings.NewReader(baseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Chargers) != 1 || cfg.Chargers[0].ID != "ALP-HYC-001" {
		t.Fatalf("chargers = %+v, want the one from charger:", cfg.Chargers)
	}
	if cfg.LegacyCharger != nil {
		t.Error("the singular field must be cleared after loading")
	}
	for _, e := range cfg.Location.EVSEs {
		if e.Charger != "ALP-HYC-001" {
			t.Errorf("EVSE %s: charger = %q, want the only charger's id", e.UID, e.Charger)
		}
	}
	if len(cfg.EVSEsOf("ALP-HYC-001")) != 2 {
		t.Error("EVSEsOf did not return both EVSEs")
	}
}

func TestMultiChargerValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(string) string
		want   string // exactly one problem, at this path
	}{{
		name:   "second charger heartbeat_timeout not above its interval",
		mutate: rep("heartbeat_interval: 30s", "heartbeat_interval: 30s\n    heartbeat_timeout: 20s", 1),
		want:   "chargers[1].heartbeat_timeout",
	}, {
		name:   "second charger ocpp_version",
		mutate: rep("  - id: PRUESTEL_001\n", "  - id: PRUESTEL_001\n    ocpp_version: \"9.9\"\n", 1),
		want:   "chargers[1].ocpp_version",
	}, {
		name:   "second charger call_timeout",
		mutate: rep("  - id: PRUESTEL_001\n", "  - id: PRUESTEL_001\n    call_timeout: soon\n", 1),
		want:   "chargers[1].call_timeout",
	}, {
		name: "duplicate charger id",
		mutate: func(s string) string {
			s = strings.Replace(s, "  - id: PRUESTEL_001\n", "  - id: PRUESTEL_000\n", 1)
			// Keep the EVSE valid so only the duplicate id is reported; its
			// connector id then differs from the first EVSE's.
			s = strings.Replace(s, "charger: PRUESTEL_001", "charger: PRUESTEL_000", 1)
			k := strings.LastIndex(s, "ocpp_connector_id: 1")
			return s[:k] + "ocpp_connector_id: 2" + s[k+len("ocpp_connector_id: 1"):]
		},
		want: "chargers[1].id",
	}, {
		name:   "evse without charger",
		mutate: rep("      charger: PRUESTEL_001\n", "", 1),
		want:   "location.evses[1].charger",
	}, {
		name:   "evse names an unknown charger",
		mutate: rep("charger: PRUESTEL_001", "charger: PRUESTEL_009", 1),
		want:   "location.evses[1].charger",
	}, {
		name: "same connector id twice on one charger",
		mutate: func(s string) string {
			return strings.Replace(s, "charger: PRUESTEL_001", "charger: PRUESTEL_000", 1)
		},
		want: "location.evses[1].ocpp_connector_id",
	}, {
		name:   "id tag duplicates the default case-insensitively",
		mutate: rep("    - RFID_PRUESTEL_001", "    - rfid_pruestel_000", 1),
		want:   "auth.id_tags[0]",
	}, {
		name:   "id tag duplicates another id tag",
		mutate: rep("    - RFID_PRUESTEL_001", "    - RFID_PRUESTEL_001\n    - Rfid_Pruestel_001", 1),
		want:   "auth.id_tags[1]",
	}, {
		name:   "id tag is not a valid idTag",
		mutate: rep("    - RFID_PRUESTEL_001", `    - "has space"`, 1),
		want:   "auth.id_tags[0]",
	}, {
		name:   "id tag longer than 20 characters",
		mutate: rep("    - RFID_PRUESTEL_001", "    - 123456789012345678901", 1),
		want:   "auth.id_tags[0]",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseMulti(t, tt.mutate)
			var verrs config.ValidationErrors
			if !errors.As(err, &verrs) {
				t.Fatalf("expected ValidationErrors, got %v", err)
			}
			if got := verrs.Paths(); len(got) != 1 || got[0] != tt.want {
				t.Errorf("problems at %v, want exactly [%s]\n%v", got, tt.want, err)
			}
		})
	}
}

func TestSingularAndPluralChargersTogetherAreRejected(t *testing.T) {
	t.Parallel()

	s := strings.Replace(multiYAML(), "chargers:", "charger:\n  id: ALP-HYC-001\nchargers:", 1)
	_, err := config.Parse(strings.NewReader(s))
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs.Paths()) != 1 || verrs.Paths()[0] != "chargers" {
		t.Fatalf("err = %v, want one problem at chargers", err)
	}
	if !strings.Contains(err.Error(), "charger") {
		t.Errorf("message does not name the fields: %v", err)
	}
}

func TestNeitherChargerNorChargersIsRejected(t *testing.T) {
	t.Parallel()

	s := baseYAML
	s = s[:strings.Index(s, "charger:")] + s[strings.Index(s, "server:"):]
	_, err := config.Parse(strings.NewReader(s))
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs.Paths()) != 1 || verrs.Paths()[0] != "chargers" {
		t.Fatalf("err = %v, want one problem at chargers", err)
	}
}

func TestSingularChargerErrorsKeepTheirPaths(t *testing.T) {
	t.Parallel()

	// The existing messages must not change for the single-charger form.
	s := strings.Replace(baseYAML, "heartbeat_timeout: 90s", "heartbeat_timeout: 10s", 1)
	_, err := config.Parse(strings.NewReader(s))
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs.Paths()) != 1 || verrs.Paths()[0] != "charger.heartbeat_timeout" {
		t.Fatalf("err = %v, want charger.heartbeat_timeout", err)
	}
	if !strings.Contains(err.Error(), "charger.heartbeat_interval") {
		t.Errorf("message = %v, want it to name charger.heartbeat_interval", err)
	}
}

func TestSingleChargerEVSENamingAnotherChargerIsRejected(t *testing.T) {
	t.Parallel()

	s := strings.Replace(baseYAML, "uid: ALP-HYC-001-1\n", "uid: ALP-HYC-001-1\n      charger: OTHER\n", 1)
	_, err := config.Parse(strings.NewReader(s))
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs.Paths()) != 1 || verrs.Paths()[0] != "location.evses[0].charger" {
		t.Fatalf("err = %v, want location.evses[0].charger", err)
	}
}
