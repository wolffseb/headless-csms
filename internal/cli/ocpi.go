package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// newOCPICommand groups what the operator does to the OCPI registration.
func newOCPICommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ocpi",
		Short: "Manage the OCPI registration",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "unregister",
		Short: "Forget the OCPI registration, so the partner can register again",
		Long: "Clears the registration from state.json and tells the partner with a DELETE on\n" +
			"its credentials endpoint. TOKEN_C stops working and TOKEN_A works again, so the\n" +
			"partner can redo the handshake: cpms is the Receiver and cannot start one itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newStationEnv(cmd, opts)
			if err != nil {
				return err
			}
			raw, err := env.call(cmd, socket.Request{Op: socket.OpOCPIUnregister})
			if err != nil {
				return err
			}
			var res socket.UnregisterResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			cmd.Printf("unregistered from %s\n", res.Party)
			if res.NotifyError != "" {
				cmd.PrintErrf("warning: %s was not told (%s); it finds out on its next call\n", res.Party, res.NotifyError)
			}
			cmd.Printf("to register again, the partner POSTs its credentials with TOKEN_A; versions URL %s\n",
				env.cfg.OCPI.PublicBaseURL+"/versions")
			return nil
		},
	})
	return cmd
}
