package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads, defaults and validates the config file at path.
//
// Unknown keys are rejected rather than ignored: a silently-dropped
// "hearbeat_timeout" typo would leave the operator wondering why their setting
// had no effect.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// Stat the file being read, not the path, so the time belongs to these
	// contents even if the file is replaced in between.
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cfg, err := parse(f)
	if err != nil {
		return nil, err
	}
	cfg.ModTime = info.ModTime()
	return cfg, nil
}

// Parse reads a config from any reader. Load is the usual entry point; this
// exists for tests and for reading from stdin.
func Parse(r io.Reader) (*Config, error) { return parse(r) }

func parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ValidationErrors{{Path: "", Msg: "file is empty"}}
		}
		return nil, yamlErrors(err)
	}

	// A second document is almost certainly a stray "---" and would be
	// silently ignored otherwise.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, ValidationErrors{{Path: "", Msg: "file contains more than one YAML document"}}
	}

	cfg.normalize()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// normalize trims incidental whitespace and formatting so that validation and
// everything downstream see one canonical form.
func (c *Config) normalize() {
	c.Charger.ID = strings.TrimSpace(c.Charger.ID)
	c.Charger.IP = strings.TrimSpace(c.Charger.IP)
	c.Charger.OCPPVersion = strings.TrimSpace(c.Charger.OCPPVersion)
	c.Server.OCPPBind = strings.TrimSpace(c.Server.OCPPBind)
	c.Server.OCPIBind = strings.TrimSpace(c.Server.OCPIBind)
	c.Auth.DefaultIDTag = strings.TrimSpace(c.Auth.DefaultIDTag)
	c.OCPI.CountryCode = strings.TrimSpace(c.OCPI.CountryCode)
	c.OCPI.PartyID = strings.TrimSpace(c.OCPI.PartyID)
	// A trailing slash on the advertised root would produce "//" in every URL
	// we hand to the counterparty.
	c.OCPI.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.OCPI.PublicBaseURL), "/")
	c.OCPI.TokenA = strings.TrimSpace(c.OCPI.TokenA)
	c.OCPI.TokenC = strings.TrimSpace(c.OCPI.TokenC)
	c.Location.ID = strings.TrimSpace(c.Location.ID)
	c.Location.Country = strings.TrimSpace(c.Location.Country)
	c.Location.TimeZone = strings.TrimSpace(c.Location.TimeZone)
	c.Location.Coordinates.Latitude = strings.TrimSpace(c.Location.Coordinates.Latitude)
	c.Location.Coordinates.Longitude = strings.TrimSpace(c.Location.Coordinates.Longitude)

	for i := range c.Location.EVSEs {
		e := &c.Location.EVSEs[i]
		e.UID = strings.TrimSpace(e.UID)
		e.EVSEID = strings.TrimSpace(e.EVSEID)
		for j := range e.Connectors {
			conn := &e.Connectors[j]
			conn.ID = strings.TrimSpace(conn.ID)
			conn.Standard = strings.TrimSpace(conn.Standard)
			conn.Format = strings.TrimSpace(conn.Format)
			conn.PowerType = strings.TrimSpace(conn.PowerType)
		}
	}
}

// typeYAMLPath maps the Go types yaml.v3 names in its errors back to the YAML
// paths an operator actually edits, so "field foo not found in type
// config.Charger" can be reported as "charger.foo".
var typeYAMLPath = map[string]string{
	"config.Config":      "",
	"config.Charger":     "charger",
	"config.Server":      "server",
	"config.Auth":        "auth",
	"config.OCPI":        "ocpi",
	"config.Push":        "ocpi.push",
	"config.Location":    "location",
	"config.Coordinates": "location.coordinates",
	"config.EVSE":        "location.evses[]",
	"config.Connector":   "location.evses[].connectors[]",
}

var (
	unknownFieldRe = regexp.MustCompile(`^line (\d+): field (\S+) not found in type (\S+)$`)
	lineRe         = regexp.MustCompile(`^line (\d+): (.*)$`)
)

// yamlErrors turns a yaml decode failure into ValidationErrors, recovering the
// field path where the underlying message carries enough information.
func yamlErrors(err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		// A syntax error: no field context available, report it as-is.
		return ValidationErrors{{Path: "", Msg: strings.TrimPrefix(err.Error(), "yaml: ")}}
	}

	out := make(ValidationErrors, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		if m := unknownFieldRe.FindStringSubmatch(msg); m != nil {
			line, field, typeName := m[1], m[2], m[3]
			path := field
			if prefix, ok := typeYAMLPath[typeName]; ok && prefix != "" {
				path = prefix + "." + field
			}
			out = append(out, FieldError{Path: path, Msg: fmt.Sprintf("unknown field (line %s)", line)})
			continue
		}
		if m := lineRe.FindStringSubmatch(msg); m != nil {
			out = append(out, FieldError{Path: "line " + m[1], Msg: m[2]})
			continue
		}
		out = append(out, FieldError{Path: "", Msg: msg})
	}
	return out
}
