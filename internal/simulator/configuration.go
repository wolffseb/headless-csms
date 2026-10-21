package simulator

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// configEntry is one configuration key as GetConfiguration and
// ChangeConfiguration see it.
type configEntry struct {
	key      string
	readonly bool
	value    string
	// set applies a new value, or says why it cannot. It is nil for a
	// read-only key.
	set func(value string) error
}

// defaultSettings are the writable keys that the simulator only stores. The
// values are what a station set up for a CSMS that decides every
// authorization itself: no local whitelist, no shortcuts.
func defaultSettings() map[string]string {
	return map[string]string{
		v16.KeyConnectionTimeOut:         "60",
		v16.KeyAuthorizeRemoteTxRequests: "false",
		v16.KeyLocalAuthListEnabled:      "false",
		v16.KeyLocalPreAuthorize:         "false",
		v16.KeyLocalAuthorizeOffline:     "false",
	}
}

// configuration lists every key the simulator knows, in the order
// GetConfiguration reports them, with the keys the options leave unreported
// already removed.
func (s *Simulator) configuration() []configEntry {
	// The Reservation profile is advertised unless the scenario says the
	// station refuses reservations, so that `cpms probe` reports the same
	// capability the station then actually honours.
	profiles := "Core,FirmwareManagement,RemoteTrigger,Reservation"
	if s.opts.Scenario == ScenarioRejectReserve {
		profiles = "Core,FirmwareManagement,RemoteTrigger"
	}
	if s.opts.FeatureProfiles != "" {
		profiles = s.opts.FeatureProfiles
	}

	s.mu.Lock()
	heartbeat := s.heartbeat
	meterInterval := s.meterInterval
	settings := make(map[string]string, len(s.settings))
	for k, v := range s.settings {
		settings[k] = v
	}
	s.mu.Unlock()

	stored := func(key string, parse func(string) error) configEntry {
		return configEntry{key: key, value: settings[key], set: func(value string) error {
			if err := parse(value); err != nil {
				return err
			}
			s.mu.Lock()
			s.settings[key] = value
			s.mu.Unlock()
			return nil
		}}
	}

	var all []configEntry
	for _, e := range []configEntry{
		{key: v16.KeySupportedFeatureProfiles, readonly: true, value: profiles},
		{key: v16.KeyNumberOfConnectors, readonly: true, value: strconv.Itoa(s.opts.Connectors)},
		{key: v16.KeyHeartbeatInterval, value: seconds(heartbeat), set: s.setHeartbeatInterval},
		stored(v16.KeyConnectionTimeOut, parseSeconds),
		{key: v16.KeyMeterValueSampleInterval, value: seconds(meterInterval), set: s.setMeterInterval},
		stored(v16.KeyAuthorizeRemoteTxRequests, parseBool),
		stored(v16.KeyLocalAuthListEnabled, parseBool),
		stored(v16.KeyLocalPreAuthorize, parseBool),
		stored(v16.KeyLocalAuthorizeOffline, parseBool),
		{key: v16.KeyReserveConnectorZeroSupported, readonly: true, value: "false"},
	} {
		if !slices.Contains(s.opts.UnreportedKeys, e.key) {
			all = append(all, e)
		}
	}
	return all
}

// seconds renders a duration as the whole seconds a configuration key holds.
// It rounds up, so that a sub-second test interval does not read as 0, which
// would mean "off".
func seconds(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10)
}

func parseSeconds(value string) error {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return fmt.Errorf("%q is not a number of seconds", value)
	}
	return nil
}

func parseBool(value string) error {
	if _, err := strconv.ParseBool(value); err != nil {
		return fmt.Errorf("%q is not a boolean", value)
	}
	return nil
}

// setHeartbeatInterval takes effect from the next heartbeat on.
func (s *Simulator) setHeartbeatInterval(value string) error {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fmt.Errorf("%q is not a positive number of seconds", value)
	}
	s.mu.Lock()
	s.heartbeat = time.Duration(n) * time.Second
	s.mu.Unlock()
	return nil
}

// setMeterInterval takes effect from the next sample on. Turning streaming on
// starts it for sessions already running, as a station would.
func (s *Simulator) setMeterInterval(value string) error {
	if err := parseSeconds(value); err != nil {
		return err
	}
	n, _ := strconv.Atoi(value)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.meterInterval = time.Duration(n) * time.Second
	for id, c := range s.connectors {
		s.startMeterLocked(id, c)
	}
	return nil
}

func (s *Simulator) getConfiguration(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.GetConfigurationReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	var all []v16.ConfigurationKey
	for _, e := range s.configuration() {
		all = append(all, v16.ConfigurationKey{Key: e.key, Readonly: e.readonly, Value: e.value})
	}

	if len(req.Key) == 0 {
		return v16.GetConfigurationConf{ConfigurationKey: all}, nil
	}

	conf := v16.GetConfigurationConf{}
	for _, want := range req.Key {
		found := false
		for _, key := range all {
			if key.Key == want {
				conf.ConfigurationKey = append(conf.ConfigurationKey, key)
				found = true
				break
			}
		}
		if !found {
			conf.UnknownKey = append(conf.UnknownKey, want)
		}
	}
	return conf, nil
}

// changeConfiguration sets one key: NotSupported for a key the station does
// not know (or does not report), Rejected for a read-only key or a value it
// cannot take, Accepted otherwise. Nothing here needs a reboot.
func (s *Simulator) changeConfiguration(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.ChangeConfigurationReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	entries := s.configuration()
	i := slices.IndexFunc(entries, func(e configEntry) bool { return e.key == req.Key })
	if i < 0 {
		return v16.ChangeConfigurationConf{Status: v16.ConfigNotSupported}, nil
	}
	e := entries[i]
	if e.readonly {
		return v16.ChangeConfigurationConf{Status: v16.ConfigRejected}, nil
	}
	if err := e.set(req.Value); err != nil {
		s.log.Info("refusing configuration change", "key", req.Key, "error", err)
		return v16.ChangeConfigurationConf{Status: v16.ConfigRejected}, nil
	}

	s.log.Info("configuration changed", "key", req.Key, "value", req.Value)
	return v16.ChangeConfigurationConf{Status: v16.ConfigAccepted}, nil
}
