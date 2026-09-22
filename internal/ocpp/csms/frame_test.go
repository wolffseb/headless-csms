package csms

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

func TestParseFrameValid(t *testing.T) {
	t.Parallel()

	t.Run("call", func(t *testing.T) {
		t.Parallel()

		f, _, rpcErr := parseFrame([]byte(`[2,"abc","BootNotification",{"chargePointVendor":"Alpitronic"}]`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if f.Type != messageTypeCall || f.ID != "abc" || f.Action != "BootNotification" {
			t.Fatalf("got %+v, want a CALL abc/BootNotification", f)
		}
		var payload map[string]string
		if err := json.Unmarshal(f.Payload, &payload); err != nil {
			t.Fatalf("payload did not survive: %v", err)
		}
		if payload["chargePointVendor"] != "Alpitronic" {
			t.Errorf("payload = %v", payload)
		}
	})

	t.Run("call result", func(t *testing.T) {
		t.Parallel()

		f, _, rpcErr := parseFrame([]byte(`[3,"abc",{"status":"Accepted"}]`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if f.Type != messageTypeCallResult || f.ID != "abc" {
			t.Fatalf("got %+v, want a CALLRESULT abc", f)
		}
	})

	t.Run("call error", func(t *testing.T) {
		t.Parallel()

		f, _, rpcErr := parseFrame([]byte(`[4,"abc","NotSupported","nope",{"extra":1}]`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if f.ErrorCode != ocpp.ErrNotSupported || f.ErrorDescription != "nope" {
			t.Fatalf("got %+v, want NotSupported/nope", f)
		}
	})

	t.Run("call error without details", func(t *testing.T) {
		t.Parallel()

		// The spec says five elements, but stacks in the wild omit the details
		// object. Understanding the response beats being right about arity.
		f, _, rpcErr := parseFrame([]byte(`[4,"abc","GenericError","boom"]`))
		if rpcErr != nil {
			t.Fatalf("unexpected error: %v", rpcErr)
		}
		if f.ErrorCode != ocpp.ErrGenericError {
			t.Fatalf("got %+v, want GenericError", f)
		}
	})
}

func TestParseFrameMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		wantID string
	}{
		{"not json", `{`, unknownMessageID},
		{"not an array", `{"messageId":"abc"}`, unknownMessageID},
		{"empty array", `[]`, unknownMessageID},
		{"too few elements", `[2,"abc"]`, unknownMessageID},
		{"message type not a number", `["two","abc","Heartbeat",{}]`, unknownMessageID},
		{"message id not a string", `[2,42,"Heartbeat",{}]`, unknownMessageID},
		{"empty message id", `[2,"","Heartbeat",{}]`, unknownMessageID},
		// From here the id is readable, so the peer can match our error to its
		// pending call.
		{"unknown message type id", `[9,"abc","Heartbeat",{}]`, "abc"},
		{"call missing payload", `[2,"abc","Heartbeat"]`, "abc"},
		{"call with extra element", `[2,"abc","Heartbeat",{},{}]`, "abc"},
		{"call action not a string", `[2,"abc",7,{}]`, "abc"},
		{"call action empty", `[2,"abc","",{}]`, "abc"},
		{"call result with extra element", `[3,"abc",{},{}]`, "abc"},
		{"call error too short", `[4,"abc","GenericError"]`, "abc"},
		{"call error code not a string", `[4,"abc",7,"boom",{}]`, "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, gotID, rpcErr := parseFrame([]byte(tt.input))
			if rpcErr == nil {
				t.Fatal("expected the frame to be rejected")
			}
			// Anything that is not a well-formed RPC frame is
			// RpcFrameworkError; FormationViolation is for a good frame with a
			// payload that does not fit its action, which only the version
			// handler can judge.
			if rpcErr.Code != ocpp.ErrRpcFrameworkError {
				t.Errorf("code = %s, want %s", rpcErr.Code, ocpp.ErrRpcFrameworkError)
			}
			if gotID != tt.wantID {
				t.Errorf("reply id = %q, want %q", gotID, tt.wantID)
			}
			if rpcErr.Description == "" {
				t.Error("error should describe what was wrong")
			}
		})
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("call", func(t *testing.T) {
		t.Parallel()

		data, err := encodeCall("7", "Heartbeat", map[string]any{"a": 1})
		if err != nil {
			t.Fatal(err)
		}
		f, _, rpcErr := parseFrame(data)
		if rpcErr != nil {
			t.Fatalf("our own CALL did not parse: %v", rpcErr)
		}
		if f.Type != messageTypeCall || f.ID != "7" || f.Action != "Heartbeat" {
			t.Errorf("got %+v", f)
		}
	})

	t.Run("nil payloads become empty objects", func(t *testing.T) {
		t.Parallel()

		// A response of `null` is rejected by strict chargers; `{}` is correct
		// for a message that carries nothing back.
		data, err := encodeCallResult("7", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(data); !strings.Contains(got, "{}") {
			t.Errorf("encoded %s, want an empty object payload", got)
		}
	})

	t.Run("call error", func(t *testing.T) {
		t.Parallel()

		data, err := encodeCallError("7", ocpp.Errorf(ocpp.ErrNotImplemented, "no such action"))
		if err != nil {
			t.Fatal(err)
		}
		f, _, rpcErr := parseFrame(data)
		if rpcErr != nil {
			t.Fatalf("our own CALLERROR did not parse: %v", rpcErr)
		}
		if f.ErrorCode != ocpp.ErrNotImplemented || f.ErrorDescription != "no such action" {
			t.Errorf("got %+v", f)
		}
		// Details must be an object even when we have none to send.
		if !strings.HasSuffix(string(data), `{}]`) {
			t.Errorf("encoded %s, want a trailing empty details object", data)
		}
	})
}

// TestOccurrenceConstraintViolationKeepsSpecSpelling guards a detail that is
// easy to "fix" by accident: peers compare this string literally, so the
// specification's own misspelling has to survive.
func TestOccurrenceConstraintViolationKeepsSpecSpelling(t *testing.T) {
	t.Parallel()

	if got := string(ocpp.ErrOccurrenceConstraintViolation); got != "OccurenceConstraintViolation" {
		t.Errorf("wire value = %q, want the spec's spelling %q", got, "OccurenceConstraintViolation")
	}
}
