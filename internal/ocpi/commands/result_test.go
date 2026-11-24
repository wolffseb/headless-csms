package commands_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/ocpi/commands"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

func refused(status ocpp.CommandStatus) error {
	return &control.RefusedError{Op: control.OpReserve, EVSEUID: "EVSE-1", Status: status, Raw: string(status)}
}

// How what control did becomes the CommandResultType. The result types are
// compared with their literal wire values.
func TestResultFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, "ACCEPTED"},
		{"rejected", refused(ocpp.CommandRejected), "REJECTED"},
		{"occupied", refused(ocpp.CommandOccupied), "EVSE_OCCUPIED"},
		{"faulted", refused(ocpp.CommandFaulted), "EVSE_INOPERATIVE"},
		{"unavailable", refused(ocpp.CommandUnavailable), "EVSE_INOPERATIVE"},
		{"unlock failed", refused(ocpp.CommandUnlockFailed), "FAILED"},
		{"not supported", refused(ocpp.CommandNotSupported), "NOT_SUPPORTED"},
		{"not implemented", refused(ocpp.CommandNotImplemented), "NOT_SUPPORTED"},
		{"a status the spec does not define", refused(ocpp.CommandUnknown), "FAILED"},
		{"wrapped refusal", fmt.Errorf("context: %w", refused(ocpp.CommandOccupied)), "EVSE_OCCUPIED"},
		{"the station is silent", ocpp.ErrTimeout, "TIMEOUT"},
		{"the backstop deadline", context.DeadlineExceeded, "TIMEOUT"},
		{"unknown reservation", fmt.Errorf("RES-1: %w", control.ErrNoReservation), "UNKNOWN_RESERVATION"},
		{"EVSE already reserved", fmt.Errorf("EVSE-1: %w", control.ErrAlreadyReserved), "EVSE_OCCUPIED"},
		{"CALLERROR: not implemented", &ocpp.RPCError{Code: ocpp.ErrNotImplemented}, "NOT_SUPPORTED"},
		{"CALLERROR: not supported", &ocpp.RPCError{Code: ocpp.ErrNotSupported}, "NOT_SUPPORTED"},
		{"CALLERROR: anything else", &ocpp.RPCError{Code: ocpp.ErrInternalError}, "FAILED"},
		{"not connected", ocpp.ErrNotConnected, "FAILED"},
		{"never sent", ocpp.ErrNotSent, "FAILED"},
		{"no session to stop", control.ErrNoActiveTransaction, "FAILED"},
		{"shutting down", control.ErrShuttingDown, "FAILED"},
		{"cancelled", context.Canceled, "FAILED"},
		{"anything else", errors.New("boom"), "FAILED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, msg := commands.ResultFor(tc.err)
			if string(got) != tc.want {
				t.Errorf("ResultFor(%v) = %s, want %s", tc.err, got, tc.want)
			}
			if (tc.err == nil) != (msg == "") {
				t.Errorf("message = %q for error %v", msg, tc.err)
			}
		})
	}
}
