package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/buildinfo"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cpms version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Get().String())
			return nil
		},
	}
}
