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
