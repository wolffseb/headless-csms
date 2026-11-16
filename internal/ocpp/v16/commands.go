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
	ActionChangeConfiguration    = "ChangeConfiguration"
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
	KeySupportedFeatureProfiles      = "SupportedFeatureProfiles"
	KeyHeartbeatInterval             = "HeartbeatInterval"
	KeyNumberOfConnectors            = "NumberOfConnectors"
	KeyReserveConnectorZeroSupported = "ReserveConnectorZeroSupported"
	KeyAuthorizeRemoteTxRequests     = "AuthorizeRemoteTxRequests"
	KeyLocalAuthListEnabled          = "LocalAuthListEnabled"
	KeyLocalPreAuthorize             = "LocalPreAuthorize"
	KeyLocalAuthorizeOffline         = "LocalAuthorizeOffline"
	KeyConnectionTimeOut             = "ConnectionTimeOut"
	KeyMeterValueSampleInterval      = "MeterValueSampleInterval"
	KeyMeterValuesSampledData        = "MeterValuesSampledData"

	// KeySupportedFeatureProfilesMaxLength caps how many profiles the
	// station can list in SupportedFeatureProfiles, so a list that long may
	// have been cut short.
	KeySupportedFeatureProfilesMaxLength = "SupportedFeatureProfilesMaxLength"

	// The Smart Charging profile's own keys.
	KeyChargingScheduleAllowedChargingRateUnit = "ChargingScheduleAllowedChargingRateUnit"
	KeyChargeProfileMaxStackLevel              = "ChargeProfileMaxStackLevel"
	KeyChargingScheduleMaxPeriods              = "ChargingScheduleMaxPeriods"
	KeyMaxChargingProfilesInstalled            = "MaxChargingProfilesInstalled"
)

// ChangeConfigurationReq sets one configuration key.
type ChangeConfigurationReq struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ConfigurationStatus values for ChangeConfiguration.conf.
const (
	ConfigAccepted       = "Accepted"
	ConfigRejected       = "Rejected"
	ConfigRebootRequired = "RebootRequired"
	ConfigNotSupported   = "NotSupported"
)

// ChangeConfigurationConf answers a ChangeConfigurationReq.
type ChangeConfigurationConf struct {
	Status string `json:"status"`
}

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

// Action names for the Smart Charging profile.
const (
	ActionSetChargingProfile   = "SetChargingProfile"
	ActionClearChargingProfile = "ClearChargingProfile"
	ActionGetCompositeSchedule = "GetCompositeSchedule"
)

// ChargingProfilePurposeType values.
const (
	PurposeChargePointMax = "ChargePointMaxProfile"
	PurposeTxDefault      = "TxDefaultProfile"
	PurposeTx             = "TxProfile"
)

// ChargingProfileKindType values.
const (
	KindAbsolute  = "Absolute"
	KindRecurring = "Recurring"
	KindRelative  = "Relative"
)

// RecurrencyKindType values.
const (
	RecurrencyDaily  = "Daily"
	RecurrencyWeekly = "Weekly"
)

// ChargingRateUnitType values.
const (
	UnitA = "A"
	UnitW = "W"
)

// ChargingSchedulePeriod is one step of a schedule. StartPeriod is seconds
// from the schedule's start.
//
// Limit is a plain float64: encoding/json writes it without an exponent for
// every magnitude a charger can see (it switches only from 1e21), so 20000
// goes out as 20000 and 16.5 as 16.5.
type ChargingSchedulePeriod struct {
	StartPeriod  int     `json:"startPeriod"`
	Limit        float64 `json:"limit"`
	NumberPhases *int    `json:"numberPhases,omitempty"`
}

// ChargingSchedule is the schedule inside a charging profile, and the
// composite one GetCompositeSchedule answers with.
type ChargingSchedule struct {
	Duration               *int                     `json:"duration,omitempty"`
	StartSchedule          string                   `json:"startSchedule,omitempty"`
	ChargingRateUnit       string                   `json:"chargingRateUnit"`
	ChargingSchedulePeriod []ChargingSchedulePeriod `json:"chargingSchedulePeriod"`
	MinChargingRate        *float64                 `json:"minChargingRate,omitempty"`
}

// ChargingProfile is csChargingProfiles in SetChargingProfile.req.
type ChargingProfile struct {
	ChargingProfileID      int              `json:"chargingProfileId"`
	TransactionID          *int             `json:"transactionId,omitempty"`
	StackLevel             int              `json:"stackLevel"`
	ChargingProfilePurpose string           `json:"chargingProfilePurpose"`
	ChargingProfileKind    string           `json:"chargingProfileKind"`
	RecurrencyKind         string           `json:"recurrencyKind,omitempty"`
	ValidFrom              string           `json:"validFrom,omitempty"`
	ValidTo                string           `json:"validTo,omitempty"`
	ChargingSchedule       ChargingSchedule `json:"chargingSchedule"`
}

// SetChargingProfileReq installs a profile on a connector, or on the whole
// station with connector 0.
type SetChargingProfileReq struct {
	ConnectorID        int             `json:"connectorId"`
	CSChargingProfiles ChargingProfile `json:"csChargingProfiles"`
}

// ChargingProfileStatus values for SetChargingProfile.conf.
const (
	ProfileAccepted     = "Accepted"
	ProfileRejected     = "Rejected"
	ProfileNotSupported = "NotSupported"
)

// SetChargingProfileConf answers a SetChargingProfileReq.
type SetChargingProfileConf struct {
	Status string `json:"status"`
}

// ClearChargingProfileReq removes the profiles matching every field that is
// set. With no field set it removes them all.
type ClearChargingProfileReq struct {
	ID                     *int   `json:"id,omitempty"`
	ConnectorID            *int   `json:"connectorId,omitempty"`
	ChargingProfilePurpose string `json:"chargingProfilePurpose,omitempty"`
	StackLevel             *int   `json:"stackLevel,omitempty"`
}

// ClearChargingProfileStatus values. Unknown is a defined answer here: no
// profile matched.
const (
	ClearAccepted = "Accepted"
	ClearUnknown  = "Unknown"
)

// ClearChargingProfileConf answers a ClearChargingProfileReq.
type ClearChargingProfileConf struct {
	Status string `json:"status"`
}

// GetCompositeScheduleReq asks for the limits a connector (0: the station)
// will see over the next Duration seconds.
type GetCompositeScheduleReq struct {
	ConnectorID      int    `json:"connectorId"`
	Duration         int    `json:"duration"`
	ChargingRateUnit string `json:"chargingRateUnit,omitempty"`
}

// GetCompositeScheduleStatus values.
const (
	CompositeAccepted = "Accepted"
	CompositeRejected = "Rejected"
)

// GetCompositeScheduleConf answers a GetCompositeScheduleReq. Everything but
// the status is optional, and absent when the station rejects the request.
type GetCompositeScheduleConf struct {
	Status           string            `json:"status"`
	ConnectorID      *int              `json:"connectorId,omitempty"`
	ScheduleStart    string            `json:"scheduleStart,omitempty"`
	ChargingSchedule *ChargingSchedule `json:"chargingSchedule,omitempty"`
}
