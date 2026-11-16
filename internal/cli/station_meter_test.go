package cli_test

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

// Issue #64 uses ports 19670–19679.

// TestStationMeterShowsOnTheChargePointLine: a station with clock-aligned
// data on sends MeterValues for connector 0, the station as a whole. That
// reading belongs on the charge-point line of status, not in an EVSE row of
// its own such as ALP-HYC-001-0.
func TestStationMeterShowsOnTheChargePointLine(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19670)

	client := ocpptest.MustDial(t, "ws://127.0.0.1:"+strconv.Itoa(s.port)+"/ocpp/ALP-HYC-001")
	t.Cleanup(client.Close)
	client.MustCall("BootNotification", map[string]string{
		"chargePointVendor": "Alpitronic", "chargePointModel": "HYC50",
	})
	for _, c := range []int{0, 1} {
		client.MustCall("StatusNotification", map[string]any{
			"connectorId": c, "errorCode": "NoError", "status": "Available",
		})
	}
	client.MustCall("MeterValues", map[string]any{
		"connectorId": 0,
		"meterValue": []map[string]any{{
			"timestamp": "2026-11-16T12:00:00Z",
			"sampledValue": []map[string]string{
				{"value": "50000", "measurand": "Power.Offered", "unit": "W", "context": "Sample.Clock"},
				{"value": "125", "measurand": "Current.Offered", "unit": "A", "context": "Sample.Clock"},
			},
		}},
	})

	raw := s.mustCPMS("status", "--json")
	var st struct {
		ChargePoint struct {
			Meter map[string]any `json:"meter"`
		} `json:"charge_point"`
		EVSEs []struct {
			UID string `json:"uid"`
		} `json:"evses"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("decoding status --json: %v", err)
	}
	if len(st.EVSEs) != 1 || st.EVSEs[0].UID != "EVSE-1" {
		t.Errorf("status --json EVSEs = %+v, want only the configured EVSE-1:\n%s", st.EVSEs, raw)
	}
	if m := st.ChargePoint.Meter; m["power_offered_w"] != 50000.0 || m["current_offered_a"] != 125.0 || m["reading_at"] == nil {
		t.Errorf("charge_point.meter = %v, want 50 kW and 125 A offered with a time:\n%s", m, raw)
	}

	human := s.mustCPMS("status")
	if strings.Contains(human, "ALP-HYC-001-0") {
		t.Errorf("status shows a connector-0 EVSE row:\n%s", human)
	}
	first, _, _ := strings.Cut(human, "\n")
	if !regexp.MustCompile(`meter offered 50\.0 kW 125\.0 A, \d+\S* ago`).MatchString(first) {
		t.Errorf("charge-point line = %q, want the station meter on it", first)
	}
}
