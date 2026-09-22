// Package cli assembles the cpms command tree.
package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// DefaultConfigPath is where cpms looks when --config is not given.
const DefaultConfigPath = "config.yaml"

// ErrSilent tells the top level to exit non-zero without printing anything
// further, for commands that have already written a full report themselves.
var ErrSilent = errors.New("cpms: command reported its own error")

// options are the settings shared by every subcommand.
type options struct {
	configPath string
}

// NewRootCommand builds the command tree. Output is written to the command's
// own streams, so tests can capture it with SetOut/SetErr.
func NewRootCommand() *cobra.Command {
	opts := &options{}

	root := &cobra.Command{
		Use:   "cpms",
		Short: "A terminal charge point management system (OCPP + OCPI)",
		Long: "cpms drives an OCPP charging station from the terminal and exposes a\n" +
			"minimal OCPI 2.3.0 CPO interface on the LAN.\n\n" +
			"The charger connects to us: point the station's CSMS URL at\n" +
			"ws://<this-host>:<ocpp port>/ocpp/<charge point id>.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Without a subcommand, show help rather than doing something surprising.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", DefaultConfigPath,
		"path to the configuration file")

	root.AddCommand(
		newRunCommand(opts),
		newVersionCommand(),
		newConfigCommand(opts),
	)
	return root
}
