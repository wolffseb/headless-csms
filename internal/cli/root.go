// Package cli assembles the cpms command tree.
package cli

import (
	"errors"
	"path/filepath"

	"github.com/spf13/cobra"
)

// DefaultConfigPath is where cpms looks when --config is not given.
const DefaultConfigPath = "config.yaml"

// DefaultStateFile is the state file's name when --state is not given. It
// sits next to the config file, not in the working directory, so the two
// files travel together wherever cpms is started from.
const DefaultStateFile = "state.json"

// ErrSilent tells the top level to exit non-zero without printing anything
// further, for commands that have already written a full report themselves.
var ErrSilent = errors.New("cpms: command reported its own error")

// options are the settings shared by every subcommand.
type options struct {
	configPath string
	statePath  string
	socketPath string
}

// resolvedStatePath is --state if given, otherwise state.json in the config
// file's directory.
func (o *options) resolvedStatePath() string {
	if o.statePath != "" {
		return o.statePath
	}
	return filepath.Join(filepath.Dir(o.configPath), DefaultStateFile)
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
	root.PersistentFlags().StringVar(&opts.statePath, "state", "",
		"path to the state file (default: "+DefaultStateFile+" next to the config file)")
	root.PersistentFlags().StringVar(&opts.socketPath, "socket", "",
		"path to the control socket of `cpms run` (default: "+DefaultSocketFile+" next to the state file)")

	root.AddCommand(
		newRunCommand(opts),
		newSimulateCommand(opts),
		newVersionCommand(),
		newConfigCommand(opts),
	)
	root.AddCommand(newStationCommands(opts)...)
	return root
}
