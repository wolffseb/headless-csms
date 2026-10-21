package v16

import (
	"context"
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

	profiles, reported := values[KeySupportedFeatureProfiles]
	report.Features = features(profiles, reported)

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
// profile the station advertises, in its order. Without the key at all,
// nothing is known about any of them except Core, which every 1.6 station
// must implement: it is assumed present.
func features(value string, reported bool) []ocpp.Feature {
	advertised := parseProfiles(value)
	has := make(map[string]bool, len(advertised))
	for _, p := range advertised {
		has[p] = true
	}

	out := make([]ocpp.Feature, 0, len(neededProfiles)+len(advertised))
	needed := make(map[string]bool, len(neededProfiles))
	for _, n := range neededProfiles {
		needed[n.name] = true
		support, assumed := ocpp.SupportUnknown, false
		if !reported && n.name == profileCore {
			support, assumed = ocpp.SupportYes, true
		}
		if reported {
			support = ocpp.SupportNo
			if has[n.name] {
				support = ocpp.SupportYes
			}
		}
		out = append(out, ocpp.Feature{Name: n.name, Support: support, Grants: n.grants, Assumed: assumed})
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
