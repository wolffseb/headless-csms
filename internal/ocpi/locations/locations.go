// Package locations is the OCPI 2.3.0 Locations module, Sender interface: the
// one configured Location with its EVSEs and Connectors, the static data from
// config.yaml and the live EVSE status from core.
//
// Build is the serialisation; the push client (roadmap step 7) PATCHes
// fragments of the same objects, so it lives apart from the HTTP handlers.
//
// # Status
//
// An EVSE's status is core's effective status, which only the station's own
// reports move (plus the offline and station-wide rules in core). Core's
// reservation records never override it, in any of their states:
//
//   - pending and active: a station that accepted ReserveNow reports the
//     connector Reserved itself, and that is what OCPI shows.
//   - cancelling: ReserveNow got no usable answer, so nobody knows whether the
//     station holds the reservation, and a CancelReservation is on its way.
//     Showing RESERVED would advertise a reservation nobody owns; showing
//     AVAILABLE would contradict a station that does hold it. The station's
//     report is the only witness, so it wins here too. That core treats the
//     EVSE as occupied for a second ReserveNow is a local guard, not a status.
package locations

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// DateTime is an OCPI DateTime: it marshals as RFC 3339 in UTC with a "Z".
type DateTime time.Time

// MarshalJSON renders the time with ocpi.FormatDateTime.
func (d DateTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(ocpi.FormatDateTime(time.Time(d)))
}

// Time returns d as a time.Time.
func (d DateTime) Time() time.Time { return time.Time(d) }

// Location is the OCPI Location object, with the fields config backs. Fields
// are in the spec's order, so the JSON reads like its table.
type Location struct {
	CountryCode string      `json:"country_code"`
	PartyID     string      `json:"party_id"`
	ID          string      `json:"id"`
	Publish     bool        `json:"publish"`
	Name        string      `json:"name,omitempty"`
	Address     string      `json:"address"`
	City        string      `json:"city"`
	PostalCode  string      `json:"postal_code,omitempty"`
	Country     string      `json:"country"`
	Coordinates GeoLocation `json:"coordinates"`
	EVSEs       []EVSE      `json:"evses"`
	TimeZone    string      `json:"time_zone"`
	// LastUpdated is the latest of the Location's own data and every EVSE's
	// and Connector's last_updated, as the date_from/date_to filter requires.
	LastUpdated DateTime `json:"last_updated"`
}

// GeoLocation is OCPI's coordinate pair. Both are strings on the wire and in
// config, and are passed through untouched: a round trip through float64
// could change the digits.
type GeoLocation struct {
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
}

// EVSE is the OCPI EVSE object.
type EVSE struct {
	UID          string          `json:"uid"`
	EVSEID       string          `json:"evse_id,omitempty"`
	Status       core.EVSEStatus `json:"status"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Connectors   []Connector     `json:"connectors"`
	// LastUpdated is the later of its status change and its Connectors'.
	LastUpdated DateTime `json:"last_updated"`
}

// Connector is the OCPI Connector object.
type Connector struct {
	ID               string `json:"id"`
	Standard         string `json:"standard"`
	Format           string `json:"format"`
	PowerType        string `json:"power_type"`
	MaxVoltage       int    `json:"max_voltage"`
	MaxAmperage      int    `json:"max_amperage"`
	MaxElectricPower int    `json:"max_electric_power,omitempty"`
	// LastUpdated is when config last changed: connectors are static data.
	LastUpdated DateTime `json:"last_updated"`
}

// Capability is an OCPI EVSE Capability.
type Capability string

// The capabilities cpms advertises. Commands (roadmap step 8) carries out
// remote start/stop, unlock and reservations through the station.
const (
	CapabilityRemoteStartStop Capability = "REMOTE_START_STOP_CAPABLE"
	CapabilityUnlock          Capability = "UNLOCK_CAPABLE"
	CapabilityReservable      Capability = "RESERVABLE"
)

// Build renders the configured Location with the live status in snap.
// staticAt is when the static data last changed (the config file's
// modification time). It does no I/O, so its output can be golden-tested.
func Build(cfg *config.Config, staticAt time.Time, snap core.Snapshot) Location {
	live := map[string]core.EVSESnapshot{}
	if cp, ok := snap.ChargePoint(cfg.Charger.ID); ok {
		for _, e := range cp.EVSEs {
			live[e.UID] = e
		}
	}

	l := cfg.Location
	loc := Location{
		CountryCode: cfg.OCPI.CountryCode,
		PartyID:     cfg.OCPI.PartyID,
		ID:          l.ID,
		Publish:     l.Publish,
		Name:        l.Name,
		Address:     l.Address,
		City:        l.City,
		PostalCode:  l.PostalCode,
		Country:     l.Country,
		Coordinates: GeoLocation{Latitude: l.Coordinates.Latitude, Longitude: l.Coordinates.Longitude},
		EVSEs:       make([]EVSE, 0, len(l.EVSEs)),
		TimeZone:    l.TimeZone,
	}
	locUpdated := staticAt
	for _, e := range l.EVSEs {
		evse := buildEVSE(e, staticAt, live[e.UID])
		locUpdated = latest(locUpdated, evse.LastUpdated.Time())
		loc.EVSEs = append(loc.EVSEs, evse)
	}
	loc.LastUpdated = DateTime(locUpdated)
	return loc
}

func buildEVSE(e config.EVSE, staticAt time.Time, live core.EVSESnapshot) EVSE {
	status := live.Status
	if status == "" {
		// Not in the snapshot: core tracks every configured EVSE, so this is
		// only a snapshot from elsewhere, and it knows nothing about it.
		status = core.StatusUnknown
	}

	caps := []Capability{CapabilityRemoteStartStop, CapabilityUnlock}
	if e.IsReservable() {
		caps = append(caps, CapabilityReservable)
	}

	evse := EVSE{
		UID:          e.UID,
		EVSEID:       e.EVSEID,
		Status:       status,
		Capabilities: caps,
		Connectors:   make([]Connector, 0, len(e.Connectors)),
	}
	updated := latest(staticAt, live.LastUpdated)
	for _, c := range e.Connectors {
		evse.Connectors = append(evse.Connectors, Connector{
			ID:               c.ID,
			Standard:         c.Standard,
			Format:           c.Format,
			PowerType:        c.PowerType,
			MaxVoltage:       c.MaxVoltage,
			MaxAmperage:      c.MaxAmperage,
			MaxElectricPower: c.MaxElectricPower,
			LastUpdated:      DateTime(staticAt),
		})
	}
	evse.LastUpdated = DateTime(updated)
	return evse
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// evse finds an EVSE by uid. OCPI ids are CiStrings, compared without case.
func (l Location) evse(uid string) (EVSE, bool) {
	for _, e := range l.EVSEs {
		if strings.EqualFold(e.UID, uid) {
			return e, true
		}
	}
	return EVSE{}, false
}

// connector finds a Connector by id, without case like evse.
func (e EVSE) connector(id string) (Connector, bool) {
	for _, c := range e.Connectors {
		if strings.EqualFold(c.ID, id) {
			return c, true
		}
	}
	return Connector{}, false
}
