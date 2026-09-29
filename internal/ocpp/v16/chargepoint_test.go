package v16_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

var _ v16.Caller = (*ocppj.Conn)(nil)

// sentCall is one call the fake would have put on the wire.
type sentCall struct {
	action  string
	payload []byte
}

// fakeCaller records calls and answers every one with the same response.
type fakeCaller struct {
	mu    sync.Mutex
	calls []sentCall

	response json.RawMessage
	err      error
}

func (*fakeCaller) ID() string            { return testCP }
func (*fakeCaller) Version() ocpp.Version { return ocpp.Version16 }

func (f *fakeCaller) Call(_ context.Context, action string, payload any) (json.RawMessage, error) {
	// Marshalled exactly as ocppj.EncodeCall does, so the bytes recorded here
	// are the bytes a station would receive.
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, sentCall{action: action, payload: b})
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func (f *fakeCaller) sent() []sentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentCall(nil), f.calls...)
}

func newFakeChargePoint(response string) (*v16.ChargePoint, *fakeCaller) {
	fake := &fakeCaller{response: json.RawMessage(response)}
	return v16.NewChargePoint(fake, testConfig()), fake
}

// TestGoldenPayloads pins the exact bytes of every outbound command. A change
// here is a change to what the station receives, so it should be deliberate:
// regenerate with `go test ./internal/ocpp/v16 -run Golden -update`.
func TestGoldenPayloads(t *testing.T) {
	t.Parallel()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("loading Europe/Berlin: %v", err)
	}
	// 14:30 in Berlin in summer is 12:30 UTC. The sub-second part must not
	// reach the wire.
	expires := time.Date(2026, 8, 14, 14, 30, 0, 123456789, berlin)

	tests := []struct {
		action string
		send   func(context.Context, *v16.ChargePoint) error
	}{
		{v16.ActionReserveNow, func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.ReserveNow(ctx, ocpp.ReserveRequest{
				ReservationID: 42, EVSEUID: "EVSE-2", IDTag: testTag, ExpiresAt: expires,
			})
			return err
		}},
		{v16.ActionCancelReservation, func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.CancelReservation(ctx, 42)
			return err
		}},
		{v16.ActionRemoteStartTransaction, func(ctx context.Context, cp *v16.ChargePoint) error {
			// RemoteStartID has no 1.6 field and must leave no trace.
			_, err := cp.RemoteStart(ctx, ocpp.RemoteStartRequest{
				RemoteStartID: 7, EVSEUID: "EVSE-2", IDTag: testTag,
			})
			return err
		}},
		{v16.ActionRemoteStopTransaction, func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.RemoteStop(ctx, "1001")
			return err
		}},
		{v16.ActionUnlockConnector, func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.UnlockConnector(ctx, "EVSE-2")
			return err
		}},
		{v16.ActionTriggerMessage, func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.TriggerStatus(ctx, "EVSE-2")
			return err
		}},
		{v16.ActionGetConfiguration, func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.Capabilities(ctx)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			t.Parallel()

			cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
			if err := tt.send(context.Background(), cp); err != nil {
				t.Fatalf("send: %v", err)
			}

			sent := fake.sent()
			if len(sent) != 1 {
				t.Fatalf("%d calls sent, want 1", len(sent))
			}
			if sent[0].action != tt.action {
				t.Fatalf("action = %q, want %q", sent[0].action, tt.action)
			}

			path := filepath.Join("testdata", "golden", tt.action+".json")
			if *update {
				if err := os.WriteFile(path, sent[0].payload, 0o644); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden (run with -update to create it): %v", err)
			}
			if !bytes.Equal(sent[0].payload, want) {
				t.Errorf("payload differs from %s\n got: %s\nwant: %s", path, sent[0].payload, want)
			}
		})
	}
}

// TestCommandStatusMapping covers every status the 1.6 spec defines for each
// .conf, plus a value it does not, sent through the adapter's own methods.
func TestCommandStatusMapping(t *testing.T) {
	t.Parallel()

	send := map[string]func(context.Context, *v16.ChargePoint) (ocpp.Result, error){
		v16.ActionReserveNow: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.ReserveNow(ctx, ocpp.ReserveRequest{ReservationID: 1, EVSEUID: "EVSE-1", IDTag: testTag})
		},
		v16.ActionCancelReservation: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.CancelReservation(ctx, 1)
		},
		v16.ActionRemoteStartTransaction: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.RemoteStart(ctx, ocpp.RemoteStartRequest{EVSEUID: "EVSE-1", IDTag: testTag})
		},
		v16.ActionRemoteStopTransaction: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.RemoteStop(ctx, "1")
		},
		v16.ActionUnlockConnector: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.UnlockConnector(ctx, "EVSE-1")
		},
		v16.ActionTriggerMessage: func(ctx context.Context, cp *v16.ChargePoint) (ocpp.Result, error) {
			return cp.TriggerStatus(ctx, "EVSE-1")
		},
	}

	tests := []struct {
		action string
		raw    string
		want   ocpp.CommandStatus
	}{
		// ReservationStatus
		{v16.ActionReserveNow, "Accepted", ocpp.CommandAccepted},
		{v16.ActionReserveNow, "Faulted", ocpp.CommandFaulted},
		{v16.ActionReserveNow, "Occupied", ocpp.CommandOccupied},
		{v16.ActionReserveNow, "Rejected", ocpp.CommandRejected},
		{v16.ActionReserveNow, "Unavailable", ocpp.CommandUnavailable},
		{v16.ActionReserveNow, "Reserved", ocpp.CommandUnknown},
		// CancelReservationStatus
		{v16.ActionCancelReservation, "Accepted", ocpp.CommandAccepted},
		{v16.ActionCancelReservation, "Rejected", ocpp.CommandRejected},
		{v16.ActionCancelReservation, "Occupied", ocpp.CommandUnknown},
		// RemoteStartStopStatus
		{v16.ActionRemoteStartTransaction, "Accepted", ocpp.CommandAccepted},
		{v16.ActionRemoteStartTransaction, "Rejected", ocpp.CommandRejected},
		{v16.ActionRemoteStartTransaction, "accepted", ocpp.CommandUnknown},
		{v16.ActionRemoteStopTransaction, "Accepted", ocpp.CommandAccepted},
		{v16.ActionRemoteStopTransaction, "Rejected", ocpp.CommandRejected},
		{v16.ActionRemoteStopTransaction, "", ocpp.CommandUnknown},
		// UnlockStatus
		{v16.ActionUnlockConnector, "Unlocked", ocpp.CommandUnlocked},
		{v16.ActionUnlockConnector, "UnlockFailed", ocpp.CommandUnlockFailed},
		{v16.ActionUnlockConnector, "NotSupported", ocpp.CommandNotSupported},
		{v16.ActionUnlockConnector, "Accepted", ocpp.CommandUnknown},
		// TriggerMessageStatus
		{v16.ActionTriggerMessage, "Accepted", ocpp.CommandAccepted},
		{v16.ActionTriggerMessage, "Rejected", ocpp.CommandRejected},
		{v16.ActionTriggerMessage, "NotImplemented", ocpp.CommandNotImplemented},
		{v16.ActionTriggerMessage, "Maybe", ocpp.CommandUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.action+"/"+tt.raw, func(t *testing.T) {
			t.Parallel()

			resp, _ := json.Marshal(map[string]string{"status": tt.raw})
			cp, _ := newFakeChargePoint(string(resp))

			got, err := send[tt.action](context.Background(), cp)
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if got.Status != tt.want {
				t.Errorf("Status = %q, want %q", got.Status, tt.want)
			}
			if got.Raw != tt.raw {
				t.Errorf("Raw = %q, want %q preserved", got.Raw, tt.raw)
			}
		})
	}
}

func TestUnknownEVSEIsRefusedBeforeSending(t *testing.T) {
	t.Parallel()

	tests := map[string]func(context.Context, *v16.ChargePoint) error{
		"ReserveNow": func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.ReserveNow(ctx, ocpp.ReserveRequest{ReservationID: 1, EVSEUID: "NOPE", IDTag: testTag})
			return err
		},
		"RemoteStart": func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.RemoteStart(ctx, ocpp.RemoteStartRequest{EVSEUID: "NOPE", IDTag: testTag})
			return err
		},
		"UnlockConnector": func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.UnlockConnector(ctx, "NOPE")
			return err
		},
		"TriggerStatus": func(ctx context.Context, cp *v16.ChargePoint) error {
			_, err := cp.TriggerStatus(ctx, "NOPE")
			return err
		},
	}

	for name, send := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
			err := send(context.Background(), cp)
			if !errors.Is(err, ocpp.ErrUnknownEVSE) {
				t.Errorf("err = %v, want ErrUnknownEVSE", err)
			}
			if n := len(fake.sent()); n != 0 {
				t.Errorf("%d calls sent for an unknown EVSE, want 0", n)
			}
		})
	}
}

func TestRemoteStopRefusesNonNumericIDBeforeSending(t *testing.T) {
	t.Parallel()

	cp, fake := newFakeChargePoint(`{"status":"Accepted"}`)
	if _, err := cp.RemoteStop(context.Background(), "tx-abc"); err == nil {
		t.Error("expected an error for a non-numeric transaction id")
	}
	if n := len(fake.sent()); n != 0 {
		t.Errorf("%d calls sent, want 0", n)
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response string
		want     ocpp.Capabilities
	}{
		{
			name:     "both advertised",
			response: `{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true,"value":"Core,Reservation,RemoteTrigger"}]}`,
			want:     ocpp.Capabilities{Reservation: true, RemoteTrigger: true},
		},
		{
			name:     "spaces after commas",
			response: `{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true,"value":"Core, Reservation"}]}`,
			want:     ocpp.Capabilities{Reservation: true},
		},
		{
			name:     "core only",
			response: `{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true,"value":"Core"}]}`,
			want:     ocpp.Capabilities{},
		},
		{
			// The value is optional in 1.6.
			name:     "no value",
			response: `{"configurationKey":[{"key":"SupportedFeatureProfiles","readonly":true}]}`,
			want:     ocpp.Capabilities{},
		},
		{
			// Unknown means "not advertised", not a failure.
			name:     "unknown key",
			response: `{"unknownKey":["SupportedFeatureProfiles"]}`,
			want:     ocpp.Capabilities{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cp, _ := newFakeChargePoint(tt.response)
			got, err := cp.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCallerErrorsPassThrough(t *testing.T) {
	t.Parallel()

	rpcErr := &ocpp.RPCError{Code: ocpp.ErrNotSupported, Description: "no reservations here"}
	fake := &fakeCaller{err: rpcErr}
	cp := v16.NewChargePoint(fake, testConfig())

	_, err := cp.ReserveNow(context.Background(), ocpp.ReserveRequest{ReservationID: 1, EVSEUID: "EVSE-1", IDTag: testTag})
	var got *ocpp.RPCError
	if !errors.As(err, &got) || got != rpcErr {
		t.Errorf("err = %v, want the station's RPCError", err)
	}
}
