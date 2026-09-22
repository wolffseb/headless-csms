package config_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// baseYAML is a valid config. Each invalid-case test below mutates exactly one
// thing about it, so a failure points at the rule under test rather than at
// whatever else happened to be wrong in a hand-written fixture.
const baseYAML = `
charger:
  id: ALP-HYC-001
  ip: 192.168.1.42
  ocpp_version: "1.6"
  heartbeat_interval: 60s
  heartbeat_timeout: 90s
  call_timeout: 30s
server:
  ocpp_bind: 0.0.0.0:9000
  ocpi_bind: 0.0.0.0:8080
auth:
  default_id_tag: "04A1B2C3D4"
ocpi:
  country_code: DE
  party_id: FRY
  public_base_url: http://192.168.1.10:8080/ocpi
  token_a: "token-a-secret"
  token_c: "token-c-secret"
  push:
    debounce: 500ms
    max_retries: 5
location:
  id: OFFICE-01
  name: Fryte HQ
  address: Musterstrasse 1
  city: Berlin
  postal_code: "10115"
  country: DEU
  coordinates:
    latitude: "52.520008"
    longitude: "13.404954"
  evses:
    - uid: ALP-HYC-001-1
      evse_id: DE*FRY*E001*1
      ocpp_connector_id: 1
      connectors:
        - id: "1"
          standard: IEC_62196_T2_COMBO
          format: CABLE
          power_type: DC
          max_voltage: 920
          max_amperage: 500
          max_electric_power: 400000
    - uid: ALP-HYC-001-2
      evse_id: DE*FRY*E001*2
      ocpp_connector_id: 2
      connectors:
        - id: "1"
          standard: IEC_62196_T2_COMBO
          format: CABLE
          power_type: DC
          max_voltage: 920
          max_amperage: 500
          max_electric_power: 400000
`

// rep replaces the first n occurrences of old.
func rep(old, replacement string, n int) func(string) string {
	return func(s string) string { return strings.Replace(s, old, replacement, n) }
}

func TestBaseYAMLIsValid(t *testing.T) {
	t.Parallel()
	if _, err := config.Parse(strings.NewReader(baseYAML)); err != nil {
		t.Fatalf("base fixture must be valid, got: %v", err)
	}
}

func TestInvalidConfigsNameTheOffendingField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(string) string
		wantPaths []string
	}{{
		name:      "missing charger id",
		mutate:    rep("  id: ALP-HYC-001\n", "", 1),
		wantPaths: []string{"charger.id"},
	}, {
		name:      "charger id is not a safe url segment",
		mutate:    rep("id: ALP-HYC-001", `id: "ALP/HYC 001"`, 1),
		wantPaths: []string{"charger.id"},
	}, {
		name:      "charger ip is neither address nor hostname",
		mutate:    rep("ip: 192.168.1.42", `ip: "192.168.1.42:9000"`, 1),
		wantPaths: []string{"charger.ip"},
	}, {
		name:      "unknown field in charger",
		mutate:    rep("heartbeat_timeout: 90s", "hearbeat_timeout: 90s", 1),
		wantPaths: []string{"charger.hearbeat_timeout"},
	}, {
		name:      "unknown field in a connector",
		mutate:    rep("max_amperage: 500", "max_amps: 500", 1),
		wantPaths: []string{"location.evses[].connectors[].max_amps"},
	}, {
		name:      "duration without a unit",
		mutate:    rep("heartbeat_timeout: 90s", "heartbeat_timeout: 90", 1),
		wantPaths: []string{"charger.heartbeat_timeout"},
	}, {
		name:      "zero heartbeat timeout",
		mutate:    rep("heartbeat_timeout: 90s", "heartbeat_timeout: 0s", 1),
		wantPaths: []string{"charger.heartbeat_timeout"},
	}, {
		name:      "heartbeat timeout below the interval",
		mutate:    rep("heartbeat_timeout: 90s", "heartbeat_timeout: 30s", 1),
		wantPaths: []string{"charger.heartbeat_timeout"},
	}, {
		name:      "heartbeat timeout equal to the interval",
		mutate:    rep("heartbeat_timeout: 90s", "heartbeat_timeout: 60s", 1),
		wantPaths: []string{"charger.heartbeat_timeout"},
	}, {
		name:      "zero call timeout",
		mutate:    rep("call_timeout: 30s", "call_timeout: 0s", 1),
		wantPaths: []string{"charger.call_timeout"},
	}, {
		name:      "unsupported ocpp version",
		mutate:    rep(`ocpp_version: "1.6"`, `ocpp_version: "1.5"`, 1),
		wantPaths: []string{"charger.ocpp_version"},
	}, {
		name:      "listeners share a port",
		mutate:    rep("ocpi_bind: 0.0.0.0:8080", "ocpi_bind: 127.0.0.1:9000", 1),
		wantPaths: []string{"server.ocpi_bind"},
	}, {
		name:      "bind without a port",
		mutate:    rep("ocpp_bind: 0.0.0.0:9000", "ocpp_bind: 0.0.0.0", 1),
		wantPaths: []string{"server.ocpp_bind"},
	}, {
		name: "bind on port zero",
		// Port 0 picks a random port, which no charger can be pointed at.
		mutate:    rep("ocpp_bind: 0.0.0.0:9000", "ocpp_bind: 0.0.0.0:0", 1),
		wantPaths: []string{"server.ocpp_bind"},
	}, {
		name:      "id tag longer than 20 characters",
		mutate:    rep(`default_id_tag: "04A1B2C3D4"`, `default_id_tag: "012345678901234567890"`, 1),
		wantPaths: []string{"auth.default_id_tag"},
	}, {
		name:      "id tag containing a space",
		mutate:    rep(`default_id_tag: "04A1B2C3D4"`, `default_id_tag: "04A1 B2C3"`, 1),
		wantPaths: []string{"auth.default_id_tag"},
	}, {
		name:      "lowercase country code",
		mutate:    rep("country_code: DE", "country_code: de", 1),
		wantPaths: []string{"ocpi.country_code"},
	}, {
		name:      "party id of the wrong length",
		mutate:    rep("party_id: FRY", "party_id: FRYT", 1),
		wantPaths: []string{"ocpi.party_id"},
	}, {
		name:      "public base url with a non-http scheme",
		mutate:    rep("public_base_url: http://192.168.1.10:8080/ocpi", "public_base_url: ftp://192.168.1.10/ocpi", 1),
		wantPaths: []string{"ocpi.public_base_url"},
	}, {
		name:      "token a and token c are identical",
		mutate:    rep(`token_c: "token-c-secret"`, `token_c: "token-a-secret"`, 1),
		wantPaths: []string{"ocpi.token_c"},
	}, {
		name:      "token too short",
		mutate:    rep(`token_a: "token-a-secret"`, `token_a: "abc"`, 1),
		wantPaths: []string{"ocpi.token_a"},
	}, {
		name:      "negative push retries",
		mutate:    rep("max_retries: 5", "max_retries: -1", 1),
		wantPaths: []string{"ocpi.push.max_retries"},
	}, {
		name:      "latitude with too few decimals",
		mutate:    rep(`latitude: "52.520008"`, `latitude: "52.5"`, 1),
		wantPaths: []string{"location.coordinates.latitude"},
	}, {
		name:      "latitude out of range",
		mutate:    rep(`latitude: "52.520008"`, `latitude: "95.520008"`, 1),
		wantPaths: []string{"location.coordinates.latitude"},
	}, {
		name:      "longitude missing",
		mutate:    rep(`    longitude: "13.404954"`, "", 1),
		wantPaths: []string{"location.coordinates.longitude"},
	}, {
		name:      "location country is not alpha-3",
		mutate:    rep("\n  country: DEU", "\n  country: DE", 1),
		wantPaths: []string{"location.country"},
	}, {
		name:      "duplicate ocpp connector id",
		mutate:    rep("ocpp_connector_id: 2", "ocpp_connector_id: 1", 1),
		wantPaths: []string{"location.evses[1].ocpp_connector_id"},
	}, {
		name:      "connector id zero addresses the charge point itself",
		mutate:    rep("ocpp_connector_id: 1", "ocpp_connector_id: 0", 1),
		wantPaths: []string{"location.evses[0].ocpp_connector_id"},
	}, {
		name:      "duplicate evse uid",
		mutate:    rep("uid: ALP-HYC-001-2", "uid: ALP-HYC-001-1", 1),
		wantPaths: []string{"location.evses[1].uid"},
	}, {
		name:      "evse id is not eMI3 shaped",
		mutate:    rep("evse_id: DE*FRY*E001*1", "evse_id: 12345", 1),
		wantPaths: []string{"location.evses[0].evse_id"},
	}, {
		name:      "unknown connector standard",
		mutate:    rep("standard: IEC_62196_T2_COMBO", "standard: CCS2", 1),
		wantPaths: []string{"location.evses[0].connectors[0].standard"},
	}, {
		name:      "unknown connector format",
		mutate:    rep("format: CABLE", "format: PLUG", 1),
		wantPaths: []string{"location.evses[0].connectors[0].format"},
	}, {
		name:      "unknown power type",
		mutate:    rep("power_type: DC", "power_type: DC_FAST", 1),
		wantPaths: []string{"location.evses[0].connectors[0].power_type"},
	}, {
		name:      "zero max voltage",
		mutate:    rep("max_voltage: 920", "max_voltage: 0", 1),
		wantPaths: []string{"location.evses[0].connectors[0].max_voltage"},
	}, {
		name: "no evses configured",
		mutate: func(s string) string {
			return s[:strings.Index(s, "  evses:")] + "  evses: []\n"
		},
		wantPaths: []string{"location.evses"},
	}, {
		name: "evse with no connectors",
		mutate: func(s string) string {
			return s[:strings.Index(s, "      connectors:")] + "      connectors: []\n"
		},
		wantPaths: []string{"location.evses[0].connectors"},
	}, {
		name: "several problems are all reported",
		mutate: func(s string) string {
			s = rep("  id: ALP-HYC-001\n", "", 1)(s)
			s = rep("country_code: DE", "country_code: de", 1)(s)
			s = rep("party_id: FRY", "party_id: F", 1)(s)
			return s
		},
		wantPaths: []string{"charger.id", "ocpi.country_code", "ocpi.party_id"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse(strings.NewReader(tt.mutate(baseYAML)))
			if err == nil {
				t.Fatal("expected the config to be rejected, but it validated")
			}

			var verrs config.ValidationErrors
			if !errors.As(err, &verrs) {
				t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
			}
			got := verrs.Paths()
			for _, want := range tt.wantPaths {
				if !contains(got, want) {
					t.Errorf("expected a problem at %q, got %v\nfull error: %v", want, got, err)
				}
			}
			if len(got) != len(tt.wantPaths) {
				t.Errorf("expected exactly %d problem(s) %v, got %d %v",
					len(tt.wantPaths), tt.wantPaths, len(got), got)
			}
		})
	}
}

func TestLoadValidFile(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.Charger.ID, "ALP-HYC-001"; got != want {
		t.Errorf("charger id = %q, want %q", got, want)
	}
	if got, want := cfg.Charger.HeartbeatTimeout.Duration(), 2*time.Minute; got != want {
		t.Errorf("heartbeat timeout = %v, want %v", got, want)
	}
	if got, want := cfg.OCPI.Push.Debounce.Duration(), 250*time.Millisecond; got != want {
		t.Errorf("push debounce = %v, want %v", got, want)
	}
	// The fixture's public_base_url ends in a slash; the loader must strip it
	// so that joined paths do not come out as "//locations".
	if got, want := cfg.OCPI.PublicBaseURL, "http://192.168.1.10:8080/ocpi"; got != want {
		t.Errorf("public base url = %q, want %q", got, want)
	}
	if got, want := len(cfg.Location.EVSEs), 2; got != want {
		t.Errorf("evse count = %d, want %d", got, want)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ocpp_version", cfg.Charger.OCPPVersion, config.DefaultOCPPVersion},
		{"heartbeat_interval", cfg.Charger.HeartbeatInterval.Duration(), config.DefaultHeartbeatInterval},
		{"heartbeat_timeout", cfg.Charger.HeartbeatTimeout.Duration(), config.DefaultHeartbeatTimeout},
		{"call_timeout", cfg.Charger.CallTimeout.Duration(), config.DefaultCallTimeout},
		{"ocpp_bind", cfg.Server.OCPPBind, config.DefaultOCPPBind},
		{"ocpi_bind", cfg.Server.OCPIBind, config.DefaultOCPIBind},
		{"push.debounce", cfg.OCPI.Push.Debounce.Duration(), config.DefaultPushDebounce},
		{"push.max_retries", cfg.OCPI.Push.MaxRetries, config.DefaultPushMaxRetries},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestExampleConfigIsValid keeps the shipped example honest: if a field is
// renamed without updating config.example.yaml, this fails.
func TestExampleConfigIsValid(t *testing.T) {
	t.Parallel()

	if _, err := config.Load(filepath.Join("..", "..", "config.example.yaml")); err != nil {
		t.Fatalf("config.example.yaml must validate, got: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	_, err := config.Load(filepath.Join("testdata", "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	var verrs config.ValidationErrors
	if errors.As(err, &verrs) {
		t.Error("a missing file should surface as an I/O error, not as ValidationErrors")
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "empty"},
		{"syntax error", "charger:\n  id: [unclosed\n", "did not find"},
		{"two documents", baseYAML + "\n---\ncharger:\n  id: OTHER\n", "more than one YAML document"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse(strings.NewReader(tt.input))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.want)
			}
		})
	}
}

// TestTypeMismatchReportsLine covers the one case where no field path is
// recoverable: yaml reports these by line only, and that is still actionable.
func TestTypeMismatchReportsLine(t *testing.T) {
	t.Parallel()

	in := rep("ocpp_connector_id: 1", `ocpp_connector_id: "one"`, 1)(baseYAML)
	_, err := config.Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected an error")
	}
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
	if len(verrs) != 1 || !strings.HasPrefix(verrs[0].Path, "line ") {
		t.Errorf("expected a single line-anchored problem, got %v", verrs.Paths())
	}
}

func TestEVSELookups(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	evse, ok := cfg.EVSEByUID("ALP-HYC-001-2")
	if !ok || evse.OCPPConnectorID != 2 {
		t.Errorf("EVSEByUID(ALP-HYC-001-2) = %+v, %v; want connector 2", evse, ok)
	}
	if _, ok := cfg.EVSEByUID("nope"); ok {
		t.Error("EVSEByUID found an EVSE that is not configured")
	}

	evse, ok = cfg.EVSEByConnectorID(1)
	if !ok || evse.UID != "ALP-HYC-001-1" {
		t.Errorf("EVSEByConnectorID(1) = %+v, %v; want ALP-HYC-001-1", evse, ok)
	}
	if _, ok := cfg.EVSEByConnectorID(99); ok {
		t.Error("EVSEByConnectorID found a connector that is not mapped")
	}
}

func TestValidationErrorsMessage(t *testing.T) {
	t.Parallel()

	errs := config.ValidationErrors{
		{Path: "charger.id", Msg: "is required"},
		{Path: "ocpi.party_id", Msg: "must be three uppercase letters"},
	}
	msg := errs.Error()
	for _, want := range []string{"2 problems", "charger.id: is required", "ocpi.party_id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
