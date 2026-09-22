package v16

import "github.com/wolffseb/cli-cpms/internal/core"

// The OCPP 1.6 ChargePointStatus values.
const (
	StatusAvailable     = "Available"
	StatusPreparing     = "Preparing"
	StatusCharging      = "Charging"
	StatusSuspendedEVSE = "SuspendedEVSE"
	StatusSuspendedEV   = "SuspendedEV"
	StatusFinishing     = "Finishing"
	StatusReserved      = "Reserved"
	StatusUnavailable   = "Unavailable"
	StatusFaulted       = "Faulted"
)

// statusMap translates OCPP 1.6 connector status to the OCPI 2.3 status the
// domain model speaks.
//
// Two groupings are worth explaining, because OCPI has a coarser vocabulary
// than OCPP here:
//
//   - Preparing and Finishing both mean "a car is at the connector but no
//     energy is flowing". OCPI 2.3 has no state for that, and AVAILABLE is the
//     honest answer: the connector can still be started.
//   - SuspendedEV and SuspendedEVSE are pauses inside a running session, not
//     the end of one, so they stay CHARGING rather than flapping the EVSE back
//     to AVAILABLE mid-session and confusing a roaming partner.
var statusMap = map[string]core.EVSEStatus{
	StatusAvailable:     core.StatusAvailable,
	StatusPreparing:     core.StatusAvailable,
	StatusFinishing:     core.StatusAvailable,
	StatusCharging:      core.StatusCharging,
	StatusSuspendedEV:   core.StatusCharging,
	StatusSuspendedEVSE: core.StatusCharging,
	StatusReserved:      core.StatusReserved,
	StatusUnavailable:   core.StatusInoperative,
	StatusFaulted:       core.StatusOutOfOrder,
}

// MapStatus converts an OCPP 1.6 connector status. Unknown values map to
// UNKNOWN rather than being guessed at.
func MapStatus(s string) core.EVSEStatus {
	if mapped, ok := statusMap[s]; ok {
		return mapped
	}
	return core.StatusUnknown
}
