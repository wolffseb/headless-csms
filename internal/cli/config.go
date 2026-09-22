package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
)

func newConfigCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the configuration file",
	}
	cmd.AddCommand(newConfigValidateCommand(opts))
	return cmd
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

			cmd.Printf("%s is valid.\n\n", opts.configPath)
			printSummary(cmd, cfg)
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
