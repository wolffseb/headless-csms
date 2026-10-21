package v16_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// fullConfiguration is a station that reports every key the probe reads.
const fullConfiguration = `{"configurationKey":[
	{"key":"SupportedFeatureProfiles","readonly":true,"value":"Core, FirmwareManagement,Reservation,RemoteTrigger,SmartCharging"},
	{"key":"NumberOfConnectors","readonly":true,"value":"2"},
	{"key":"HeartbeatInterval","readonly":false,"value":"300"},
	{"key":"ConnectionTimeOut","readonly":false,"value":"45"},
	{"key":"MeterValueSampleInterval","readonly":false,"value":"60"},
	{"key":"AuthorizeRemoteTxRequests","readonly":false,"value":"true"},
	{"key":"LocalAuthListEnabled","readonly":false,"value":"true"},
	{"key":"LocalPreAuthorize","readonly":false,"value":"false"},
	{"key":"LocalAuthorizeOffline","readonly":false,"value":"true"},
	{"key":"ReserveConnectorZeroSupported","readonly":true,"value":"false"},
	{"key":"VendorSpecificThing","readonly":true,"value":"42"}
]}`

func TestProbeReadsTheWholeConfigurationOnce(t *testing.T) {
	t.Parallel()

	cp, fake := newFakeChargePoint(fullConfiguration)
	report, err := cp.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	sent := fake.sent()
	if len(sent) != 1 || sent[0].action != "GetConfiguration" || string(sent[0].payload) != `{}` {
		t.Fatalf("sent %+v, want one GetConfiguration asking for every key", sent)
	}

	if report.Version != ocpp.Version16 {
		t.Errorf("version = %q", report.Version)
	}
	for _, c := range ocpp.AllCapabilities() {
		if got := report.Support(c); got != ocpp.SupportYes {
			t.Errorf("%s = %s, want yes", c, got)
		}
	}

	// The needed profiles first, then the rest in the station's order.
	var names []string
	for _, f := range report.Features {
		names = append(names, f.Name)
	}
	want := []string{"Core", "Reservation", "RemoteTrigger", "FirmwareManagement", "SmartCharging"}
	if !slices.Equal(names, want) {
		t.Errorf("features = %v, want %v", names, want)
	}
	for _, f := range report.Features[3:] {
		if len(f.Grants) != 0 {
			t.Errorf("%s grants %v, but cpms has no use for it", f.Name, f.Grants)
		}
	}

	if report.Connectors == nil || *report.Connectors != 2 {
		t.Errorf("connectors = %v, want 2", report.Connectors)
	}
	if report.HeartbeatInterval == nil || *report.HeartbeatInterval != 5*time.Minute {
		t.Errorf("heartbeat interval = %v, want 5m", report.HeartbeatInterval)
	}

	settings := map[string]ocpp.Setting{}
	for _, s := range report.Settings {
		if !s.Reported || s.Meaning == "" {
			t.Errorf("setting %+v: want reported, with a meaning", s)
		}
		settings[s.Name] = s
	}
	if len(settings) != 9 {
		t.Errorf("%d settings, want 9", len(settings))
	}
	if s := settings["AuthorizeRemoteTxRequests"]; s.Value != "true" {
		t.Errorf("AuthorizeRemoteTxRequests = %q", s.Value)
	}

	if len(report.Raw) != 11 {
		t.Fatalf("%d raw keys, want all 11", len(report.Raw))
	}
	if last := report.Raw[10]; last != (ocpp.RawSetting{Key: "VendorSpecificThing", Value: "42", Readonly: true}) {
		t.Errorf("raw key = %+v", last)
	}
}

func TestProbeWithoutReservationProfile(t *testing.T) {
	t.Parallel()

	cp, _ := newFakeChargePoint(`{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true,"value":"Core,RemoteTrigger"}]}`)
	report, err := cp.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Support(ocpp.CapReservation); got != ocpp.SupportNo {
		t.Errorf("reservation = %s, want no", got)
	}
	if got := report.Support(ocpp.CapRemoteStartStop); got != ocpp.SupportYes {
		t.Errorf("remote start/stop = %s, want yes", got)
	}
}

// Listing profiles without Core is a statement, not an omission: nothing is
// assumed then.
func TestProbeCoreAbsentFromAListIsMissing(t *testing.T) {
	t.Parallel()

	cp, _ := newFakeChargePoint(`{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true,"value":"Reservation"}]}`)
	report, err := cp.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Support(ocpp.CapUnlock); got != ocpp.SupportNo {
		t.Errorf("unlock = %s, want no", got)
	}
}

// A station that leaves keys out has not said no to anything: every missing
// key is "not reported", and what it did report is still read.
func TestProbeMissingKeysAreNotReported(t *testing.T) {
	t.Parallel()

	cp, _ := newFakeChargePoint(`{"configurationKey":[
		{"key":"NumberOfConnectors","readonly":true,"value":"1"},
		{"key":"HeartbeatInterval","readonly":false,"value":"sixty"}
	]}`)
	report, err := cp.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Core is mandatory in 1.6, so it is assumed; the rest is not known.
	for _, c := range ocpp.AllCapabilities() {
		want := ocpp.SupportUnknown
		if c == ocpp.CapRemoteStartStop || c == ocpp.CapUnlock {
			want = ocpp.SupportYes
		}
		if got := report.Support(c); got != want {
			t.Errorf("%s = %s without SupportedFeatureProfiles, want %s", c, got, want)
		}
	}
	for _, f := range report.Features {
		if f.Assumed != (f.Name == "Core") {
			t.Errorf("%s assumed = %v", f.Name, f.Assumed)
		}
	}
	if report.Connectors == nil || *report.Connectors != 1 {
		t.Errorf("connectors = %v, want 1", report.Connectors)
	}
	// An unreadable value is not guessed at.
	if report.HeartbeatInterval != nil {
		t.Errorf("heartbeat interval = %v from %q, want nil", *report.HeartbeatInterval, "sixty")
	}
	for _, s := range report.Settings {
		reported := s.Name == "NumberOfConnectors" || s.Name == "HeartbeatInterval"
		if s.Reported != reported {
			t.Errorf("%s reported = %v, want %v", s.Name, s.Reported, reported)
		}
	}
}

// The value of SupportedFeatureProfiles is optional in 1.6, and some stations
// send it empty: that says no more than leaving the key out does.
func TestProbeEmptyProfilesAreNotReported(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "  ", " , "} {
		cp, _ := newFakeChargePoint(`{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true,"value":"` + value + `"}]}`)
		report, err := cp.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range ocpp.AllCapabilities() {
			want := ocpp.SupportUnknown
			if c == ocpp.CapRemoteStartStop || c == ocpp.CapUnlock {
				want = ocpp.SupportYes
			}
			if got := report.Support(c); got != want {
				t.Errorf("value %q: %s = %s, want %s", value, c, got, want)
			}
		}
	}
}

func TestProbeErrorsAreTheCallsErrors(t *testing.T) {
	t.Parallel()

	cp := v16.NewChargePoint(&fakeCaller{err: ocpp.ErrTimeout}, testConfig())
	if _, err := cp.Probe(context.Background()); !errors.Is(err, ocpp.ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}

	bad, _ := newFakeChargePoint(`{"configurationKey":"nope"}`)
	if _, err := bad.Probe(context.Background()); err == nil {
		t.Error("a malformed answer should be an error")
	}
}
