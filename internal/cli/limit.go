package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// usage is an error in how a command was called, found before connecting.
func usage(format string, args ...any) error {
	return &ExitStatus{Code: ExitError, Err: fmt.Errorf(format, args...)}
}

// addTargetFlags adds --evse and --station to a limit or profile command.
func addTargetFlags(c *cobra.Command) {
	c.Flags().String("evse", "", "EVSE uid from config (may be omitted when only one EVSE is configured)")
	c.Flags().Bool("station", false, "address the whole station (OCPP 1.6 connector 0) instead of an EVSE")
}

// limitEnv is a station env plus whether the command addresses the whole
// station. --evse and --station exclude each other.
func newLimitEnv(cmd *cobra.Command, opts *options) (*stationEnv, bool, error) {
	station, _ := cmd.Flags().GetBool("station")
	if station && cmd.Flags().Changed("evse") {
		return nil, false, usage("--evse and --station exclude each other")
	}
	env, err := newStationEnv(cmd, opts)
	if err != nil {
		return nil, false, err
	}
	if station {
		env.evse = ""
	}
	return env, station, nil
}

func newLimitCommand(opts *options) *cobra.Command {
	limit := &cobra.Command{
		Use:   "limit",
		Short: "Set, clear and show charging limits (OCPP Smart Charging)",
		Long: "Charging limits are OCPP charging profiles. cpms records the ones the\n" +
			"station accepted in state.json and lists them in `cpms status` as sent;\n" +
			"the station is the source of truth and may drop them, for instance on a\n" +
			"reboot. `cpms limit show` asks the station what it actually applies.",
		Args: cobra.NoArgs,
	}
	limit.AddCommand(newLimitSetCommand(opts), newLimitClearCommand(opts), newLimitShowCommand(opts))
	return limit
}

func newLimitSetCommand(opts *options) *cobra.Command {
	var (
		kw, amps float64
		purpose  string
		stack    int
		forD     time.Duration
		force    bool
	)
	c := &cobra.Command{
		Use:   "set",
		Short: "Limit an EVSE, or the station, to a power or current",
		Long: "Installs a one-period charging profile from now on. The default purpose,\n" +
			"tx-default, applies to the sessions that start after it, and can be set\n" +
			"while the EVSE is reserved. tx limits the running session; station-max\n" +
			"caps the whole station and needs --station.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := socket.LimitRequest{Purpose: purpose, StackLevel: stack, Force: force}
			switch hasKW, hasAmps := cmd.Flags().Changed("kw"), cmd.Flags().Changed("amps"); {
			case hasKW && hasAmps:
				return usage("--kw and --amps exclude each other")
			case hasKW:
				req.KW = &kw
			case hasAmps:
				req.Amps = &amps
			default:
				return usage("one of --kw or --amps is required")
			}
			if _, err := socket.ParsePurpose(purpose); err != nil {
				return usage("--purpose: %v", err)
			}
			station, _ := cmd.Flags().GetBool("station")
			switch {
			case station && purpose != "station-max":
				return usage("--station only takes --purpose station-max, not %s", purpose)
			case !station && purpose == "station-max":
				return usage("--purpose station-max caps the whole station; add --station")
			case forD < 0:
				return usage("--for %s is negative", forD)
			case stack < 0:
				return usage("--stack %d is negative", stack)
			case force && purpose != "tx":
				return usage("--force only applies to --purpose tx")
			}
			if forD > 0 {
				req.For = forD.String()
			}

			env, station, err := newLimitEnv(cmd, opts)
			if err != nil {
				return err
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpLimitSet, EVSE: env.evse, Station: station, Limit: &req})
			if err != nil {
				return err
			}
			return printProfileResult(cmd.OutOrStdout(), raw)
		},
	}
	addTargetFlags(c)
	c.Flags().Float64Var(&kw, "kw", 0, "power limit in kW (sent as W)")
	c.Flags().Float64Var(&amps, "amps", 0, "current limit in A per phase")
	c.Flags().StringVar(&purpose, "purpose", "tx-default", "tx-default, station-max or tx")
	c.Flags().IntVar(&stack, "stack", 0, "stack level: the highest of one purpose wins")
	c.Flags().DurationVar(&forD, "for", 0, "how long the limit holds (default: until cleared)")
	c.Flags().BoolVar(&force, "force", false, "with --purpose tx: send it even with no session running, without a transaction id")
	return c
}

func newLimitClearCommand(opts *options) *cobra.Command {
	var (
		id      int
		purpose string
		all     bool
	)
	c := &cobra.Command{
		Use:   "clear",
		Short: "Clear charging profiles",
		Long: "Clears by --id, by --purpose, or --all. --purpose and --all clear on\n" +
			"every EVSE and the station unless --evse or --station narrows them.\n" +
			"A station that holds no matching profile answers Unknown; cpms then drops\n" +
			"its own records of them anyway and exits with the rejected code.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n := 0
			for _, f := range []string{"id", "purpose", "all"} {
				if cmd.Flags().Changed(f) {
					n++
				}
			}
			if n != 1 {
				return usage("give exactly one of --id, --purpose or --all")
			}
			req := socket.ClearRequest{}
			byTarget := cmd.Flags().Changed("evse") || cmd.Flags().Changed("station")
			switch {
			case cmd.Flags().Changed("id"):
				if byTarget {
					return usage("--id names one profile; it takes no --evse or --station")
				}
				req.ID = &id
			case cmd.Flags().Changed("purpose"):
				if _, err := socket.ParsePurpose(purpose); err != nil {
					return usage("--purpose: %v", err)
				}
				req.Purpose = purpose
			}
			req.ByTarget = byTarget

			env, station, err := newLimitEnv(cmd, opts)
			if err != nil {
				return err
			}
			if !byTarget {
				// Not narrowed: the whole station is the subject, and the
				// daemon ignores the target without ByTarget.
				env.evse, station = "", true
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpLimitClear, EVSE: env.evse, Station: station, Clear: &req})
			if err != nil {
				return err
			}
			var res socket.ClearResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(res.Dropped) == 0 {
				_, err = fmt.Fprintln(out, "cleared at the station (cpms held no matching record)")
				return err
			}
			ids := make([]string, len(res.Dropped))
			for i, d := range res.Dropped {
				ids[i] = fmt.Sprintf("#%d", d)
			}
			_, err = fmt.Fprintf(out, "cleared at the station: %s\n", strings.Join(ids, ", "))
			return err
		},
	}
	addTargetFlags(c)
	c.Flags().IntVar(&id, "id", 0, "the profile id to clear")
	c.Flags().StringVar(&purpose, "purpose", "", "clear every profile of this purpose: tx-default, station-max or tx")
	c.Flags().BoolVar(&all, "all", false, "clear every profile")
	return c
}

func newLimitShowCommand(opts *options) *cobra.Command {
	var (
		window time.Duration
		unit   string
	)
	c := &cobra.Command{
		Use:   "show",
		Short: "Show the limits the station applies (GetCompositeSchedule)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if window <= 0 || window%time.Second != 0 {
				return usage("--duration %s is not a positive number of seconds", window)
			}
			if unit != "" && unit != "W" && unit != "A" {
				return usage("--unit %q: want W or A", unit)
			}
			env, station, err := newLimitEnv(cmd, opts)
			if err != nil {
				return err
			}
			raw, err := env.call(cmd, socket.Request{
				Op: socket.OpLimitShow, EVSE: env.evse, Station: station,
				Schedule: &socket.ScheduleRequest{Duration: window.String(), Unit: unit},
			})
			if err != nil {
				return err
			}
			var res socket.ScheduleResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			return printSchedule(cmd.OutOrStdout(), res)
		},
	}
	addTargetFlags(c)
	c.Flags().DurationVar(&window, "duration", socket.DefaultScheduleWindow, "how far ahead to look")
	c.Flags().StringVar(&unit, "unit", "", "W or A (default: the station's choice)")
	return c
}

func newProfileCommand(opts *options) *cobra.Command {
	profile := &cobra.Command{
		Use:   "profile",
		Short: "Install full charging profiles",
		Args:  cobra.NoArgs,
	}
	var file string
	set := &cobra.Command{
		Use:   "set",
		Short: "Install a charging profile from a JSON file (shape in the README)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return usage("-f is required")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return usage("reading the profile: %v", err)
			}
			var p socket.Profile
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&p); err != nil {
				return usage("%s is not a profile: %v", file, err)
			}
			// The names and the rules are checked here too, so a mistake in
			// the file needs no running cpms to be told.
			target := "EVSE" // any EVSE: the uid itself is checked below
			if station, _ := cmd.Flags().GetBool("station"); station {
				target = ""
			}
			check, err := p.ToOCPP(target)
			if err == nil {
				err = check.Validate()
			}
			if err != nil {
				return usage("%s: %v", file, err)
			}

			env, station, err := newLimitEnv(cmd, opts)
			if err != nil {
				return err
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpProfileSet, EVSE: env.evse, Station: station, Profile: &p})
			if err != nil {
				return err
			}
			return printProfileResult(cmd.OutOrStdout(), raw)
		},
	}
	addTargetFlags(set)
	set.Flags().StringVarP(&file, "file", "f", "", "the profile, as JSON")
	profile.AddCommand(set)
	return profile
}

func printProfileResult(w io.Writer, raw json.RawMessage) error {
	var res socket.ProfileResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s: charging profile #%d accepted: %s\n", targetName(res.EVSE), res.ID, describeProfile(res.ChargingProfileStatus))
	return err
}

func targetName(evse string) string {
	if evse == "" {
		return "station"
	}
	return evse
}

// describeProfile is one profile in one line, for status and set.
func describeProfile(p socket.ChargingProfileStatus) string {
	s := fmt.Sprintf("%s stack %d", p.Purpose, p.StackLevel)
	if len(p.Periods) > 0 {
		s += ", " + rate(p.Periods[0].Limit, p.Unit)
		if len(p.Periods) > 1 {
			s += fmt.Sprintf(" (+%d periods)", len(p.Periods)-1)
		}
	}
	if p.TransactionID != "" {
		s += ", transaction " + p.TransactionID
	}
	if p.ValidTo != nil {
		s += ", until " + localTime(*p.ValidTo)
	}
	return s
}

// rate renders a limit: W as kW with one decimal, A as is.
func rate(limit float64, unit string) string {
	if unit == "W" {
		return fmt.Sprintf("%.1f kW", limit/1000)
	}
	return fmt.Sprintf("%.1f %s", limit, unit)
}

// printSchedule renders a composite schedule as a period table.
func printSchedule(w io.Writer, res socket.ScheduleResult) error {
	head := fmt.Sprintf("%s: composite schedule from %s", targetName(res.EVSE), localTime(res.Start))
	if res.DurationSeconds != nil {
		head += fmt.Sprintf(" for %s", time.Duration(*res.DurationSeconds)*time.Second)
	}
	if _, err := fmt.Fprintln(w, head); err != nil {
		return err
	}
	if len(res.Periods) == 0 {
		_, err := fmt.Fprintln(w, "no limits")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FROM\tUNTIL\tLIMIT")
	for i, p := range res.Periods {
		from := res.Start.Add(time.Duration(p.StartSeconds) * time.Second)
		until := "-"
		switch {
		case i+1 < len(res.Periods):
			until = localTime(res.Start.Add(time.Duration(res.Periods[i+1].StartSeconds) * time.Second))
		case res.DurationSeconds != nil:
			until = localTime(res.Start.Add(time.Duration(*res.DurationSeconds) * time.Second))
		}
		limit := rate(p.Limit, res.Unit)
		if p.Phases != nil {
			limit += fmt.Sprintf(" (%d phases)", *p.Phases)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", localTime(from), until, limit)
	}
	return tw.Flush()
}

// printProfiles lists the recorded profiles under the status table, when
// there are any.
func printProfiles(w io.Writer, st socket.Status) {
	type row struct {
		target string
		p      socket.ChargingProfileStatus
	}
	var rows []row
	for _, p := range st.ChargePoint.ChargingProfiles {
		rows = append(rows, row{"station", p})
	}
	for _, e := range st.EVSEs {
		for _, p := range e.ChargingProfiles {
			rows = append(rows, row{e.UID, p})
		}
	}
	if len(rows) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Charging profiles, as sent (the station may have dropped them; `cpms limit show` asks it):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "  %s\t#%d\t%s\n", r.target, r.p.ID, describeProfile(r.p))
	}
	_ = tw.Flush()
}
