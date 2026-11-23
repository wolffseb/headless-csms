// Package state persists what cpms learns at runtime to state.json: the OCPI
// registration, the active reservations and transactions, the charging
// profiles the station accepted, and the id counters that keep reservation,
// transaction, remote-start and charging-profile ids from being reused.
//
// config.yaml is read-only to the tool; this is the only file cpms writes. The
// package imports nothing else from internal/, so any layer can use it without
// an import cycle. It holds plain DTOs: consumers map to and from their own
// domain types.
//
// A Store is safe for concurrent use within one process. It does not guard
// against a second cpms process writing the same file: the control socket is
// the single-instance guard, not this package.
package state

import "time"

// Version is the schema version this build reads and writes, and the only one
// it accepts.
//
// Every schema change bumps it, adding an optional field included: an older
// cpms that read a newer file would silently drop the fields it does not know
// on its next write, and refusing the file is better than losing data. Each
// bump ships with a migration from the previous version, so an upgraded cpms
// still reads the file the old one wrote.
//
// Version 2 added next_charging_profile_id and charging_profiles. A version 1
// file has neither, which decodes as a zero counter and no profiles: the
// migration is to read it and write it back as version 2.
const Version = 2

// State is the whole of state.json.
type State struct {
	Version           int               `json:"version"`
	OCPI              *OCPIRegistration `json:"ocpi,omitempty"` // nil = not registered
	NextReservationID int               `json:"next_reservation_id"`
	NextTransactionID int               `json:"next_transaction_id"`
	// NextRemoteStartID feeds the remoteStartId OCPP 2.0.1 requires on
	// RequestStartTransaction and echoes in TransactionEvent. 1.6 has no such
	// field, but the counter advances for every remote start regardless.
	NextRemoteStartID int `json:"next_remote_start_id"`
	// NextChargingProfileID feeds chargingProfileId: a profile with the id
	// of one already installed replaces it at the station, so ids are never
	// reused.
	NextChargingProfileID int           `json:"next_charging_profile_id"`
	Reservations          []Reservation `json:"reservations"` // active only
	Transactions          []Transaction `json:"transactions"` // active only: this is not a history
	// ChargingProfiles are the profiles the station accepted, as sent. The
	// station is the source of truth: it may have dropped them since, for
	// instance on a reboot.
	ChargingProfiles []ChargingProfile `json:"charging_profiles"`
}

// OCPIRegistration is what the counterparty told us during the credentials
// handshake.
type OCPIRegistration struct {
	RegisteredAt time.Time   `json:"registered_at"`
	TokenB       string      `json:"token_b"` // theirs; we send it on every request to them
	VersionsURL  string      `json:"versions_url"`
	Version      string      `json:"version"` // e.g. "2.3.0"
	Endpoints    []Endpoint  `json:"endpoints"`
	Roles        []PartyRole `json:"roles"` // from their credentials object
}

// Endpoint is one module endpoint the counterparty exposes.
type Endpoint struct {
	Identifier string `json:"identifier"`
	Role       string `json:"role"`
	URL        string `json:"url"`
}

// PartyRole is one role the counterparty registered with.
type PartyRole struct {
	CountryCode string `json:"country_code"`
	PartyID     string `json:"party_id"`
	Role        string `json:"role"`
}

// Reservation is an active reservation.
type Reservation struct {
	ID                int       `json:"id"`                            // OCPP-side id, unique, never reused
	OCPIReservationID string    `json:"ocpi_reservation_id,omitempty"` // when it came in over OCPI
	ChargePointID     string    `json:"charge_point_id"`
	EVSEUID           string    `json:"evse_uid"`
	IDTag             string    `json:"id_tag"`
	ExpiresAt         time.Time `json:"expires_at"`
	CreatedAt         time.Time `json:"created_at"`
	Source            string    `json:"source"`                 // "cli", "tui" or "ocpi"
	ResponseURL       string    `json:"response_url,omitempty"` // OCPI CommandResult target
	// ResponseRegistration is the registered_at of the partner registration
	// ResponseURL belongs to; results go there only while it is still current.
	ResponseRegistration time.Time `json:"response_registration,omitzero"`
}

// Transaction is an active charging transaction.
type Transaction struct {
	// ID is a string because OCPP 2.0.1 transaction ids are strings; 1.6's
	// integers are stored in decimal.
	ID            string    `json:"id"`
	ChargePointID string    `json:"charge_point_id"`
	EVSEUID       string    `json:"evse_uid"`
	ConnectorID   int       `json:"connector_id"`
	IDTag         string    `json:"id_tag"`
	MeterStart    int       `json:"meter_start"`
	StartedAt     time.Time `json:"started_at"`
	ReservationID int       `json:"reservation_id,omitempty"`
}

// ChargingProfile is a charging profile the station accepted, as cpms sent
// it.
type ChargingProfile struct {
	ID            int    `json:"id"`
	ChargePointID string `json:"charge_point_id"`
	// EVSEUID is "" for a profile on the whole station (connector 0 in 1.6).
	EVSEUID    string `json:"evse_uid"`
	Purpose    string `json:"purpose"` // "StationMax", "TxDefault" or "Tx"
	StackLevel int    `json:"stack_level"`
	Unit       string `json:"unit"` // "W" or "A"
	// Periods are the schedule's steps, as offsets from its start.
	Periods   []ChargingPeriod `json:"periods"`
	ValidFrom *time.Time       `json:"valid_from,omitempty"`
	ValidTo   *time.Time       `json:"valid_to,omitempty"`
	// TransactionID is set for a Tx profile: the session it ends with.
	TransactionID string    `json:"transaction_id,omitempty"`
	SetAt         time.Time `json:"set_at"`
	Source        string    `json:"source"` // "cli", "tui" or "ocpi"
}

// ChargingPeriod is one step of a recorded schedule.
type ChargingPeriod struct {
	StartSeconds int     `json:"start_s"`
	Limit        float64 `json:"limit"`
	Phases       *int    `json:"phases,omitempty"`
}

// empty is the state of a tool that has never written anything.
func empty() State {
	return State{
		Version: Version, Reservations: []Reservation{}, Transactions: []Transaction{},
		ChargingProfiles: []ChargingProfile{},
	}
}

// clone returns a deep copy, so no caller can reach the store's own slices.
func (s State) clone() State {
	out := s
	out.Reservations = append([]Reservation{}, s.Reservations...)
	out.Transactions = append([]Transaction{}, s.Transactions...)
	out.ChargingProfiles = make([]ChargingProfile, len(s.ChargingProfiles))
	for i, p := range s.ChargingProfiles {
		out.ChargingProfiles[i] = p.clone()
	}
	if s.OCPI != nil {
		reg := *s.OCPI
		reg.Endpoints = append([]Endpoint(nil), s.OCPI.Endpoints...)
		reg.Roles = append([]PartyRole(nil), s.OCPI.Roles...)
		out.OCPI = &reg
	}
	return out
}

// normalize puts a state into the one form the store keeps and writes: the
// current version, empty lists rather than null, and every timestamp in UTC.
// UTC also strips the monotonic clock reading, which JSON does not carry, so
// what is in memory compares equal to what a later Open reads back.
func (s *State) normalize() {
	s.Version = Version
	if s.Reservations == nil {
		s.Reservations = []Reservation{}
	}
	if s.Transactions == nil {
		s.Transactions = []Transaction{}
	}
	if s.ChargingProfiles == nil {
		s.ChargingProfiles = []ChargingProfile{}
	}
	for i := range s.ChargingProfiles {
		p := &s.ChargingProfiles[i]
		p.SetAt = p.SetAt.UTC()
		p.ValidFrom = utcPtr(p.ValidFrom)
		p.ValidTo = utcPtr(p.ValidTo)
		if p.Periods == nil {
			p.Periods = []ChargingPeriod{}
		}
	}
	for i := range s.Reservations {
		r := &s.Reservations[i]
		r.ExpiresAt = r.ExpiresAt.UTC()
		r.CreatedAt = r.CreatedAt.UTC()
		r.ResponseRegistration = r.ResponseRegistration.UTC()
	}
	for i := range s.Transactions {
		s.Transactions[i].StartedAt = s.Transactions[i].StartedAt.UTC()
	}
	if s.OCPI != nil {
		s.OCPI.RegisteredAt = s.OCPI.RegisteredAt.UTC()
	}
}

// clone deep-copies a profile: its periods and pointers.
func (p ChargingProfile) clone() ChargingProfile {
	out := p
	out.Periods = make([]ChargingPeriod, len(p.Periods))
	for i, period := range p.Periods {
		out.Periods[i] = period
		if period.Phases != nil {
			n := *period.Phases
			out.Periods[i].Phases = &n
		}
	}
	out.ValidFrom = utcPtr(p.ValidFrom)
	out.ValidTo = utcPtr(p.ValidTo)
	return out
}

// utcPtr copies a time pointer, in UTC.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
