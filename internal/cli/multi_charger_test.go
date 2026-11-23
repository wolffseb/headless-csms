package cli_test

import (
	"strings"
	"testing"
)

const multiChargerYAML = `
chargers:
  - id: PRUESTEL_000
  - id: PRUESTEL_001
    heartbeat_timeout: 2m
server:
  ocpp_bind: 0.0.0.0:9000
  ocpi_bind: 0.0.0.0:8080
auth:
  default_id_tag: RFID_PRUESTEL_000
  id_tags: [RFID_PRUESTEL_001]
ocpi:
  country_code: DE
  party_id: EXA
  public_base_url: http://192.168.1.10:8080/ocpi
  token_a: "token-a-secret"
  token_c: "token-c-secret"
location:
  id: SITE-01
  name: Example Site
  address: Musterstrasse 1
  city: Berlin
  postal_code: "10115"
  country: DEU
  coordinates: {latitude: "52.520008", longitude: "13.404954"}
  time_zone: Europe/Berlin
  evses:
    - uid: PRUESTEL_000-1
      charger: PRUESTEL_000
      evse_id: DE*EXA*E001*1
      ocpp_connector_id: 1
      connectors:
        - {id: "1", standard: IEC_62196_T2, format: SOCKET, power_type: AC_3_PHASE, max_voltage: 400, max_amperage: 32}
    - uid: PRUESTEL_001-1
      charger: PRUESTEL_001
      evse_id: DE*EXA*E002*1
      ocpp_connector_id: 1
      connectors:
        - {id: "1", standard: IEC_62196_T2, format: SOCKET, power_type: AC_3_PHASE, max_voltage: 400, max_amperage: 32}
`

func TestConfigValidateSummarisesEveryCharger(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := dir + "/multi.yaml"
	writeFile(t, path, multiChargerYAML)

	stdout, stderr, err := run(t, "config", "validate", "-c", path)
	if err != nil {
		t.Fatalf("expected success, got %v (stderr: %s)", err, stderr)
	}
	for _, want := range []string{
		"ws://<this-host>:9000/ocpp/PRUESTEL_000",
		"ws://<this-host>:9000/ocpp/PRUESTEL_001",
		"RFID_PRUESTEL_000, RFID_PRUESTEL_001",
		"CHARGER",
		"1m30s",
		"Heartbeat timeout  2m",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary does not mention %q\n%s", want, stdout)
		}
	}
}
