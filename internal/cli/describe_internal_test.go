package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// The TUI runs commands in-process and words their errors with describe; the
// one-shot CLI gets the same error as JSON off the socket and words it with
// exitFor. The two must say the same thing, for every kind of failure.
func TestDescribeSaysWhatTheCLISays(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   socket.Op
		err  error
		want string // the literal sentence, so a change to the wording is a visible one
	}{
		{"not connected", socket.OpReserve,
			fmt.Errorf("ALP-1: %w", ocpp.ErrNotConnected),
			"ALP-1: the charge point is not connected to cpms. Check that the station's CSMS URL points here (`cpms config validate` prints it)"},
		{"rejected", socket.OpReserve,
			&control.RefusedError{Op: control.OpReserve, EVSEUID: "ALP-1", Status: ocpp.CommandRejected, Raw: "Rejected"},
			"ALP-1: the station rejected the reservation (status Rejected). If it does not support the Reservation profile, `cpms probe` will say so"},
		{"occupied", socket.OpStart,
			&control.RefusedError{Op: control.OpStart, EVSEUID: "ALP-1", Status: ocpp.CommandOccupied, Raw: "Occupied"},
			"ALP-1: the station says it is occupied (status Occupied)"},
		{"timeout", socket.OpUnlock,
			fmt.Errorf("unlock: %w", ocpp.ErrTimeout),
			"ALP-1: the station did not answer within charger.call_timeout"},
		{"unlock failed", socket.OpUnlock,
			&control.RefusedError{Op: control.OpUnlock, EVSEUID: "ALP-1", Status: ocpp.CommandUnlockFailed, Raw: "UnlockFailed"},
			"ALP-1: the station could not release the cable lock (status UnlockFailed)"},
		{"already reserved", socket.OpReserve,
			fmt.Errorf("ALP-1: %w", control.ErrAlreadyReserved),
			"EVSE already has a reservation"},
		{"nothing to stop", socket.OpStop, control.ErrNoActiveTransaction, "no active transaction"},
		{"something else", socket.OpCancel, errors.New("disk on fire"), "disk on fire"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			// What the CLI does: the daemon classifies, the JSON crosses the
			// socket, exitFor words it.
			raw, err := json.Marshal(socket.Classify(c.err))
			if err != nil {
				t.Fatal(err)
			}
			var wire socket.Error
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			viaCLI := exitFor(c.op, "ALP-1", &wire).Error()

			got := describe(c.op, "ALP-1", c.err)
			if got != viaCLI {
				t.Errorf("TUI says %q, CLI says %q", got, viaCLI)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("describe = %q, want it to contain %q", got, c.want)
			}
		})
	}
}
