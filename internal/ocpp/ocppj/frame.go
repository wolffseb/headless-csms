package ocppj

import (
	"encoding/json"
	"fmt"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// MessageType is the first element of an OCPP-J array, saying what kind of
// message it is.
//
// OCPP-J wraps every message in a JSON array:
//
//	CALL        [2, messageId, action, payload]
//	CALLRESULT  [3, messageId, payload]
//	CALLERROR   [4, messageId, errorCode, errorDescription, errorDetails]
type MessageType int

// The OCPP-J message type ids.
const (
	MessageTypeCall       MessageType = 2
	MessageTypeCallResult MessageType = 3
	MessageTypeCallError  MessageType = 4
)

// UnknownMessageID is what we put in a CALLERROR when the incoming frame was
// so malformed that its message id could not be recovered. OCPP-J requires
// some id to be present, and the peer has no pending call to match it to
// anyway.
const UnknownMessageID = "-1"

// Frame is a parsed inbound OCPP-J message.
type Frame struct {
	Type MessageType
	ID   string

	// Action and Payload are set for CALL; Payload alone for CALLRESULT.
	Action  string
	Payload json.RawMessage

	// Set for CALLERROR.
	ErrorCode        ocpp.ErrorCode
	ErrorDescription string
	ErrorDetails     json.RawMessage
}

// ParseFrame decodes one inbound message.
//
// On failure it returns the message id to answer with — recovered from the
// frame when possible, UnknownMessageID when not — and the RPCError to send.
// Everything that is not a well-formed RPC frame is RpcFrameworkError;
// FormationViolation is reserved for a valid frame whose payload does not fit
// the action, which only the version handler can judge.
func ParseFrame(data []byte) (Frame, string, *ocpp.RPCError) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Frame{}, UnknownMessageID, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
			"message is not a JSON array: %v", err)
	}
	if len(raw) < 3 {
		return Frame{}, UnknownMessageID, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
			"message has %d elements, want at least 3", len(raw))
	}

	var typ int
	if err := json.Unmarshal(raw[0], &typ); err != nil {
		return Frame{}, UnknownMessageID, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
			"message type id is not a number")
	}

	// The message id is read before validating the type so that a frame with a
	// bad type can still be answered against the right id.
	var id string
	if err := json.Unmarshal(raw[1], &id); err != nil {
		return Frame{}, UnknownMessageID, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
			"message id is not a string")
	}
	if id == "" {
		return Frame{}, UnknownMessageID, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
			"message id is empty")
	}

	f := Frame{Type: MessageType(typ), ID: id}

	switch f.Type {
	case MessageTypeCall:
		if len(raw) != 4 {
			return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
				"CALL has %d elements, want 4", len(raw))
		}
		if err := json.Unmarshal(raw[2], &f.Action); err != nil {
			return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError, "action is not a string")
		}
		if f.Action == "" {
			return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError, "action is empty")
		}
		f.Payload = raw[3]

	case MessageTypeCallResult:
		if len(raw) != 3 {
			return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
				"CALLRESULT has %d elements, want 3", len(raw))
		}
		f.Payload = raw[2]

	case MessageTypeCallError:
		// The spec says five elements. Some stacks omit the details object, so
		// we accept four rather than failing a response we can understand.
		if len(raw) < 4 {
			return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
				"CALLERROR has %d elements, want at least 4", len(raw))
		}
		var code string
		if err := json.Unmarshal(raw[2], &code); err != nil {
			return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError, "error code is not a string")
		}
		f.ErrorCode = ocpp.ErrorCode(code)
		// A non-string description is not worth rejecting the frame over.
		_ = json.Unmarshal(raw[3], &f.ErrorDescription)
		if len(raw) > 4 {
			f.ErrorDetails = raw[4]
		}

	default:
		return Frame{}, id, ocpp.Errorf(ocpp.ErrRpcFrameworkError,
			"unknown message type id %d", typ)
	}

	return f, "", nil
}

// EncodeCall encodes a request.
func EncodeCall(id, action string, payload any) ([]byte, error) {
	if payload == nil {
		payload = struct{}{}
	}
	b, err := json.Marshal([]any{int(MessageTypeCall), id, action, payload})
	if err != nil {
		return nil, fmt.Errorf("encoding CALL %s: %w", action, err)
	}
	return b, nil
}

// EncodeCallResult encodes a successful response. A nil payload is written
// as an empty object, since strict peers reject a null one.
func EncodeCallResult(id string, payload any) ([]byte, error) {
	// An empty result must still be an object on the wire, not null.
	if payload == nil {
		payload = struct{}{}
	}
	b, err := json.Marshal([]any{int(MessageTypeCallResult), id, payload})
	if err != nil {
		return nil, fmt.Errorf("encoding CALLRESULT: %w", err)
	}
	return b, nil
}

// EncodeCallError encodes an error response.
func EncodeCallError(id string, rpcErr *ocpp.RPCError) ([]byte, error) {
	details := rpcErr.Details
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal([]any{
		int(MessageTypeCallError), id, string(rpcErr.Code), rpcErr.Description, details,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding CALLERROR: %w", err)
	}
	return b, nil
}
