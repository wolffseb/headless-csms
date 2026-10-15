package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// DefaultSocketFile is the control socket's name when --socket is not given.
// It sits next to the state file, which sits next to the config file.
const DefaultSocketFile = "cpms.sock"

// socketGrace is how much longer than charger.call_timeout a one-shot command
// waits for `cpms run`. The daemon reports a slow station as a timeout of its
// own; this margin only exists so that a hung daemon cannot hang the CLI.
const socketGrace = 5 * time.Second

// resolvedSocketPath is --socket if given, otherwise cpms.sock next to the
// state file.
func (o *options) resolvedSocketPath(statePath string) string {
	if o.socketPath != "" {
		return o.socketPath
	}
	return filepath.Join(filepath.Dir(statePath), DefaultSocketFile)
}

// newStationCommands builds the one-shot commands that act on the station
// through a running `cpms run`.
func newStationCommands(opts *options) []*cobra.Command {
	var (
		reserveTag string
		expires    time.Duration
		startTag   string
		statusJSON bool
	)

	reserve := &cobra.Command{
		Use:   "reserve",
		Short: "Reserve an EVSE for an RFID tag",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			req := socket.Request{Op: socket.OpReserve, EVSE: env.evse, IDTag: reserveTag}
			if expires > 0 {
				req.ExpiresIn = expires.String()
			}
			raw, err := env.call(cmd, req)
			if err != nil {
				return err
			}
			var r socket.ReserveResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			cmd.Printf("%s reserved for %s until %s (reservation %d)\n",
				r.EVSE, r.IDTag, localTime(r.ExpiresAt), r.ReservationID)
			return nil
		},
	}
	reserve.Flags().StringVar(&reserveTag, "tag", "", "RFID tag to hold the EVSE for (default: auth.default_id_tag)")
	reserve.Flags().DurationVar(&expires, "expires", 0, "how long the reservation holds (default 15m)")

	cancel := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel the reservation on an EVSE",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return simpleCommand(cmd, opts, socket.OpCancel, "", "%s reservation cancelled\n")
		},
	}

	start := &cobra.Command{
		Use:   "start",
		Short: "Start a charging session on an EVSE",
		Long: "Asks the station to start a session. It returns once the station has\n" +
			"accepted; the session itself appears in `cpms status` when the station\n" +
			"reports it, which may wait for a car to be plugged in.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return simpleCommand(cmd, opts, socket.OpStart, startTag, "%s remote start accepted\n")
		},
	}
	start.Flags().StringVar(&startTag, "tag", "", "RFID tag to start with (default: auth.default_id_tag)")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the charging session running on an EVSE",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return simpleCommand(cmd, opts, socket.OpStop, "", "%s remote stop accepted\n")
		},
	}

	unlock := &cobra.Command{
		Use:   "unlock",
		Short: "Release the cable lock on an EVSE",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return simpleCommand(cmd, opts, socket.OpUnlock, "", "%s unlocked\n")
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the station, its EVSEs, reservations and sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpStatus})
			if err != nil {
				return err
			}
			var st socket.Status
			if err := json.Unmarshal(raw, &st); err != nil {
				return err
			}
			if statusJSON {
				out, err := json.MarshalIndent(st, "", "  ")
				if err != nil {
					return err
				}
				cmd.Println(string(out))
				return nil
			}
			printStatus(cmd, st)
			return nil
		},
	}
	status.Flags().BoolVar(&statusJSON, "json", false, "print the status as JSON")

	for _, c := range []*cobra.Command{reserve, cancel, start, stop, unlock} {
		c.Flags().String("evse", "", "EVSE uid from config (may be omitted when only one EVSE is configured)")
	}
	return []*cobra.Command{reserve, cancel, start, stop, unlock, status}
}

// stationEnv is what every one-shot command needs: the config, the EVSE it
// acts on, and where `cpms run` listens.
type stationEnv struct {
	cfg        *config.Config
	evse       string
	socketPath string
}

func newStationEnv(cmd *cobra.Command, opts *options) (*stationEnv, error) {
	cfg, err := loadConfig(cmd, opts)
	if err != nil {
		return nil, err
	}
	env := &stationEnv{cfg: cfg, socketPath: opts.resolvedSocketPath(opts.resolvedStatePath())}

	// The EVSE is checked here, against config, before anything is sent: a
	// typo should not need a running cpms to be told it is a typo.
	if f := cmd.Flags().Lookup("evse"); f != nil {
		evse, err := resolveEVSE(cfg, f.Value.String())
		if err != nil {
			return nil, &ExitStatus{Code: ExitError, Err: err}
		}
		env.evse = evse
	}
	return env, nil
}

func (e *stationEnv) call(cmd *cobra.Command, req socket.Request) (json.RawMessage, error) {
	timeout := e.cfg.Charger.CallTimeout.Duration() + socketGrace
	raw, err := socket.Call(cmd.Context(), e.socketPath, req, timeout)
	if err != nil {
		return nil, exitFor(req.Op, req.EVSE, err)
	}
	return raw, nil
}

// simpleCommand runs an operation whose only result is success, and prints
// format with the EVSE uid.
func simpleCommand(cmd *cobra.Command, opts *options, op socket.Op, tag, format string) error {
	env, err := newStationEnv(cmd, opts)
	if err != nil {
		return err
	}
	if _, err := env.call(cmd, socket.Request{Op: op, EVSE: env.evse, IDTag: tag}); err != nil {
		return err
	}
	cmd.Printf(format, env.evse)
	return nil
}

// resolveEVSE checks an --evse value against config. It may be left out only
// when there is exactly one EVSE, because then there is nothing to choose.
func resolveEVSE(cfg *config.Config, uid string) (string, error) {
	evses := cfg.Location.EVSEs
	if uid == "" {
		if len(evses) == 1 {
			return evses[0].UID, nil
		}
		return "", fmt.Errorf("--evse is required with %d EVSEs configured; one of: %s", len(evses), evseList(cfg))
	}
	if _, ok := cfg.EVSEByUID(uid); !ok {
		return "", fmt.Errorf("unknown EVSE %q; configured: %s", uid, evseList(cfg))
	}
	return uid, nil
}

func evseList(cfg *config.Config) string {
	uids := make([]string, len(cfg.Location.EVSEs))
	for i, e := range cfg.Location.EVSEs {
		uids[i] = e.UID
	}
	return strings.Join(uids, ", ")
}

// printStatus renders the status for a person: one line for the station, then
// a table with a row per EVSE.
func printStatus(cmd *cobra.Command, st socket.Status) {
	cp := st.ChargePoint
	state := "offline"
	if cp.Online {
		state = "online"
	}
	line := cp.ID + "  " + state
	if station := strings.TrimSpace(strings.Join([]string{cp.Vendor, cp.Model}, " ")); station != "" {
		line += "  " + station
		if cp.Firmware != "" {
			line += " fw " + cp.Firmware
		}
	}
	if cp.LastSeen != nil {
		line += "  last seen " + localTime(*cp.LastSeen)
	} else {
		line += "  never seen"
	}
	cmd.Println(line)
	cmd.Println()

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EVSE\tSTATUS\tRESERVATION\tTRANSACTION")
	for _, e := range st.EVSEs {
		status := e.Status
		if e.ErrorCode != "" && e.ErrorCode != "NoError" {
			status += " (" + e.ErrorCode + ")"
		}
		reservation, transaction := "-", "-"
		if r := e.Reservation; r != nil {
			reservation = fmt.Sprintf("#%d %s until %s", r.ID, r.IDTag, localTime(r.ExpiresAt))
			if r.State != "active" {
				reservation += " (" + r.State + ")"
			}
		}
		if tx := e.Transaction; tx != nil {
			transaction = fmt.Sprintf("%s %s since %s", tx.ID, tx.IDTag, localTime(tx.StartedAt))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.UID, status, reservation, transaction)
	}
	_ = w.Flush()
}

// localTime shows a timestamp the way a person at the station reads a clock:
// local time, with the date only when it is not today.
func localTime(t time.Time) string {
	t = t.Local()
	now := time.Now()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("2006-01-02 15:04:05")
}
