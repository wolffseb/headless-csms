package v16

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// The OCPP 1.6-J action names we handle.
const (
	ActionBootNotification              = "BootNotification"
	ActionHeartbeat                     = "Heartbeat"
	ActionStatusNotification            = "StatusNotification"
	ActionAuthorize                     = "Authorize"
	ActionStartTransaction              = "StartTransaction"
	ActionStopTransaction               = "StopTransaction"
	ActionMeterValues                   = "MeterValues"
	ActionDataTransfer                  = "DataTransfer"
	ActionFirmwareStatusNotification    = "FirmwareStatusNotification"
	ActionDiagnosticsStatusNotification = "DiagnosticsStatusNotification"
)

// decode unmarshals a request payload.
//
// Unknown fields are tolerated on purpose: chargers routinely add vendor
// extensions, and rejecting a BootNotification because of one extra key would
// be a spectacular own goal.
func decode[T any](payload json.RawMessage) (T, *ocpp.RPCError) {
	var v T
	if len(payload) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, ocpp.Errorf(ocpp.ErrFormationViolation, "payload does not match the action: %v", err)
	}
	return v, nil
}

// missing reports a required field that was not sent. The OCPP specification
// calls this an occurrence-constraint violation, as distinct from a field that
// is present but holds a bad value.
func missing(field string) *ocpp.RPCError {
	return ocpp.Errorf(ocpp.ErrOccurrenceConstraintViolation, "%s is required", field)
}

// badValue reports a field that is present but invalid.
func badValue(field, value string) *ocpp.RPCError {
	return ocpp.Errorf(ocpp.ErrPropertyConstraintViolation, "%s has invalid value %q", field, value)
}

// IDTagInfo is the authorisation result embedded in several responses.
type IDTagInfo struct {
	Status      string `json:"status"`
	ExpiryDate  string `json:"expiryDate,omitempty"`
	ParentIDTag string `json:"parentIdTag,omitempty"`
}

// Authorisation statuses (1.6 AuthorizationStatus).
const (
	AuthAccepted = "Accepted"
	AuthInvalid  = "Invalid"
)

// BootNotificationReq is sent by the charge point when it starts up.
type BootNotificationReq struct {
	ChargePointVendor       string `json:"chargePointVendor"`
	ChargePointModel        string `json:"chargePointModel"`
	ChargePointSerialNumber string `json:"chargePointSerialNumber,omitempty"`
	ChargeBoxSerialNumber   string `json:"chargeBoxSerialNumber,omitempty"`
	FirmwareVersion         string `json:"firmwareVersion,omitempty"`
	ICCID                   string `json:"iccid,omitempty"`
	IMSI                    string `json:"imsi,omitempty"`
	MeterType               string `json:"meterType,omitempty"`
	MeterSerialNumber       string `json:"meterSerialNumber,omitempty"`
}

func (r BootNotificationReq) validate() *ocpp.RPCError {
	if r.ChargePointVendor == "" {
		return missing("chargePointVendor")
	}
	if r.ChargePointModel == "" {
		return missing("chargePointModel")
	}
	return nil
}

// BootNotificationConf accepts the charge point and tells it how often to
// check in.
type BootNotificationConf struct {
	Status      string `json:"status"`
	CurrentTime string `json:"currentTime"`
	Interval    int    `json:"interval"`
}

// HeartbeatConf answers a Heartbeat.
type HeartbeatConf struct {
	CurrentTime string `json:"currentTime"`
}

// StatusNotificationReq reports the state of one connector, or of the station
// as a whole when ConnectorID is 0.
type StatusNotificationReq struct {
	ConnectorID     int    `json:"connectorId"`
	ErrorCode       string `json:"errorCode"`
	Status          string `json:"status"`
	Info            string `json:"info,omitempty"`
	Timestamp       string `json:"timestamp,omitempty"`
	VendorID        string `json:"vendorId,omitempty"`
	VendorErrorCode string `json:"vendorErrorCode,omitempty"`
}

func (r StatusNotificationReq) validate() *ocpp.RPCError {
	if r.ConnectorID < 0 {
		return badValue("connectorId", itoa(r.ConnectorID))
	}
	if r.Status == "" {
		return missing("status")
	}
	if _, ok := statusMap[r.Status]; !ok {
		return badValue("status", r.Status)
	}
	return nil
}

// AuthorizeReq asks whether an RFID tag may charge.
type AuthorizeReq struct {
	IDTag string `json:"idTag"`
}

// AuthorizeConf answers an AuthorizeReq.
type AuthorizeConf struct {
	IDTagInfo IDTagInfo `json:"idTagInfo"`
}

// StartTransactionReq opens a charging session.
type StartTransactionReq struct {
	ConnectorID   int    `json:"connectorId"`
	IDTag         string `json:"idTag"`
	MeterStart    int    `json:"meterStart"`
	Timestamp     string `json:"timestamp"`
	ReservationID *int   `json:"reservationId,omitempty"`
}

func (r StartTransactionReq) validate() *ocpp.RPCError {
	// Connector 0 addresses the station, so it cannot host a transaction.
	if r.ConnectorID < 1 {
		return badValue("connectorId", itoa(r.ConnectorID))
	}
	if r.IDTag == "" {
		return missing("idTag")
	}
	return nil
}

// StartTransactionConf hands back the transaction id we assigned.
type StartTransactionConf struct {
	TransactionID int       `json:"transactionId"`
	IDTagInfo     IDTagInfo `json:"idTagInfo"`
}

// StopTransactionReq closes a charging session.
type StopTransactionReq struct {
	TransactionID int    `json:"transactionId"`
	MeterStop     int    `json:"meterStop"`
	Timestamp     string `json:"timestamp"`
	IDTag         string `json:"idTag,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// StopTransactionConf answers a StopTransactionReq.
type StopTransactionConf struct {
	IDTagInfo *IDTagInfo `json:"idTagInfo,omitempty"`
}

// MeterValuesReq carries meter readings.
type MeterValuesReq struct {
	ConnectorID   int          `json:"connectorId"`
	TransactionID *int         `json:"transactionId,omitempty"`
	MeterValue    []MeterValue `json:"meterValue"`
}

// MeterValue is one timestamped set of sampled values.
type MeterValue struct {
	Timestamp    string         `json:"timestamp"`
	SampledValue []SampledValue `json:"sampledValue"`
}

// SampledValue is a single reading.
type SampledValue struct {
	Value     string `json:"value"`
	Measurand string `json:"measurand,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Context   string `json:"context,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Location  string `json:"location,omitempty"`
	Format    string `json:"format,omitempty"`
}

// DataTransferReq is the vendor extension escape hatch.
type DataTransferReq struct {
	VendorID  string `json:"vendorId"`
	MessageID string `json:"messageId,omitempty"`
	Data      string `json:"data,omitempty"`
}

// DataTransferConf answers a DataTransferReq.
type DataTransferConf struct {
	Status string `json:"status"`
	Data   string `json:"data,omitempty"`
}

// StatusOnlyReq covers the notifications that carry nothing but a status.
type StatusOnlyReq struct {
	Status string `json:"status"`
}

// emptyConf is the response for messages that carry no data back.
type emptyConf struct{}

// formatTime renders a timestamp the way OCPP expects: ISO 8601 in UTC.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseTime reads an OCPP timestamp, falling back to the given default when
// the charger omitted or mangled it. A bad clock on the charger is not worth
// rejecting a transaction over.
func parseTime(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fallback
	}
	return t
}

func itoa(i int) string { return strconv.Itoa(i) }
