package v16

// The CSMS-to-charge-point messages: the commands that drive a station rather
// than report on it.
//
// They live here beside the inbound messages because both directions are part
// of the same protocol version. The simulator answers them today; the CSMS
// command layer sends them.

// Action names for the outbound commands.
const (
	ActionReserveNow             = "ReserveNow"
	ActionCancelReservation      = "CancelReservation"
	ActionRemoteStartTransaction = "RemoteStartTransaction"
	ActionRemoteStopTransaction  = "RemoteStopTransaction"
	ActionUnlockConnector        = "UnlockConnector"
	ActionTriggerMessage         = "TriggerMessage"
	ActionGetConfiguration       = "GetConfiguration"
	ActionChangeAvailability     = "ChangeAvailability"
	ActionReset                  = "Reset"
)

// Generic command statuses. OCPP 1.6 reuses "Accepted"/"Rejected" across most
// of these, with a few commands adding their own values.
const (
	CmdAccepted  = "Accepted"
	CmdRejected  = "Rejected"
	CmdScheduled = "Scheduled"
)

// ReserveNowReq asks the station to hold a connector for a token.
type ReserveNowReq struct {
	ConnectorID   int    `json:"connectorId"`
	ExpiryDate    string `json:"expiryDate"`
	IDTag         string `json:"idTag"`
	ParentIDTag   string `json:"parentIdTag,omitempty"`
	ReservationID int    `json:"reservationId"`
}

// ReservationStatus values for ReserveNow.conf.
const (
	ReservationAccepted    = "Accepted"
	ReservationFaulted     = "Faulted"
	ReservationOccupied    = "Occupied"
	ReservationRejected    = "Rejected"
	ReservationUnavailable = "Unavailable"
)

// ReserveNowConf answers a ReserveNowReq.
type ReserveNowConf struct {
	Status string `json:"status"`
}

// CancelReservationReq releases a reservation by id.
type CancelReservationReq struct {
	ReservationID int `json:"reservationId"`
}

// CancelReservationConf answers a CancelReservationReq.
type CancelReservationConf struct {
	Status string `json:"status"`
}

// RemoteStartTransactionReq starts a session on behalf of a token. ConnectorID
// is optional in the spec: omitting it lets the station choose.
type RemoteStartTransactionReq struct {
	ConnectorID *int   `json:"connectorId,omitempty"`
	IDTag       string `json:"idTag"`
}

// RemoteStartTransactionConf answers a RemoteStartTransactionReq. It only says
// whether the station accepted the request; the session itself is reported
// afterwards by StartTransaction.
type RemoteStartTransactionConf struct {
	Status string `json:"status"`
}

// RemoteStopTransactionReq stops a running session by transaction id.
type RemoteStopTransactionReq struct {
	TransactionID int `json:"transactionId"`
}

// RemoteStopTransactionConf answers a RemoteStopTransactionReq.
type RemoteStopTransactionConf struct {
	Status string `json:"status"`
}

// UnlockConnectorReq releases the cable lock. It carries no token: unlocking
// is a mechanical action, not an authorisation.
type UnlockConnectorReq struct {
	ConnectorID int `json:"connectorId"`
}

// UnlockStatus values for UnlockConnector.conf.
const (
	UnlockUnlocked     = "Unlocked"
	UnlockFailed       = "UnlockFailed"
	UnlockNotSupported = "NotSupported"
)

// UnlockConnectorConf answers an UnlockConnectorReq.
type UnlockConnectorConf struct {
	Status string `json:"status"`
}

// TriggerMessageReq asks the station to send a message now.
type TriggerMessageReq struct {
	RequestedMessage string `json:"requestedMessage"`
	ConnectorID      *int   `json:"connectorId,omitempty"`
}

// TriggerMessageStatus values.
const (
	TriggerAccepted       = "Accepted"
	TriggerRejected       = "Rejected"
	TriggerNotImplemented = "NotImplemented"
)

// TriggerMessageConf answers a TriggerMessageReq.
type TriggerMessageConf struct {
	Status string `json:"status"`
}

// GetConfigurationReq reads configuration keys. An empty Key list means "all".
type GetConfigurationReq struct {
	Key []string `json:"key,omitempty"`
}

// ConfigurationKey is one configuration entry.
type ConfigurationKey struct {
	Key      string `json:"key"`
	Readonly bool   `json:"readonly"`
	Value    string `json:"value,omitempty"`
}

// GetConfigurationConf answers a GetConfigurationReq. SupportedFeatureProfiles
// is the interesting key: it is how we find out whether a station implements
// the Reservation profile at all.
type GetConfigurationConf struct {
	ConfigurationKey []ConfigurationKey `json:"configurationKey,omitempty"`
	UnknownKey       []string           `json:"unknownKey,omitempty"`
}

// Configuration keys cpms cares about.
const (
	KeySupportedFeatureProfiles = "SupportedFeatureProfiles"
	KeyHeartbeatInterval        = "HeartbeatInterval"
	KeyNumberOfConnectors       = "NumberOfConnectors"
)

// ChangeAvailabilityReq takes a connector, or the whole station with
// connector 0, in or out of service.
type ChangeAvailabilityReq struct {
	ConnectorID int    `json:"connectorId"`
	Type        string `json:"type"`
}

// Availability types.
const (
	AvailabilityInoperative = "Inoperative"
	AvailabilityOperative   = "Operative"
)

// ChangeAvailabilityConf answers a ChangeAvailabilityReq.
type ChangeAvailabilityConf struct {
	Status string `json:"status"`
}

// ResetReq restarts the station.
type ResetReq struct {
	Type string `json:"type"`
}

// Reset types.
const (
	ResetHard = "Hard"
	ResetSoft = "Soft"
)

// ResetConf answers a ResetReq.
type ResetConf struct {
	Status string `json:"status"`
}
