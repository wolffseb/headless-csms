package v16

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// The 1.6 feature profiles cpms needs, and what each one gives it. Core is
// mandatory in 1.6: a station that does not report its profiles at all is
// assumed to implement it, while one that lists profiles without it does not.
var neededProfiles = []struct {
	name   string
	grants []ocpp.Capability
}{
	{profileCore, []ocpp.Capability{ocpp.CapRemoteStartStop, ocpp.CapUnlock}},
	{profileReservation, []ocpp.Capability{ocpp.CapReservation}},
	{profileRemoteTrigger, []ocpp.Capability{ocpp.CapTriggerStatus}},
	{profileSmartCharging, []ocpp.Capability{ocpp.CapSmartCharging}},
}

// probedSettings are the keys that change how the station behaves towards
// cpms, in the order a report shows them, with why each one matters.
var probedSettings = []struct {
	key     string
	meaning string
}{
	{KeyReserveConnectorZeroSupported,
		"whether the whole station can be reserved at once; cpms always reserves one EVSE, so this is informational"},
	{KeyAuthorizeRemoteTxRequests,
		"if true, the station asks cpms to Authorize the tag of a remote start; cpms accepts only auth.default_id_tag"},
	{KeyLocalAuthListEnabled,
		"if true, a whitelist held on the station can let tags charge without asking cpms"},
	{KeyLocalPreAuthorize,
		"if true, the station starts at once for tags it knows locally, before cpms answers"},
	{KeyLocalAuthorizeOffline,
		"if true, the station lets tags it knows locally charge while cpms is unreachable"},
	{KeyNumberOfConnectors,
		"how many connectors the station has; checked against the EVSEs in config"},
	{KeyHeartbeatInterval,
		"seconds between heartbeats; cpms sets it at boot from charger.heartbeat_interval"},
	{KeyConnectionTimeOut,
		"seconds the station waits for a cable after a remote start or a presented tag"},
	{KeyMeterValueSampleInterval,
		"seconds between meter values during a session; 0 means none are sent"},
	{KeyMeterValuesSampledData,
		"measurands sent during a session; Power.Offered and Current.Offered carry a charging limit, Power.Active.Import and Current.Import its effect; entries may end in a phase, as in Current.Import.L1"},
	{KeyChargingScheduleAllowedChargingRateUnit,
		"units a charging schedule may use (Current, Power or both); decides whether a limit can be set in kW or in A"},
	{KeyChargeProfileMaxStackLevel,
		"highest stack level the station accepts for a charging profile"},
	{KeyChargingScheduleMaxPeriods,
		"most periods the station accepts in one charging schedule"},
	{KeyMaxChargingProfilesInstalled,
		"how many charging profiles the station keeps installed at once"},
}

// Probe reads the station's whole configuration and turns it into a report.
//
// Every key is asked for at once, rather than only the interesting ones: the
// raw dump is part of the report, and one round trip is kinder to a station
// than several. A key the station does not report is "not reported", never
// an error.
func (cp *ChargePoint) Probe(ctx context.Context) (ocpp.ProbeReport, error) {
	conf, err := cp.GetConfiguration(ctx, nil)
	if err != nil {
		return ocpp.ProbeReport{}, err
	}

	values := make(map[string]string, len(conf.ConfigurationKey))
	report := ocpp.ProbeReport{
		Version: cp.Version(),
		Raw:     make([]ocpp.RawSetting, 0, len(conf.ConfigurationKey)),
	}
	for _, k := range conf.ConfigurationKey {
		values[k.Key] = k.Value
		report.Raw = append(report.Raw, ocpp.RawSetting{Key: k.Key, Value: k.Value, Readonly: k.Readonly})
	}

	report.Features = features(values)

	for _, s := range probedSettings {
		value, ok := values[s.key]
		report.Settings = append(report.Settings, ocpp.Setting{
			Name: s.key, Value: value, Reported: ok, Meaning: s.meaning,
		})
	}

	if n, ok := intValue(values, KeyNumberOfConnectors); ok {
		report.Connectors = &n
	}
	if n, ok := intValue(values, KeyHeartbeatInterval); ok {
		d := time.Duration(n) * time.Second
		report.HeartbeatInterval = &d
	}
	return report, nil
}

// features lists the profiles cpms needs, present or not, then every other
// profile the station advertises, in its order. It reads
// SupportedFeatureProfiles and SupportedFeatureProfilesMaxLength from values.
//
// Without the profile list, or with an empty one (the value is optional in
// 1.6), nothing is known about any profile except Core, which every 1.6
// station must implement: it is assumed present.
//
// A list as long as SupportedFeatureProfilesMaxLength may have been cut
// short: an Alpitronic HYC50 implements six profiles, can list five, and
// leaves out Reservation, which it supports. A profile missing from a full
// list is therefore unknown, not absent, except Core, which is mandatory and
// so assumed present. Without a usable max length, a list is taken at its
// word.
//
// This is a vendor workaround, not spec: OCPP 1.6 errata 3.81 withdraws
// SupportedFeatureProfilesMaxLength ("should not have been part of OCPP
// 1.6"). Its price: a station whose list is complete and exactly as long as
// its max length, and which really lacks a profile, shows that profile as
// unconfirmed (exit 0) instead of missing (exit 10).
func features(values map[string]string) []ocpp.Feature {
	advertised := parseProfiles(values[KeySupportedFeatureProfiles])
	reported := len(advertised) > 0
	has := make(map[string]bool, len(advertised))
	for _, p := range advertised {
		has[p] = true
	}
	maxLength, ok := intValue(values, KeySupportedFeatureProfilesMaxLength)
	full := ok && maxLength > 0 && len(advertised) >= maxLength

	out := make([]ocpp.Feature, 0, len(neededProfiles)+len(advertised))
	needed := make(map[string]bool, len(neededProfiles))
	for _, n := range neededProfiles {
		needed[n.name] = true
		f := ocpp.Feature{Name: n.name, Support: ocpp.SupportUnknown, Grants: n.grants}
		switch {
		case !reported, full && n.name == profileCore && !has[n.name]:
			if n.name == profileCore {
				f.Support, f.Assumed = ocpp.SupportYes, true
			}
		case has[n.name]:
			f.Support = ocpp.SupportYes
		case full:
			f.Note = fmt.Sprintf("%s is not in the station's profile list, but the list is full (%s = %d), "+
				"so a profile missing from it may still be supported", n.name, KeySupportedFeatureProfilesMaxLength, maxLength)
		default:
			f.Support = ocpp.SupportNo
		}
		out = append(out, f)
	}
	for _, p := range advertised {
		if !needed[p] {
			out = append(out, ocpp.Feature{Name: p, Support: ocpp.SupportYes})
		}
	}
	return out
}

// parseProfiles splits a SupportedFeatureProfiles CSL: "Core,Reservation",
// sometimes with spaces after the commas.
func parseProfiles(value string) []string {
	var out []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// intValue reads an integer key. A value that is not a non-negative integer
// is treated as unreported rather than guessed at.
func intValue(values map[string]string, key string) (int, bool) {
	v, ok := values[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
