package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	"github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// shutdownGrace bounds how long we wait for connections to close on exit.
const shutdownGrace = 5 * time.Second

func newRunCommand(opts *options) *cobra.Command {
	var logLevel string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the CSMS and wait for the charger to connect",
		Long: "Starts the OCPP and OCPI servers and logs what the charge point does.\n\n" +
			"Remember that the charger dials us, not the other way round: point the\n" +
			"station's CSMS URL at the address printed on startup.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd, opts)
			if err != nil {
				return err
			}
			logger, err := newLogger(cmd.ErrOrStderr(), logLevel)
			if err != nil {
				return err
			}
			statePath := opts.resolvedStatePath()
			return run(cmd, cfg, statePath, opts.resolvedSocketPath(statePath), logger)
		},
	}

	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn or error")
	return cmd
}

func run(cmd *cobra.Command, cfg *config.Config, statePath, socketPath string, logger *slog.Logger) error {
	// OCPP 2.0.1 is a separate adapter that does not exist yet. Failing here
	// beats accepting the config and then rejecting the charger's handshake
	// with a confusing "no common version".
	if cfg.Charger.OCPPVersion != string(ocpp.Version16) {
		return fmt.Errorf("OCPP %s is not implemented yet; set charger.ocpp_version to \"1.6\"",
			cfg.Charger.OCPPVersion)
	}

	// One cpms per state file, whatever --socket says: two writers would
	// overwrite each other's reservations and tokens.
	lock, err := state.LockPath(statePath)
	if err != nil {
		return err
	}
	defer lock.Release()

	// A corrupt or newer file has to stop us before anything starts, not
	// halfway through a session.
	store, err := state.Open(statePath)
	if err != nil {
		return err
	}
	logger.Debug("state file", "path", store.Path())

	// The control socket is claimed before anything else listens, so a second
	// cpms on the same socket stops here with "already running", not later
	// with "address in use".
	ctl, err := socket.Listen(socket.Options{Path: socketPath, ChargePointID: cfg.Charger.ID, Log: logger})
	if err != nil {
		return err
	}
	defer ctl.Close()

	// Reservations and transactions from the previous run come back here,
	// with their expiry timers re-armed.
	svc := core.New(cfg, core.WithStore(store), core.WithLogger(logger))
	defer svc.Close()
	handler := v16.NewHandler(cfg, svc, logger)

	server, err := csms.New(csms.Options{
		Bind:     cfg.Server.OCPPBind,
		Core:     svc,
		Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: handler},
		NewChargePoint: func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
			switch conn.Version() {
			case ocpp.Version16:
				return v16.NewChargePoint(conn, cfg), nil
			default:
				return nil, fmt.Errorf("no command adapter for OCPP %s", conn.Version())
			}
		},
		// Exactly one station is configured, so anything else dialling in is a
		// misconfiguration worth rejecting loudly.
		Accept:      func(id string) bool { return id == cfg.Charger.ID },
		CallTimeout: cfg.Charger.CallTimeout.Duration(),
		IdleTimeout: cfg.Charger.HeartbeatTimeout.Duration(),
		Log:         logger,
	})
	if err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		return err
	}

	// Everything that acts on the station goes through control. The one-shot
	// commands reach it through the socket.
	commands := control.New(control.Options{
		Core: svc, Commands: server, Config: cfg, Log: logger,
	})
	ctl.Attach(commands, svc)

	// OCPI starts last: it is another way in to control, like the socket.
	// Until registration exists, only TOKEN_A gets anywhere, and only as far
	// as versions and the credentials stub.
	ocpiServer, err := startOCPI(cfg, logger)
	if err != nil {
		commands.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return err
	}

	events, unsubscribe := svc.Subscribe("log")
	defer unsubscribe()

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctl.Serve(ctx)

	cmd.Printf("cpms listening for %s on ws://%s/ocpp/%s\n",
		cfg.Charger.ID, server.Addr(), cfg.Charger.ID)
	cmd.Printf("OCPI versions endpoint %s (listening on %s)\n", ocpiServer.VersionsURL(), ocpiServer.Addr())
	cmd.Printf("control socket %s\n", ctl.Path())
	cmd.Println("waiting for the charge point to connect; press Ctrl-C to stop")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			logger.Info(e.String(), "kind", string(e.Kind), "charge_point", e.ChargePointID)
		}
	}()

	<-ctx.Done()
	cmd.Println("shutting down")
	// One grace period covers both listeners.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	// Stop taking commands before the station connection goes away, so no
	// command is half-sent into a closing socket.
	if err := ocpiServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("OCPI shutdown did not complete cleanly", "error", err)
	}
	ctl.Close()
	// Reserve cancels in the background; let those finish while the station
	// is still connected.
	commands.Close()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown did not complete cleanly", "error", err)
	}

	unsubscribe()
	<-done
	return nil
}

// startOCPI serves OCPI on server.ocpi_bind under the path of
// ocpi.public_base_url, with the modules that exist so far.
func startOCPI(cfg *config.Config, logger *slog.Logger) (*ocpi.Server, error) {
	s, err := ocpi.New(ocpi.Options{
		Bind:          cfg.Server.OCPIBind,
		PublicBaseURL: cfg.OCPI.PublicBaseURL,
		Auth:          ocpi.NewStaticAuthenticator(cfg.OCPI.TokenA),
		Log:           logger,
	})
	if err != nil {
		return nil, err
	}
	if err := s.Register(ocpi.CredentialsStub()); err != nil {
		return nil, err
	}
	if err := s.Start(); err != nil {
		return nil, err
	}
	return s, nil
}

// newLogger builds the structured logger the protocol layers write to.
func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "info":
		l = slog.LevelInfo
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q; want debug, info, warn or error", level)
	}

	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: l,
		// Timestamps are the only thing worth trimming here: the TUI in a later
		// step renders its own, and a terminal log is read live.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{Key: a.Key, Value: slog.StringValue(time.Now().Format("15:04:05"))}
			}
			return a
		},
	})), nil
}
