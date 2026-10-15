// Command cpms is a terminal charge point management system: it speaks OCPP to
// a charging station and exposes a minimal OCPI 2.3.0 CPO interface on the LAN.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/wolffseb/cli-cpms/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		// ErrSilent means the command already printed a full report.
		if !errors.Is(err, cli.ErrSilent) {
			fmt.Fprintln(os.Stderr, "cpms: "+err.Error())
		}
		// The one-shot commands carry distinct exit codes for scripts to
		// branch on; everything else exits 1.
		os.Exit(cli.ExitCode(err))
	}
}
