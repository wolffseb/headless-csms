// Package config loads and validates cpms's single configuration file.
//
// The tool never writes this file. Everything cpms discovers at runtime —
// the OCPI credentials token the Fryte backend hands us, active reservations,
// last known EVSE status — belongs in state.json instead. Keeping the split
// strict means the operator can hand-edit config.yaml (comments and all)
// without the tool ever clobbering it.
package config

import "time"

// Config is the fully parsed, validated configuration.
type Config struct {
	Charger  Charger  `yaml:"charger"`
	Server   Server   `yaml:"server"`
	Auth     Auth     `yaml:"auth"`
	OCPI     OCPI     `yaml:"ocpi"`
	Location Location `yaml:"location"`
}

// Charger describes the physical charging station we manage.
type Charger struct {
	// ID must match the charge point identity the station uses in its OCPP
	// WebSocket URL: ws://<host>:<port>/ocpp/<id>.
	ID string `yaml:"id"`
	// IP is used only for a reachability pre-check and for display. The OCPP
	// connection itself is dialled by the charger, not by us.
	IP string `yaml:"ip"`
	// OCPPVersion is the version we expect this station to speak.
	OCPPVersion string `yaml:"ocpp_version"`
	// HeartbeatInterval is the interval we hand the station in the
	// BootNotification response, telling it how often to check in.
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	// HeartbeatTimeout is how long we tolerate silence before marking the
	// station offline. It must exceed HeartbeatInterval.
	HeartbeatTimeout Duration `yaml:"heartbeat_timeout"`
	// CallTimeout bounds how long an outbound OCPP request waits for the
	// station to answer.
	CallTimeout Duration `yaml:"call_timeout"`
}

// Server holds the two listeners cpms opens.
type Server struct {
	// OCPPBind is where we listen for the charger's WebSocket connection.
	OCPPBind string `yaml:"ocpp_bind"`
	// OCPIBind is where we serve the OCPI REST interface on the LAN.
	OCPIBind string `yaml:"ocpi_bind"`
}

// Auth holds local authorisation defaults.
type Auth struct {
	// DefaultIDTag is the RFID tag used when a command does not name one —
	// the office tag that redeems reservations.
	DefaultIDTag string `yaml:"default_id_tag"`
}

// OCPI configures our CPO-side OCPI 2.3.0 interface.
//
// Two of the three OCPI tokens live here because they are ours to choose:
// TokenA bootstraps the handshake and TokenC is what the counterparty uses
// afterwards. The third (the counterparty's own token, which we use to call
// them) is only known after registration and so lives in state.json.
type OCPI struct {
	CountryCode string `yaml:"country_code"`
	PartyID     string `yaml:"party_id"`
	// PublicBaseURL is the OCPI root we advertise to the counterparty. It must
	// be reachable from their host, so it is not derived from OCPIBind.
	PublicBaseURL string `yaml:"public_base_url"`
	// TokenA is handed to the counterparty out of band to start registration.
	TokenA string `yaml:"token_a"`
	// TokenC is what the counterparty authenticates with once registered.
	// OCPI normally has the server generate this at handshake time; we take it
	// from config instead so that config.yaml stays read-only.
	TokenC string `yaml:"token_c"`
	Push   Push   `yaml:"push"`
}

// Push tunes the outbound PATCH client that mirrors status changes to the
// registered counterparty.
type Push struct {
	// Debounce coalesces rapid status flapping into a single PATCH.
	Debounce Duration `yaml:"debounce"`
	// MaxRetries bounds the exponential backoff before a change is dropped.
	MaxRetries int `yaml:"max_retries"`
}

// Location is the static OCPI Location data. It is served verbatim; only the
// per-EVSE status is filled in live from the OCPP connection.
type Location struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	Address     string      `yaml:"address"`
	City        string      `yaml:"city"`
	PostalCode  string      `yaml:"postal_code"`
	Country     string      `yaml:"country"`
	Coordinates Coordinates `yaml:"coordinates"`
	EVSEs       []EVSE      `yaml:"evses"`
}

// Coordinates are decimal degrees as strings, matching OCPI's wire format.
type Coordinates struct {
	Latitude  string `yaml:"latitude"`
	Longitude string `yaml:"longitude"`
}

// EVSE bridges the two addressing schemes: OCPI addresses an EVSE by UID,
// OCPP 1.6 addresses it by an integer connector id. The mapping is explicit
// here so that nothing anywhere else has to guess it.
type EVSE struct {
	UID             string      `yaml:"uid"`
	EVSEID          string      `yaml:"evse_id"`
	OCPPConnectorID int         `yaml:"ocpp_connector_id"`
	Connectors      []Connector `yaml:"connectors"`
}

// Connector is an OCPI Connector object.
type Connector struct {
	ID               string `yaml:"id"`
	Standard         string `yaml:"standard"`
	Format           string `yaml:"format"`
	PowerType        string `yaml:"power_type"`
	MaxVoltage       int    `yaml:"max_voltage"`
	MaxAmperage      int    `yaml:"max_amperage"`
	MaxElectricPower int    `yaml:"max_electric_power"`
}

// Defaults applied to any field the operator left out.
const (
	DefaultOCPPVersion       = "1.6"
	DefaultHeartbeatInterval = 60 * time.Second
	DefaultHeartbeatTimeout  = 90 * time.Second
	DefaultCallTimeout       = 30 * time.Second
	DefaultOCPPBind          = "0.0.0.0:9000"
	DefaultOCPIBind          = "0.0.0.0:8080"
	DefaultPushDebounce      = 500 * time.Millisecond
	DefaultPushMaxRetries    = 5
)

// EVSEByUID returns the configured EVSE with the given OCPI uid.
func (c *Config) EVSEByUID(uid string) (EVSE, bool) {
	for _, e := range c.Location.EVSEs {
		if e.UID == uid {
			return e, true
		}
	}
	return EVSE{}, false
}

// EVSEByConnectorID returns the configured EVSE mapped to the given OCPP 1.6
// connector id.
func (c *Config) EVSEByConnectorID(id int) (EVSE, bool) {
	for _, e := range c.Location.EVSEs {
		if e.OCPPConnectorID == id {
			return e, true
		}
	}
	return EVSE{}, false
}

// applyDefaults fills in unset fields. It runs before validation so that
// validation errors always describe the values that will actually be used.
func (c *Config) applyDefaults() {
	if c.Charger.OCPPVersion == "" {
		c.Charger.OCPPVersion = DefaultOCPPVersion
	}
	if c.Charger.HeartbeatInterval.unset() {
		c.Charger.HeartbeatInterval.setDefault(DefaultHeartbeatInterval)
	}
	if c.Charger.HeartbeatTimeout.unset() {
		c.Charger.HeartbeatTimeout.setDefault(DefaultHeartbeatTimeout)
	}
	if c.Charger.CallTimeout.unset() {
		c.Charger.CallTimeout.setDefault(DefaultCallTimeout)
	}
	if c.Server.OCPPBind == "" {
		c.Server.OCPPBind = DefaultOCPPBind
	}
	if c.Server.OCPIBind == "" {
		c.Server.OCPIBind = DefaultOCPIBind
	}
	if c.OCPI.Push.Debounce.unset() {
		c.OCPI.Push.Debounce.setDefault(DefaultPushDebounce)
	}
	if c.OCPI.Push.MaxRetries == 0 {
		c.OCPI.Push.MaxRetries = DefaultPushMaxRetries
	}
}
