package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// DefaultSocketFile is the control socket's name when --socket is not given.
// It sits next to the state file, which sits next to the config file.
const DefaultSocketFile = "cpms.sock"

// socketGrace is the margin a one-shot command adds to the longest the daemon
// can take (see socketWait). The daemon reports a slow or busy station as an
// error of its own; this margin only exists so that a hung daemon cannot hang
// the CLI.
const socketGrace = 5 * time.Second

// socketWait is how long a one-shot command waits for `cpms run`.
//
// A command that was sent to the station must never be reported as failed,
// so the daemon has to answer before the CLI gives up. The daemon holds a
// command for at most charger.call_timeout waiting for its turn on the
// station (after that it answers "busy, not sent") plus charger.call_timeout
// waiting for the station's answer (after that, "timeout"), and every socket
// command makes one such call. The margin covers the daemon's own work.
func socketWait(callTimeout time.Duration) time.Duration {
	return 2*callTimeout + socketGrace
}

// resolvedSocketPath is --socket if given, otherwise cpms.sock next to the
// state file.
//
// Unix socket paths are limited to 103 bytes on macOS, and a config file a
// few directories deep is enough to exceed that. When the default does not
// fit, it falls back to a short path in the temp directory, named after the
// state file, so `cpms run` and the one-shot commands still agree on it
// without being told. A path given with --socket is used as given, and
// refused with a clear message if it is too long: an explicit choice is not
// second-guessed.
func (o *options) resolvedSocketPath(statePath string) string {
	if o.socketPath != "" {
		return o.socketPath
	}
	path := filepath.Join(filepath.Dir(statePath), DefaultSocketFile)
	if len(path) <= socket.MaxPathLen {
		return path
	}
	return fallbackSocketPath(statePath)
}

// fallbackSocketPath is a short socket path in the temp directory that is
// unique to one state file.
func fallbackSocketPath(statePath string) string {
	if abs, err := filepath.Abs(statePath); err == nil {
		statePath = abs
	}
	sum := sha256.Sum256([]byte(statePath))
	return filepath.Join(os.TempDir(), "cpms-"+hex.EncodeToString(sum[:6])+".sock")
}

// newStationCommands builds the one-shot commands that act on the station
// through a running `cpms run`.
func newStationCommands(opts *options) []*cobra.Command {
	var (
		reserveTag string
		expires    time.Duration
		startTag   string
		stopTx     string
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s reserved for %s until %s (reservation %d)\n",
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
		Long: "Asks the station to stop the session on an EVSE. --tx names the session;\n" +
			"it is only needed when cpms holds more than one there, which a state.json\n" +
			"written before cpms ended superseded sessions can still do.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			if _, err := env.call(cmd, socket.Request{Op: socket.OpStop, EVSE: env.evse, TransactionID: stopTx}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s remote stop accepted\n", env.evse)
			return nil
		},
	}
	stop.Flags().StringVar(&stopTx, "tx", "", "transaction id to stop (default: the one running on the EVSE)")

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
		Short: "Show the stations, their EVSEs, reservations and sessions",
		Long: "Shows every configured charger with its EVSEs, reservations and sessions.\n" +
			"With more than one charger there is one section per charger; --charger\n" +
			"shows just that one, in the single-charger layout.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			charger, _ := cmd.Flags().GetString("charger")
			if charger != "" {
				if _, err := control.ResolveCharger(env.cfg, charger); err != nil {
					return &ExitStatus{Code: ExitError, Err: err}
				}
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpStatus, Charger: charger})
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
				fmt.Fprintln(cmd.OutOrStdout(), string(out))
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
	status.Flags().String("charger", "", "show only this charger (default: all)")
	return []*cobra.Command{reserve, cancel, start, stop, unlock, status}
}

// stationEnv is what every one-shot command needs: the config, the EVSE it
// acts on, and where `cpms run` listens.
type stationEnv struct {
	cfg  *config.Config
	evse string
	// charger is the station a station-level command addresses, set by
	// resolveCharger. EVSE commands leave it empty: the EVSE says.
	charger    string
	socketPath string
}

// addChargerFlag adds --charger to a command that addresses a station rather
// than an EVSE.
func addChargerFlag(c *cobra.Command) {
	c.Flags().String("charger", "", "charger id from config (required when more than one charger is configured)")
}

// resolveCharger sets env.charger from --charger: the only charger when none
// is given and one is configured, an error listing the ids otherwise.
func (e *stationEnv) resolveCharger(cmd *cobra.Command) error {
	id, _ := cmd.Flags().GetString("charger")
	ch, err := control.ResolveCharger(e.cfg, id)
	if err != nil {
		return &ExitStatus{Code: ExitError, Err: err}
	}
	e.charger = ch.ID
	return nil
}

// maxCallTimeout is the longest call timeout of any charger. A command is
// answered by the daemon within twice its charger's, so this is a bound that
// holds whichever charger it goes to.
func maxCallTimeout(cfg *config.Config) time.Duration {
	var d time.Duration
	for _, ch := range cfg.Chargers {
		d = max(d, ch.CallTimeout.Duration())
	}
	return d
}

func newStationEnv(cmd *cobra.Command, opts *options) (*stationEnv, error) {
	return newStationEnvFor(cmd, opts, false)
}

// newStationEnvFor is newStationEnv; with noEVSE it leaves --evse alone, for a
// command that addresses a station and so has no use for one.
func newStationEnvFor(cmd *cobra.Command, opts *options, noEVSE bool) (*stationEnv, error) {
	cfg, err := loadConfig(cmd, opts)
	if err != nil {
		return nil, err
	}
	env := &stationEnv{cfg: cfg, socketPath: opts.resolvedSocketPath(opts.resolvedStatePath())}

	// The EVSE is checked here, against config, before anything is sent: a
	// typo should not need a running cpms to be told it is a typo.
	if f := cmd.Flags().Lookup("evse"); f != nil && !noEVSE {
		evse, err := resolveEVSE(cfg, f.Value.String())
		if err != nil {
			return nil, &ExitStatus{Code: ExitError, Err: err}
		}
		env.evse = evse
	}
	return env, nil
}

func (e *stationEnv) call(cmd *cobra.Command, req socket.Request) (json.RawMessage, error) {
	raw, err := socket.Call(cmd.Context(), e.socketPath, req, socketWait(maxCallTimeout(e.cfg)))
	if err != nil {
		subject := req.EVSE
		if req.Op == socket.OpConfigSet {
			subject = req.Key
		}
		if req.Station {
			subject = "station"
		}
		return nil, exitFor(req.Op, subject, err)
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
	fmt.Fprintf(cmd.OutOrStdout(), format, env.evse)
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

// printStatus renders the status for a person: for each charger one line, then
// a table with a row per EVSE. With several chargers each has its own section,
// which is also where a reservation or session is shown to belong to one.
func printStatus(cmd *cobra.Command, st socket.Status) {
	if len(st.Chargers) == 0 {
		printCharger(cmd, st.ChargePoint, st.EVSEs)
		fmt.Fprintln(cmd.OutOrStdout())
	}
	for _, ch := range st.Chargers {
		printCharger(cmd, ch.ChargePoint, ch.EVSEs)
		fmt.Fprintln(cmd.OutOrStdout())
	}
	fmt.Fprintln(cmd.OutOrStdout(), ocpiLine(st.OCPI))
}

// printCharger renders one charger: its line, its EVSE table and its
// charging profiles.
func printCharger(cmd *cobra.Command, cp socket.ChargePointStatus, evses []socket.EVSEStatus) {
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
	if cp.Meter != nil {
		line += "  meter " + meterCell(*cp.Meter, time.Now())
	}
	fmt.Fprintln(cmd.OutOrStdout(), line)
	fmt.Fprintln(cmd.OutOrStdout())

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EVSE\tSTATUS\tRESERVATION\tTRANSACTION\tMETER")
	for _, e := range evses {
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.UID, status, reservation, transaction, meterCell(e.MeterStatus, time.Now()))
	}
	_ = w.Flush()
	printProfiles(cmd.OutOrStdout(), cp, evses)
}

// meterCell is a meter reading for status, an EVSE's or the station's: what it
// draws, the limit the station offers, and how old the values are, such as
// "50.0 kW 125.0 A, offered 150.0 kW, 3s ago". It is "-" without a reading.
//
// Values carried over from an older MeterValues (core merges per field) each
// get their own age instead, as in "50.0 kW 3s ago, offered 22.0 kW 1m0s ago",
// so an old limit is not passed off as fresh.
func meterCell(e socket.MeterStatus, now time.Time) string {
	if e.ReadingAt == nil {
		return "-"
	}
	type value struct {
		text string
		at   time.Time
	}
	// read is one value, or none when the reading lacks it. A value
	// without its own time is as old as the reading.
	read := func(v *float64, at *time.Time, kW bool) []value {
		if v == nil {
			return nil
		}
		when := *e.ReadingAt
		if at != nil {
			when = *at
		}
		if kW {
			return []value{{fmt.Sprintf("%.1f kW", *v/1000), when}}
		}
		return []value{{fmt.Sprintf("%.1f A", *v), when}}
	}
	draw := append(read(e.PowerW, e.PowerAt, true), read(e.CurrentA, e.CurrentAt, false)...)
	offered := append(read(e.PowerOfferedW, e.PowerOfferedAt, true), read(e.CurrentOfferedA, e.CurrentOfferedAt, false)...)

	age := func(at time.Time) string { return max(now.Sub(at), 0).Round(time.Second).String() + " ago" }
	all := append(append([]value(nil), draw...), offered...)
	if len(all) == 0 {
		// A reading with neither, such as energy only: say so, rather than
		// look like no reading at all.
		return "no power reported, " + age(*e.ReadingAt)
	}
	oneAge := true
	for _, v := range all {
		oneAge = oneAge && v.at.Equal(all[0].at)
	}

	// group renders a group's values, with their age unless all values in
	// the cell share one, which then goes once at the end.
	group := func(vs []value) string {
		texts := make([]string, 0, len(vs))
		sameAge := true
		for _, v := range vs {
			texts = append(texts, v.text)
			sameAge = sameAge && v.at.Equal(vs[0].at)
		}
		switch {
		case oneAge:
			return strings.Join(texts, " ")
		case sameAge:
			return strings.Join(texts, " ") + " " + age(vs[0].at)
		}
		for i, v := range vs {
			texts[i] = v.text + " " + age(v.at)
		}
		return strings.Join(texts, " ")
	}
	var parts []string
	if len(draw) > 0 {
		parts = append(parts, group(draw))
	}
	if len(offered) > 0 {
		parts = append(parts, "offered "+group(offered))
	}
	cell := strings.Join(parts, ", ")
	if oneAge {
		cell += ", " + age(all[0].at)
	}
	return cell
}

// ocpiLine is the registration in one line: who, since when, and where
// status changes go.
func ocpiLine(o socket.OCPIStatus) string {
	if !o.Registered {
		return "OCPI  not registered"
	}
	line := "OCPI  registered with " + o.Party
	if o.Since != nil {
		line += " since " + localTime(*o.Since)
	}
	endpoint := o.PushEndpoint
	if endpoint == "" {
		return line + ", push: no locations receiver"
	}
	line += ", push: " + endpoint
	if p := o.Push; p != nil {
		line += pushHealth(*p)
	}
	return line
}

// pushHealth is what the status push has been doing, to follow the endpoint
// on the OCPI line: when it last got through, why it last did not, and how
// much is waiting.
func pushHealth(p socket.PushStatus) string {
	var parts []string
	failing := p.LastErrorAt != nil && (p.LastSuccess == nil || p.LastErrorAt.After(*p.LastSuccess))
	switch {
	case failing:
		parts = append(parts, "last push failed "+localTime(*p.LastErrorAt)+": "+truncate(p.LastError, 60))
	case p.LastSuccess != nil:
		parts = append(parts, "last push "+localTime(*p.LastSuccess))
	default:
		parts = append(parts, "nothing pushed yet")
	}
	if p.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", p.Pending))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// truncate shortens s to at most n characters, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
