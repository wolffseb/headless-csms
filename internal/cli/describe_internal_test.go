package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// The TUI runs commands in-process and words their errors with describe; the
// one-shot CLI gets the same error as JSON off the socket and words it with
// exitFor. The two must say the same thing, for every kind of failure.
func TestDescribeSaysWhatTheCLISays(t *testing.T) {
	t.Parallel()

	cfg := timeoutConfig(map[string]time.Duration{"CP-1": 10 * time.Second}, map[string]string{"ALP-1": "CP-1"})

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
			"ALP-1: the station did not answer within 10s (charger.call_timeout)"},
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
			viaCLI := exitFor(cfg, c.op, "ALP-1", &wire).Error()

			got := describe(cfg, c.op, "ALP-1", c.err)
			if got != viaCLI {
				t.Errorf("TUI says %q, CLI says %q", got, viaCLI)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("describe = %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// timeoutConfig is a config with chargers of the given call timeouts and EVSEs
// on them, as far as the wording of errors reads it.
func timeoutConfig(timeouts map[string]time.Duration, evses map[string]string) *config.Config {
	cfg := &config.Config{}
	for id, d := range timeouts {
		cfg.Chargers = append(cfg.Chargers, config.Charger{ID: id, CallTimeout: config.NewDuration(d)})
	}
	slices.SortFunc(cfg.Chargers, func(a, b config.Charger) int { return strings.Compare(a.ID, b.ID) })
	for uid, ch := range evses {
		cfg.Location.EVSEs = append(cfg.Location.EVSEs, config.EVSE{UID: uid, Charger: ch})
	}
	return cfg
}

// A timeout names the time that applied, so nobody has to open the config to
// know how long "did not answer" was. With several chargers it is the one the
// command went to; for a command with no EVSE it is only named if every
// charger agrees.
func TestTimeoutNamesTheConfiguredDuration(t *testing.T) {
	t.Parallel()

	two := timeoutConfig(map[string]time.Duration{"CP-1": 10 * time.Second, "CP-2": 45 * time.Second},
		map[string]string{"E-1": "CP-1", "E-2": "CP-2"})
	same := timeoutConfig(map[string]time.Duration{"CP-1": 20 * time.Second, "CP-2": 20 * time.Second}, nil)

	for _, c := range []struct {
		name    string
		cfg     *config.Config
		subject string
		want    string
	}{
		{"first charger's EVSE", two, "E-1", "E-1: the station did not answer within 10s (charger.call_timeout)"},
		{"second charger's EVSE", two, "E-2", "E-2: the station did not answer within 45s (charger.call_timeout)"},
		{"station command, chargers differ", two, "station", "station: the station did not answer within charger.call_timeout"},
		{"station command, chargers agree", same, "station", "station: the station did not answer within 20s (charger.call_timeout)"},
		{"no config", nil, "E-1", "E-1: the station did not answer within charger.call_timeout"},
	} {
		got := exitFor(c.cfg, socket.OpUnlock, c.subject, &socket.Error{Code: socket.CodeTimeout}).Error()
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
