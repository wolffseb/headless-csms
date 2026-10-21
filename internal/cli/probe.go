package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// need is what cpms uses a capability for, and what happens without it.
type need struct {
	capability ocpp.Capability
	required   bool
	usedFor    string
	without    string
}

// needs is cpms's side of the probe: which capabilities it relies on. What a
// station calls them, and how it advertises them, is the adapter's business.
var needs = []need{
	{ocpp.CapRemoteStartStop, true, "start, stop",
		"cpms cannot start or stop sessions, from the terminal or for OCPI START_SESSION and STOP_SESSION."},
	{ocpp.CapUnlock, true, "unlock",
		"cpms cannot release a stuck cable, from the terminal or for OCPI UNLOCK_CONNECTOR."},
	{ocpp.CapReservation, true, "reserve, cancel, OCPI RESERVE_NOW",
		"OCPI RESERVE_NOW cannot be passed on to the station, so cpms would have to hold reservations itself."},
	{ocpp.CapTriggerStatus, false, "status refresh on demand",
		"cpms learns about status changes only when the station reports them by itself."},
}

func needOf(name string) (need, bool) {
	for _, n := range needs {
		if string(n.capability) == name {
			return n, true
		}
	}
	return need{}, false
}

// probeOutput is what `cpms probe --json` prints: the station's report, and
// cpms's verdict on it. Its field names are a public interface.
type probeOutput struct {
	socket.ProbeResult
	// OK is true when no capability cpms requires is missing. Unconfirmed
	// ones do not make it false.
	OK bool `json:"ok"`
	// Missing lists the required capabilities the station reported as
	// absent. Only these make `cpms probe` exit 10.
	Missing []string `json:"missing"`
	// Unconfirmed lists the required capabilities the station did not
	// report either way, such as Reservation from a station that lists no
	// feature profiles. They may well work; try them.
	Unconfirmed []string `json:"unconfirmed"`
	// Warnings are mismatches with config, and values the station did not
	// report that would have been checked.
	Warnings []string `json:"warnings"`

	checks []check
}

func newProbeCommand(opts *options) *cobra.Command {
	var asJSON, raw bool

	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Report what the connected station supports",
		Long: "Asks the connected station for its configuration and reports which\n" +
			"capabilities cpms relies on it has, the settings that change how it\n" +
			"behaves towards cpms, and where it disagrees with config. It changes\n" +
			"nothing on the station.\n\n" +
			"A capability the station reports as absent is missing, and the command\n" +
			"exits 10. One it does not report either way is unconfirmed: that is\n" +
			"a warning, not a failure, and the command exits 0. Core is mandatory in\n" +
			"OCPP 1.6, so it is assumed present when the station lists no feature\n" +
			"profiles at all.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			data, err := env.call(cmd, socket.Request{Op: socket.OpProbe})
			if err != nil {
				if ExitCode(err) == ExitNotConnected {
					return notConnected(cmd, env)
				}
				return err
			}
			var result socket.ProbeResult
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}

			out := evaluateProbe(env.cfg, result)
			if asJSON {
				b, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					return err
				}
				cmd.Println(string(b))
			} else {
				printProbe(cmd, out, raw)
			}

			if !out.OK {
				return &ExitStatus{Code: ExitMissingCapability, Err: fmt.Errorf(
					"the station lacks %s that cpms requires: %s", plural(len(out.Missing), "capability"),
					strings.Join(out.Missing, ", "))}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON (it always includes every key)")
	cmd.Flags().BoolVar(&raw, "raw", false, "also list every configuration key the station reported")
	return cmd
}

// evaluateProbe checks a probe result against what cpms needs and against
// config.
func evaluateProbe(cfg *config.Config, r socket.ProbeResult) probeOutput {
	out := probeOutput{ProbeResult: r, Missing: []string{}, Unconfirmed: []string{}, Warnings: []string{}}

	for _, c := range r.Capabilities {
		n, ok := needOf(c.Name)
		if !ok || !n.required {
			continue
		}
		switch ocpp.Support(c.Support) {
		case ocpp.SupportYes:
		case ocpp.SupportNo:
			out.Missing = append(out.Missing, c.Name)
		default:
			out.Unconfirmed = append(out.Unconfirmed, c.Name)
		}
	}
	out.OK = len(out.Missing) == 0

	out.checks = configChecks(cfg, r)
	for _, c := range out.checks {
		if c.mark != markOK {
			out.Warnings = append(out.Warnings, c.text)
		}
	}
	return out
}

const (
	markOK      = "ok"
	markWarn    = "!"
	markUnknown = "?"
)

type check struct{ mark, text string }

// configChecks compares what the station reports with what config says.
func configChecks(cfg *config.Config, r socket.ProbeResult) []check {
	var out []check

	evses := len(cfg.Location.EVSEs)
	switch {
	case r.Connectors == nil:
		out = append(out, check{markUnknown, "the station did not report how many connectors it has; cannot check it against config"})
	case *r.Connectors != evses:
		out = append(out, check{markWarn, fmt.Sprintf(
			"the station reports %s, but config has %s; check location.evses and their ocpp_connector_id",
			plural(*r.Connectors, "connector"), plural(evses, "EVSE"))})
	default:
		var stray []check
		for i, e := range cfg.Location.EVSEs {
			if e.OCPPConnectorID > *r.Connectors {
				stray = append(stray, check{markWarn, fmt.Sprintf(
					"location.evses[%d] (%s) maps to ocpp_connector_id %d, but the station reports %s",
					i, e.UID, e.OCPPConnectorID, plural(*r.Connectors, "connector"))})
			}
		}
		if len(stray) > 0 {
			out = append(out, stray...)
		} else {
			out = append(out, check{markOK, fmt.Sprintf("%s, matching the %s in config", plural(*r.Connectors, "connector"), plural(evses, "EVSE"))})
		}
	}

	if r.HeartbeatIntervalSeconds == nil {
		return append(out, check{markUnknown, "the station did not report its heartbeat interval; cannot check it against config"})
	}
	got := time.Duration(*r.HeartbeatIntervalSeconds) * time.Second
	want := cfg.Charger.HeartbeatInterval.Duration()
	timeout := cfg.Charger.HeartbeatTimeout.Duration()
	switch {
	case got >= timeout:
		out = append(out, check{markWarn, fmt.Sprintf(
			"the station's HeartbeatInterval setting is %s, which is not before charger.heartbeat_timeout (%s) runs out. "+
				"It may be a stale setting (some stations heartbeat at the BootNotification interval regardless): "+
				"check the heartbeat spacing in the `--log-level debug` log. "+
				"If the station really heartbeats that slowly, cpms will mark it offline after charger.heartbeat_timeout",
			seconds(got), seconds(timeout))})
	case got != want:
		out = append(out, check{markWarn, fmt.Sprintf(
			"the station's HeartbeatInterval setting is %s, but charger.heartbeat_interval is %s. "+
				"It may be a stale setting (some stations heartbeat at the BootNotification interval regardless): "+
				"check the heartbeat spacing in the `--log-level debug` log",
			seconds(got), seconds(want))})
	default:
		out = append(out, check{markOK, fmt.Sprintf("heartbeat every %s, as charger.heartbeat_interval says", seconds(got))})
	}
	return out
}

// seconds shows a duration the way stations configure it.
func seconds(d time.Duration) string { return fmt.Sprintf("%gs", d.Seconds()) }

// printProbe renders the report for a person.
func printProbe(cmd *cobra.Command, out probeOutput, raw bool) {
	r := out.ProbeResult
	cp := r.ChargePoint

	cmd.Printf("%s  OCPP %s\n", cp.ID, cp.OCPPVersion)
	var identity []string
	if station := strings.TrimSpace(cp.Vendor + " " + cp.Model); station != "" {
		identity = append(identity, station)
	}
	if cp.SerialNumber != "" {
		identity = append(identity, "serial "+cp.SerialNumber)
	}
	if cp.Firmware != "" {
		identity = append(identity, "firmware "+cp.Firmware)
	}
	if len(identity) > 0 {
		cmd.Println("  " + strings.Join(identity, ", "))
	}

	cmd.Println()
	cmd.Println("Features")
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	for _, f := range r.Features {
		required, usedFor := "", "not used by cpms"
		var uses []string
		for _, g := range f.Grants {
			if n, ok := needOf(g); ok {
				uses = append(uses, n.usedFor)
				if n.required {
					required = "required"
				} else if required == "" {
					required = "optional"
				}
			}
		}
		if len(uses) > 0 {
			usedFor = strings.Join(uses, ", ")
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", f.Name, supportMark(f.Support), required, usedFor)
	}
	_ = w.Flush()

	cmd.Println()
	cmd.Println("Settings")
	w = tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	for _, s := range r.Settings {
		value := s.Value
		if !s.Reported {
			value = "not reported"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", s.Name, value, s.Meaning)
	}
	_ = w.Flush()

	cmd.Println()
	cmd.Println("Checks")
	for _, c := range out.checks {
		cmd.Printf("  %-2s  %s\n", c.mark, c.text)
	}

	if raw {
		cmd.Println()
		cmd.Println("All keys")
		w = tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		for _, k := range r.Raw {
			access := "rw"
			if k.Readonly {
				access = "ro"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", k.Key, access, k.Value)
		}
		_ = w.Flush()
	}

	cmd.Println()
	for _, f := range r.Features {
		if f.Assumed {
			cmd.Printf("%s was not reported, but it is mandatory in OCPP %s: assumed present.\n", f.Name, r.ChargePoint.OCPPVersion)
		}
	}
	for _, name := range out.Missing {
		n, _ := needOf(name)
		cmd.Printf("%s is missing: %s\n", grantedBy(r, name), n.without)
	}
	for _, name := range out.Unconfirmed {
		n, _ := needOf(name)
		cmd.Printf("%s was not reported, so cpms cannot be sure of it. Try it (%s); if the station refuses, this is what stops working: %s\n",
			grantedBy(r, name), tryIt(name), n.without)
	}
	if len(out.Missing) == 0 && len(out.Unconfirmed) == 0 {
		cmd.Println("Every capability cpms requires is present.")
	}
}

// tryIt names a command that exercises a capability, for the user to run
// when the station did not say whether it has it.
func tryIt(capability string) string {
	switch ocpp.Capability(capability) {
	case ocpp.CapRemoteStartStop:
		return "`cpms start`"
	case ocpp.CapUnlock:
		return "`cpms unlock`"
	case ocpp.CapReservation:
		return "`cpms reserve`"
	default:
		return "the matching command"
	}
}

// grantedBy names a capability the way the station does: by the features
// that would provide it.
func grantedBy(r socket.ProbeResult, capability string) string {
	var names []string
	for _, f := range r.Features {
		for _, g := range f.Grants {
			if g == capability {
				names = append(names, f.Name)
			}
		}
	}
	if len(names) == 0 {
		return capability
	}
	return strings.Join(names, " or ")
}

func supportMark(support string) string {
	switch ocpp.Support(support) {
	case ocpp.SupportYes:
		return "✓"
	case ocpp.SupportNo:
		return "✗"
	default:
		return "?"
	}
}

// notConnected explains a station that is not there to probe: the URL it
// has to be configured with, and when it was last seen leaving.
func notConnected(cmd *cobra.Command, env *stationEnv) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is not connected to cpms, so there is nothing to probe.\n", env.cfg.Charger.ID)
	fmt.Fprintf(&b, "Set the station's CSMS URL to:\n  %s\n", csmsURL(env.cfg))

	// Best effort: the probe already failed for the reason that matters.
	if data, err := env.call(cmd, socket.Request{Op: socket.OpStatus}); err == nil {
		var st socket.Status
		if json.Unmarshal(data, &st) == nil {
			if d := st.ChargePoint.LastDisconnect; d != nil {
				fmt.Fprintf(&b, "It last disconnected at %s: %s.", localTime(d.At), d.Reason)
			} else {
				b.WriteString("It has not connected since cpms run started.")
			}
		}
	}
	return &ExitStatus{Code: ExitNotConnected, Err: errors.New(strings.TrimRight(b.String(), "\n"))}
}
