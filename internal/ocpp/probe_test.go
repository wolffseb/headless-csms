package ocpp_test

import (
	"testing"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

func TestReportSupport(t *testing.T) {
	t.Parallel()

	feature := func(s ocpp.Support, grants ...ocpp.Capability) ocpp.Feature {
		return ocpp.Feature{Name: "F", Support: s, Grants: grants}
	}
	tests := []struct {
		name     string
		features []ocpp.Feature
		want     ocpp.Support
	}{
		{"no feature grants it", []ocpp.Feature{feature(ocpp.SupportYes, ocpp.CapUnlock)}, ocpp.SupportUnknown},
		{"granted", []ocpp.Feature{feature(ocpp.SupportYes, ocpp.CapReservation)}, ocpp.SupportYes},
		{"absent", []ocpp.Feature{feature(ocpp.SupportNo, ocpp.CapReservation)}, ocpp.SupportNo},
		{"not reported", []ocpp.Feature{feature(ocpp.SupportUnknown, ocpp.CapReservation)}, ocpp.SupportUnknown},
		{"one of two present", []ocpp.Feature{
			feature(ocpp.SupportNo, ocpp.CapReservation), feature(ocpp.SupportYes, ocpp.CapReservation),
		}, ocpp.SupportYes},
		// One absent and one unknown is not proof of absence.
		{"absent and unknown", []ocpp.Feature{
			feature(ocpp.SupportNo, ocpp.CapReservation), feature(ocpp.SupportUnknown, ocpp.CapReservation),
		}, ocpp.SupportUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := ocpp.ProbeReport{Features: tt.features}
			if got := r.Support(ocpp.CapReservation); got != tt.want {
				t.Errorf("Support = %s, want %s", got, tt.want)
			}
		})
	}
}
