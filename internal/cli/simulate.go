package cli

import (
	"fmt"
	"net"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

func newSimulateCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "simulate",
		Short: "Pretend to be hardware, for testing without the station",
	}
	cmd.AddCommand(newSimulateChargerCommand(opts))
	return cmd
}

func newSimulateChargerCommand(opts *options) *cobra.Command {
	var (
		csmsURL    string
		id         string
		version    string
		connectors int
		scenario   string
		idTag      string
		vendor     string
		model      string
		logLevel   string
	)

	cmd := &cobra.Command{
		Use:   "charger",
		Short: "Run a simulated charge point against a CSMS",
		Long: "Dials a CSMS and behaves like a charging station: boots, heartbeats,\n" +
			"reports connector status and answers commands.\n\n" +
			"With no flags it reads the config file and points itself at the CSMS\n" +
			"cpms itself would serve, so `cpms run` in one terminal and\n" +
			"`cpms simulate charger` in another is a working pair.\n\n" +
			"Scenarios make the station misbehave on purpose: " + scenarioList() + ".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := newLogger(cmd.ErrOrStderr(), logLevel)
			if err != nil {
				return err
			}

			// The config supplies the defaults but is not required: the
			// simulator is just as useful pointed at someone else's CSMS.
			if cfg, err := config.Load(opts.configPath); err == nil {
				if csmsURL == "" {
					csmsURL = csmsURLFromBind(cfg.Server.OCPPBind)
				}
				if id == "" {
					id = cfg.Charger.ID
				}
				if idTag == "" {
					idTag = cfg.Auth.DefaultIDTag
				}
				if connectors == 0 {
					connectors = len(cfg.Location.EVSEs)
				}
			}
			if connectors == 0 {
				connectors = 1
			}
			if csmsURL == "" || id == "" {
				return fmt.Errorf("--csms and --id are required when %s cannot be read", opts.configPath)
			}

			sim, err := simulator.New(simulator.Options{
				URL:        csmsURL,
				ID:         id,
				Version:    ocpp.Version(version),
				Connectors: connectors,
				IDTag:      idTag,
				Vendor:     vendor,
				Model:      model,
				Scenario:   simulator.Scenario(scenario),
				Log:        logger,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cmd.Printf("simulating %s (%d connectors, scenario %s) against %s\n",
				id, connectors, scenario, csmsURL)
			cmd.Println("press Ctrl-C to stop")

			return sim.Run(ctx)
		},
	}

	f := cmd.Flags()
	f.StringVar(&csmsURL, "csms", "", "CSMS WebSocket URL (default: derived from the config's ocpp_bind)")
	f.StringVar(&id, "id", "", "charge point identity (default: the config's charger.id)")
	f.StringVar(&version, "ocpp", string(ocpp.Version16), "OCPP version to speak")
	f.IntVar(&connectors, "connectors", 0, "number of connectors (default: the config's EVSE count)")
	f.StringVar(&scenario, "scenario", string(simulator.ScenarioNormal), "misbehaviour to simulate: "+scenarioList())
	f.StringVar(&idTag, "tag", "", "RFID tag to quote when starting locally (default: the config's default_id_tag)")
	f.StringVar(&vendor, "vendor", "", "chargePointVendor to report")
	f.StringVar(&model, "model", "", "chargePointModel to report")
	f.StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn or error")

	return cmd
}

func scenarioList() string {
	names := make([]string, 0, len(simulator.Scenarios()))
	for _, s := range simulator.Scenarios() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// csmsURLFromBind turns a listen address into a URL the simulator can dial.
// A wildcard bind names no reachable host, so loopback stands in for it —
// which is right for the two-terminal demo this exists to serve.
func csmsURLFromBind(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "ws://" + net.JoinHostPort(host, port)
}
