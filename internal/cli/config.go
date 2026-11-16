package cli

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

func newConfigCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Check the configuration file, or change the station's own configuration",
	}
	cmd.AddCommand(newConfigValidateCommand(opts), newConfigSetCommand(opts))
	return cmd
}

func newConfigSetCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change a configuration key on the station (OCPP ChangeConfiguration)",
		Long: "Sets one of the STATION's OCPP configuration keys through a running `cpms run`.\n" +
			"It does not touch cpms's own config file, which cpms never writes.\n\n" +
			"The station keeps the value; cpms does not set it again after a reboot.\n" +
			"Exits 0 when the station accepts it, and also when it answers RebootRequired,\n" +
			"which is printed as a warning: the value takes effect after the station restarts.\n" +
			"Rejected and NotSupported (an unknown key) exit 4.\n\n" +
			"Example, to have the station report power and current in MeterValues:\n\n" +
			"  cpms config set MeterValuesSampledData \\\n" +
			"    Energy.Active.Import.Register,Power.Active.Import,Power.Offered,Current.Import,Current.Offered",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			if key == "" {
				return &ExitStatus{Code: ExitError, Err: errors.New("the key must not be empty")}
			}
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpConfigSet, Key: key, Value: value})
			if err != nil {
				return err
			}
			var r socket.ConfigSetResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s set to %q on the station (%s)\n", r.Key, r.Value, r.Status)
			if r.Status == "RebootRequired" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: the station needs a reboot before %s takes effect\n", r.Key)
			}
			return nil
		},
	}
}

func newConfigValidateCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration file and print a summary",
		Long: "Loads the configuration file, applies defaults, and validates it.\n" +
			"Exits 0 and prints a summary when the file is usable; exits 1 and lists\n" +
			"every problem, each named by its field path, when it is not.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd, opts)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid.\n\n", opts.configPath)
			printSummary(cmd, cfg, opts.resolvedStatePath())
			return nil
		},
	}
}

// loadConfig loads the config file, reporting validation problems in full and
// returning ErrSilent so the top level does not print them a second time.
func loadConfig(cmd *cobra.Command, opts *options) (*config.Config, error) {
	cfg, err := config.Load(opts.configPath)
	if err == nil {
		return cfg, nil
	}

	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) {
		// An I/O problem: no field context to report.
		return nil, fmt.Errorf("reading %s: %w", opts.configPath, err)
	}
	printValidationReport(cmd, opts.configPath, verrs)
	return nil, ErrSilent
}

// printValidationReport writes every problem to stderr, one per line, each
// anchored to the field the operator needs to edit.
func printValidationReport(cmd *cobra.Command, path string, errs config.ValidationErrors) {
	out := cmd.ErrOrStderr()

	fmt.Fprintf(out, "%s is not valid:\n\n", path)
	for _, fe := range errs {
		if fe.Path == "" {
			fmt.Fprintf(out, "  %s\n", fe.Msg)
			continue
		}
		fmt.Fprintf(out, "  %s: %s\n", fe.Path, fe.Msg)
	}
	if len(errs) == 1 {
		fmt.Fprintf(out, "\n1 problem found.\n")
		return
	}
	fmt.Fprintf(out, "\n%d problems found.\n", len(errs))
}
