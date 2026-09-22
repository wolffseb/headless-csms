package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	// chargePointIDRe keeps the id safe to use verbatim as a URL path segment,
	// since it appears in ws://host:port/ocpp/<id>.
	chargePointIDRe = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,48}$`)
	// idTagRe is OCPP 1.6 IdToken: CiString20, printable ASCII, no spaces.
	idTagRe = regexp.MustCompile(`^[\x21-\x7E]{1,20}$`)
	// countryCodeRe is the OCPI 2-letter party country code (ISO 3166-1 alpha-2).
	countryCodeRe = regexp.MustCompile(`^[A-Z]{2}$`)
	// partyIDRe is the OCPI 3-letter party id.
	partyIDRe = regexp.MustCompile(`^[A-Z]{3}$`)
	// alpha3Re is the ISO 3166-1 alpha-3 code used by OCPI Location.country.
	alpha3Re = regexp.MustCompile(`^[A-Z]{3}$`)
	// latitudeRe and longitudeRe are OCPI's own Coordinates patterns: decimal
	// degrees as a string with 5 to 7 decimal places.
	latitudeRe  = regexp.MustCompile(`^-?[0-9]{1,2}\.[0-9]{5,7}$`)
	longitudeRe = regexp.MustCompile(`^-?[0-9]{1,3}\.[0-9]{5,7}$`)
	// evseIDRe is a light eMI3 check: country, party, an 'E', then the rest.
	evseIDRe = regexp.MustCompile(`^[A-Za-z]{2}\*?[A-Za-z0-9]{3}\*?[Ee][A-Za-z0-9*]{1,30}$`)
)

// minTokenLen is a floor on the OCPI tokens we hand out. OCPI sets no minimum,
// but a two-character shared secret on a LAN endpoint is a mistake worth
// catching at validation time.
const minTokenLen = 8

// Validate checks the whole config and returns every problem it finds as
// ValidationErrors, or nil if the config is usable.
//
// It also resolves duration fields, so a Config that validates cleanly has
// every Duration parsed.
func (c *Config) Validate() error {
	v := &validator{}

	c.validateCharger(v)
	c.validateServer(v)
	c.validateAuth(v)
	c.validateOCPI(v)
	c.validateLocation(v)

	if len(v.errs) == 0 {
		return nil
	}
	v.errs.sortByPath()
	return v.errs
}

func (c *Config) validateCharger(v *validator) {
	if v.required("charger.id", c.Charger.ID) && !chargePointIDRe.MatchString(c.Charger.ID) {
		v.add("charger.id", "%q is not usable as a URL path segment; allowed: letters, digits and . _ : ~ - (max 48)", c.Charger.ID)
	}

	// IP is optional: it is only used for the reachability pre-check. When it
	// is given it has to be an address or a resolvable-looking hostname.
	if ip := strings.TrimSpace(c.Charger.IP); ip != "" {
		if net.ParseIP(ip) == nil && !isHostname(ip) {
			v.add("charger.ip", "%q is neither an IP address nor a hostname", ip)
		}
	}

	if _, ok := ocppVersions[c.Charger.OCPPVersion]; !ok {
		v.add("charger.ocpp_version", "%q is not supported; want one of %s",
			c.Charger.OCPPVersion, strings.Join(sorted(ocppVersions), ", "))
	}

	intervalOK := positiveDuration(v, "charger.heartbeat_interval", &c.Charger.HeartbeatInterval)
	timeoutOK := positiveDuration(v, "charger.heartbeat_timeout", &c.Charger.HeartbeatTimeout)
	positiveDuration(v, "charger.call_timeout", &c.Charger.CallTimeout)

	// A timeout at or below the interval we ask the station to keep guarantees
	// it will be marked offline between two perfectly punctual heartbeats.
	if intervalOK && timeoutOK && c.Charger.HeartbeatTimeout.dur <= c.Charger.HeartbeatInterval.dur {
		v.add("charger.heartbeat_timeout", "must be greater than charger.heartbeat_interval (%s), got %s",
			c.Charger.HeartbeatInterval, c.Charger.HeartbeatTimeout)
	}
}

// positiveDuration parses a duration field and requires it to be above zero,
// reporting problems against the field's own path.
func positiveDuration(v *validator, path string, d *Duration) bool {
	if err := d.parse(); err != nil {
		v.add(path, "%q is not a duration; want e.g. \"90s\"", d.raw)
		return false
	}
	if d.dur <= 0 {
		v.add(path, "must be greater than zero, got %s", d.raw)
		return false
	}
	return true
}

func (c *Config) validateServer(v *validator) {
	ocppHost, ocppPort, ocppOK := validateBind(v, "server.ocpp_bind", c.Server.OCPPBind)
	ocpiHost, ocpiPort, ocpiOK := validateBind(v, "server.ocpi_bind", c.Server.OCPIBind)

	// Two listeners collide when they share a port and either share a host or
	// one of them is a wildcard.
	if ocppOK && ocpiOK && ocppPort == ocpiPort {
		if ocppHost == ocpiHost || isWildcardHost(ocppHost) || isWildcardHost(ocpiHost) {
			v.add("server.ocpi_bind", "cannot share port %d with server.ocpp_bind (%s)", ocpiPort, c.Server.OCPPBind)
		}
	}
}

// validateBind checks a host:port listen address and returns its parts.
func validateBind(v *validator, path, bind string) (host string, port int, ok bool) {
	if !v.required(path, bind) {
		return "", 0, false
	}
	host, portStr, err := net.SplitHostPort(bind)
	if err != nil {
		v.add(path, "%q is not a host:port address", bind)
		return "", 0, false
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		v.add(path, "%q does not have a valid port (1-65535)", bind)
		return "", 0, false
	}
	if port == 0 {
		// Port 0 binds to whatever is free, which nobody can be configured to
		// dial. For a listener a charger and a backend must find, that is a
		// mistake rather than a feature.
		v.add(path, "port 0 picks a random port that nothing can be pointed at; choose a fixed port")
		return "", 0, false
	}
	// An empty host means "all interfaces" and is fine.
	if host != "" && !isWildcardHost(host) && net.ParseIP(host) == nil && !isHostname(host) {
		v.add(path, "%q is not a valid host", host)
		return "", 0, false
	}
	return host, port, true
}

func (c *Config) validateAuth(v *validator) {
	if v.required("auth.default_id_tag", c.Auth.DefaultIDTag) && !idTagRe.MatchString(c.Auth.DefaultIDTag) {
		v.add("auth.default_id_tag",
			"%q is not a valid OCPP idTag; want 1-20 printable ASCII characters without spaces", c.Auth.DefaultIDTag)
	}
}

func (c *Config) validateOCPI(v *validator) {
	o := &c.OCPI

	if v.required("ocpi.country_code", o.CountryCode) && !countryCodeRe.MatchString(o.CountryCode) {
		v.add("ocpi.country_code", "%q must be two uppercase letters, e.g. \"DE\"", o.CountryCode)
	}
	if v.required("ocpi.party_id", o.PartyID) && !partyIDRe.MatchString(o.PartyID) {
		v.add("ocpi.party_id", "%q must be three uppercase letters, e.g. \"FRY\"", o.PartyID)
	}

	if v.required("ocpi.public_base_url", o.PublicBaseURL) {
		u, err := url.Parse(o.PublicBaseURL)
		switch {
		case err != nil:
			v.add("ocpi.public_base_url", "%q is not a valid URL: %v", o.PublicBaseURL, err)
		case u.Scheme != "http" && u.Scheme != "https":
			v.add("ocpi.public_base_url", "%q must use http or https", o.PublicBaseURL)
		case u.Host == "":
			v.add("ocpi.public_base_url", "%q is missing a host", o.PublicBaseURL)
		}
	}

	validateToken(v, "ocpi.token_a", o.TokenA)
	validateToken(v, "ocpi.token_c", o.TokenC)
	if o.TokenA != "" && o.TokenA == o.TokenC {
		v.add("ocpi.token_c", "must differ from ocpi.token_a")
	}

	if err := o.Push.Debounce.parse(); err != nil {
		v.add("ocpi.push.debounce", "%q is not a duration; want e.g. \"500ms\"", o.Push.Debounce.raw)
	} else if o.Push.Debounce.dur < 0 {
		v.add("ocpi.push.debounce", "must not be negative, got %s", o.Push.Debounce.raw)
	}

	if o.Push.MaxRetries < 0 {
		v.add("ocpi.push.max_retries", "must not be negative, got %d", o.Push.MaxRetries)
	}
}

func validateToken(v *validator, path, token string) {
	if !v.required(path, token) {
		return
	}
	if len(token) < minTokenLen {
		v.add(path, "must be at least %d characters", minTokenLen)
	}
	// The token is sent base64-encoded in an Authorization header; whitespace
	// in the source value is almost always an accident.
	if strings.ContainsAny(token, " \t\r\n") {
		v.add(path, "must not contain whitespace")
	}
}

func (c *Config) validateLocation(v *validator) {
	l := c.Location

	if v.required("location.id", l.ID) && len(l.ID) > 36 {
		v.add("location.id", "must be at most 36 characters (OCPI CiString36), got %d", len(l.ID))
	}
	v.required("location.address", l.Address)
	v.required("location.city", l.City)
	if v.required("location.country", l.Country) && !alpha3Re.MatchString(l.Country) {
		v.add("location.country", "%q must be an ISO 3166-1 alpha-3 code, e.g. \"DEU\"", l.Country)
	}

	validateCoordinate(v, "location.coordinates.latitude", l.Coordinates.Latitude, latitudeRe, -90, 90)
	validateCoordinate(v, "location.coordinates.longitude", l.Coordinates.Longitude, longitudeRe, -180, 180)

	if len(l.EVSEs) == 0 {
		v.add("location.evses", "at least one EVSE is required")
		return
	}

	seenUID := map[string]int{}
	seenConnectorID := map[int]int{}
	seenEVSEID := map[string]int{}

	for i, e := range l.EVSEs {
		base := fmt.Sprintf("location.evses[%d]", i)

		if v.required(base+".uid", e.UID) {
			if len(e.UID) > 36 {
				v.add(base+".uid", "must be at most 36 characters (OCPI CiString36), got %d", len(e.UID))
			}
			if first, dup := seenUID[e.UID]; dup {
				v.add(base+".uid", "%q is already used by location.evses[%d]", e.UID, first)
			} else {
				seenUID[e.UID] = i
			}
		}

		if v.required(base+".evse_id", e.EVSEID) {
			if !evseIDRe.MatchString(e.EVSEID) {
				v.add(base+".evse_id", "%q does not look like an eMI3 EVSE ID, e.g. \"DE*FRY*E001*1\"", e.EVSEID)
			}
			if first, dup := seenEVSEID[e.EVSEID]; dup {
				v.add(base+".evse_id", "%q is already used by location.evses[%d]", e.EVSEID, first)
			} else {
				seenEVSEID[e.EVSEID] = i
			}
		}

		if e.OCPPConnectorID < 1 {
			// OCPP 1.6 reserves connector 0 for the charge point as a whole,
			// so an EVSE must map to 1 or higher.
			v.add(base+".ocpp_connector_id", "must be 1 or greater (0 addresses the charge point itself), got %d", e.OCPPConnectorID)
		} else if first, dup := seenConnectorID[e.OCPPConnectorID]; dup {
			v.add(base+".ocpp_connector_id", "%d is already mapped by location.evses[%d]", e.OCPPConnectorID, first)
		} else {
			seenConnectorID[e.OCPPConnectorID] = i
		}

		validateConnectors(v, base, e.Connectors)
	}
}

func validateConnectors(v *validator, base string, connectors []Connector) {
	if len(connectors) == 0 {
		v.add(base+".connectors", "at least one connector is required")
		return
	}

	seen := map[string]int{}
	for j, conn := range connectors {
		path := fmt.Sprintf("%s.connectors[%d]", base, j)

		if v.required(path+".id", conn.ID) {
			if first, dup := seen[conn.ID]; dup {
				v.add(path+".id", "%q is already used by %s.connectors[%d]", conn.ID, base, first)
			} else {
				seen[conn.ID] = j
			}
		}

		if v.required(path+".standard", conn.Standard) {
			if _, ok := connectorStandards[conn.Standard]; !ok {
				v.add(path+".standard", "%q is not an OCPI 2.3.0 ConnectorType", conn.Standard)
			}
		}
		if v.required(path+".format", conn.Format) {
			if _, ok := connectorFormats[conn.Format]; !ok {
				v.add(path+".format", "%q is not a ConnectorFormat; want one of %s",
					conn.Format, strings.Join(sorted(connectorFormats), ", "))
			}
		}
		if v.required(path+".power_type", conn.PowerType) {
			if _, ok := powerTypes[conn.PowerType]; !ok {
				v.add(path+".power_type", "%q is not a PowerType; want one of %s",
					conn.PowerType, strings.Join(sorted(powerTypes), ", "))
			}
		}

		if conn.MaxVoltage <= 0 {
			v.add(path+".max_voltage", "must be greater than zero, got %d", conn.MaxVoltage)
		}
		if conn.MaxAmperage <= 0 {
			v.add(path+".max_amperage", "must be greater than zero, got %d", conn.MaxAmperage)
		}
		// max_electric_power is optional in OCPI; zero means "not stated".
		if conn.MaxElectricPower < 0 {
			v.add(path+".max_electric_power", "must not be negative, got %d", conn.MaxElectricPower)
		}
	}
}

func validateCoordinate(v *validator, path, value string, pattern *regexp.Regexp, lo, hi float64) {
	if !v.required(path, value) {
		return
	}
	if !pattern.MatchString(value) {
		v.add(path, "%q must be decimal degrees as a string with 5 to 7 decimal places, e.g. \"52.520008\"", value)
		return
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		v.add(path, "%q is not a number", value)
		return
	}
	if f < lo || f > hi {
		v.add(path, "%v is out of range (%v to %v)", f, lo, hi)
	}
}

func isWildcardHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// isHostname reports whether s looks like a DNS name. It is a shape check, not
// a resolution attempt: validation must work without a network.
func isHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !isAlnum && r != '-' {
				return false
			}
		}
	}
	return true
}
