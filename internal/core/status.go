package core

// EVSEStatus is the status of one EVSE.
//
// The values are OCPI 2.3.0's Status enum verbatim. Carrying the OCPI
// vocabulary in the domain model means the Locations module renders it
// directly, and the translation from a protocol's own status set happens once,
// in that protocol's adapter, where the version-specific knowledge belongs.
type EVSEStatus string

// The OCPI 2.3.0 Status values. PLANNED and REMOVED describe EVSEs that are
// not yet or no longer installed, so nothing in cpms produces them; they are
// listed for completeness of the enum.
const (
	StatusAvailable   EVSEStatus = "AVAILABLE"
	StatusBlocked     EVSEStatus = "BLOCKED"
	StatusCharging    EVSEStatus = "CHARGING"
	StatusInoperative EVSEStatus = "INOPERATIVE"
	StatusOutOfOrder  EVSEStatus = "OUTOFORDER"
	StatusPlanned     EVSEStatus = "PLANNED"
	StatusRemoved     EVSEStatus = "REMOVED"
	StatusReserved    EVSEStatus = "RESERVED"
	StatusUnknown     EVSEStatus = "UNKNOWN"
)

// effectiveStatus combines what we know about a station with what we know
// about one of its EVSEs.
//
// Two rules, both of which exist to stop cpms advertising a connector as
// usable when it is not:
//
//   - A station we cannot reach tells us nothing about its connectors, so
//     everything reads UNKNOWN rather than whatever was last seen.
//   - OCPP 1.6 addresses the station as a whole with connector 0. A station
//     reporting itself faulted or unavailable overrides the per-connector
//     status, which the charger may never bother to update.
func effectiveStatus(online bool, station, connector EVSEStatus) EVSEStatus {
	if !online {
		return StatusUnknown
	}
	switch station {
	case StatusOutOfOrder, StatusInoperative:
		return station
	}
	if connector == "" {
		return StatusUnknown
	}
	return connector
}
