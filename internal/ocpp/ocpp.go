// Package ocpp holds the parts of OCPP that do not depend on a protocol
// version: the RPC error vocabulary, the version/subprotocol names, and the
// Handler interface that a version adapter implements.
//
// The transport in internal/ocpp/csms never learns a message name. It routes
// (charge point, action, raw payload) to the Handler chosen by the negotiated
// WebSocket subprotocol, which is what lets OCPP 2.0.1 arrive later as a new
// package beside internal/ocpp/v16 rather than as a rewrite.
package ocpp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Version is an OCPP protocol version.
type Version string

// The versions cpms knows about.
const (
	Version16  Version = "1.6"
	Version201 Version = "2.0.1"
)

// Subprotocol is the WebSocket subprotocol token for this version, as used in
// the Sec-WebSocket-Protocol header.
func (v Version) Subprotocol() string { return "ocpp" + string(v) }

// VersionFromSubprotocol maps a negotiated subprotocol back to a Version.
func VersionFromSubprotocol(s string) (Version, bool) {
	for _, v := range []Version{Version16, Version201} {
		if v.Subprotocol() == s {
			return v, true
		}
	}
	return "", false
}

// ErrorCode is an OCPP-J CALLERROR code.
type ErrorCode string

// The error codes defined by OCPP-J 1.6.
//
// The distinction between RpcFrameworkError and FormationViolation matters for
// interoperability and is easy to get wrong: the first says "this is not a
// valid RPC frame at all", the second says "this is a valid frame whose
// payload does not match the action".
const (
	// ErrRpcFrameworkError means the content is not a valid RPC request — bad
	// JSON, not an array, unreadable message id.
	ErrRpcFrameworkError ErrorCode = "RpcFrameworkError"
	// ErrNotImplemented means the action is not known to us.
	ErrNotImplemented ErrorCode = "NotImplemented"
	// ErrNotSupported means the action is known but we do not implement it.
	ErrNotSupported ErrorCode = "NotSupported"
	// ErrInternalError means we failed to process an otherwise valid request.
	ErrInternalError ErrorCode = "InternalError"
	// ErrProtocolError means the payload is incomplete.
	ErrProtocolError ErrorCode = "ProtocolError"
	// ErrSecurityError means a security issue prevented processing.
	ErrSecurityError ErrorCode = "SecurityError"
	// ErrFormationViolation means the payload does not conform to the PDU
	// structure for the action.
	ErrFormationViolation ErrorCode = "FormationViolation"
	// ErrPropertyConstraintViolation means a field holds an invalid value.
	ErrPropertyConstraintViolation ErrorCode = "PropertyConstraintViolation"
	// ErrOccurrenceConstraintViolation means a field violates occurrence
	// constraints.
	//
	// The wire value below is spelled with a single "r" in the middle because
	// that is how the OCPP specification spells it. It is not a typo to
	// correct: peers compare this string literally, so matching the spec beats
	// matching English.
	ErrOccurrenceConstraintViolation ErrorCode = "OccurenceConstraintViolation"
	// ErrTypeConstraintViolation means a field violates a data type constraint.
	ErrTypeConstraintViolation ErrorCode = "TypeConstraintViolation"
	// ErrGenericError is anything not covered by the codes above.
	ErrGenericError ErrorCode = "GenericError"
)

// RPCError is an error to be returned to the peer as a CALLERROR.
type RPCError struct {
	Code        ErrorCode
	Description string
	Details     map[string]any
}

func (e *RPCError) Error() string {
	if e.Description == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Description
}

// Errorf builds an RPCError with a formatted description.
func Errorf(code ErrorCode, format string, args ...any) *RPCError {
	return &RPCError{Code: code, Description: fmt.Sprintf(format, args...)}
}

// Handler processes inbound calls for one protocol version.
//
// Returning a nil *RPCError means success and the first value is marshalled as
// the CALLRESULT payload. A non-nil *RPCError becomes a CALLERROR.
type Handler interface {
	// Version reports which OCPP version this handler speaks.
	Version() Version

	// HandleCall processes one inbound CALL.
	HandleCall(ctx context.Context, chargePointID, action string, payload json.RawMessage) (any, *RPCError)
}
