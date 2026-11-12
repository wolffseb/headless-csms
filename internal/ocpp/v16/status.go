package v16

import (
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

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
// Three choices are worth explaining, because OCPI has a coarser vocabulary
// than OCPP here:
//
//   - Finishing is BLOCKED. OCPP 1.6 §7.7 defines it as a stopped transaction
//     whose connector is "not yet available for a new user" (cable still in,
//     vehicle still in the bay), and OCPI 2.3 AVAILABLE means "able to start a
//     new charging session". BLOCKED ("not accessible because of a physical
//     barrier, i.e. a car") is the state for that; AVAILABLE would send the
//     next driver to an occupied bay.
//   - Preparing stays AVAILABLE, a deliberate deviation from the letter of
//     §7.7 ("no longer available for a new user"). Drivers plug in first and
//     start from the app, so a roaming partner must still be able to route
//     someone to a connector that is Preparing. Common practice agrees (SAP
//     ev-server maps Preparing to AVAILABLE). The cost: a car that is plugged
//     in and abandoned shows AVAILABLE, and the start is refused by the
//     station.
//   - SuspendedEV and SuspendedEVSE are pauses inside a running session, not
//     the end of one, so they stay CHARGING rather than flapping the EVSE back
//     to AVAILABLE mid-session and confusing a roaming partner.
var statusMap = map[string]core.EVSEStatus{
	StatusAvailable:     core.StatusAvailable,
	StatusPreparing:     core.StatusAvailable,
	StatusFinishing:     core.StatusBlocked,
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

// commandStatusMap lists, per action, every status the 1.6 spec defines for
// its .conf. A value is only mapped for the action that defines it: "Unlocked"
// in answer to ReserveNow is as unknown as a string no action uses.
var commandStatusMap = map[string]map[string]ocpp.CommandStatus{
	// ReservationStatus.
	ActionReserveNow: {
		ReservationAccepted:    ocpp.CommandAccepted,
		ReservationFaulted:     ocpp.CommandFaulted,
		ReservationOccupied:    ocpp.CommandOccupied,
		ReservationRejected:    ocpp.CommandRejected,
		ReservationUnavailable: ocpp.CommandUnavailable,
	},
	// CancelReservationStatus.
	ActionCancelReservation: {
		CmdAccepted: ocpp.CommandAccepted,
		CmdRejected: ocpp.CommandRejected,
	},
	// RemoteStartStopStatus, for both directions.
	ActionRemoteStartTransaction: {
		CmdAccepted: ocpp.CommandAccepted,
		CmdRejected: ocpp.CommandRejected,
	},
	ActionRemoteStopTransaction: {
		CmdAccepted: ocpp.CommandAccepted,
		CmdRejected: ocpp.CommandRejected,
	},
	// UnlockStatus.
	ActionUnlockConnector: {
		UnlockUnlocked:     ocpp.CommandUnlocked,
		UnlockFailed:       ocpp.CommandUnlockFailed,
		UnlockNotSupported: ocpp.CommandNotSupported,
	},
	// TriggerMessageStatus.
	ActionTriggerMessage: {
		TriggerAccepted:       ocpp.CommandAccepted,
		TriggerRejected:       ocpp.CommandRejected,
		TriggerNotImplemented: ocpp.CommandNotImplemented,
	},
}

// MapCommandStatus converts the status in an action's .conf. Anything the
// spec does not define for that action is CommandUnknown, never a guess.
func MapCommandStatus(action, status string) ocpp.CommandStatus {
	if mapped, ok := commandStatusMap[action][status]; ok {
		return mapped
	}
	return ocpp.CommandUnknown
}
