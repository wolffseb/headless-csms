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
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// shutdownGrace bounds how long we wait for connections to close on exit.
const shutdownGrace = 5 * time.Second

func newRunCommand(opts *options) *cobra.Command {
	var logLevel string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the CSMS and wait for the charger to connect",
		Long: "Starts the OCPP server and logs what the charge point does.\n\n" +
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
			return run(cmd, cfg, logger)
		},
	}

	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn or error")
	return cmd
}

func run(cmd *cobra.Command, cfg *config.Config, logger *slog.Logger) error {
	// OCPP 2.0.1 is a separate adapter that does not exist yet. Failing here
	// beats accepting the config and then rejecting the charger's handshake
	// with a confusing "no common version".
	if cfg.Charger.OCPPVersion != string(ocpp.Version16) {
		return fmt.Errorf("OCPP %s is not implemented yet; set charger.ocpp_version to \"1.6\"",
			cfg.Charger.OCPPVersion)
	}

	svc := core.New(cfg)
	handler := v16.NewHandler(cfg, svc, logger)

	server, err := csms.New(csms.Options{
		Bind:     cfg.Server.OCPPBind,
		Core:     svc,
		Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: handler},
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

	events, unsubscribe := svc.Subscribe("log")
	defer unsubscribe()

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd.Printf("cpms listening for %s on ws://%s/ocpp/%s\n",
		cfg.Charger.ID, server.Addr(), cfg.Charger.ID)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown did not complete cleanly", "error", err)
	}

	unsubscribe()
	<-done
	return nil
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
