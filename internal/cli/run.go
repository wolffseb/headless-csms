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
	"github.com/wolffseb/cli-cpms/internal/ocpi/credentials"
	"github.com/wolffseb/cli-cpms/internal/ocpi/locations"
	"github.com/wolffseb/cli-cpms/internal/ocpi/push"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	"github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/state"
	"github.com/wolffseb/cli-cpms/internal/tui"
)

// shutdownGrace bounds how long we wait for connections to close on exit.
const shutdownGrace = 5 * time.Second

func newRunCommand(opts *options) *cobra.Command {
	var (
		logLevel string
		headless bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the CSMS and wait for the charger to connect",
		Long: "Starts the OCPP and OCPI servers and shows what the charge point does in a\n" +
			"terminal UI: arrow keys select an EVSE, and r, c, s, x, u and t act on it.\n" +
			"With --headless, or when stdout is not a terminal, it logs lines to stderr\n" +
			"instead. While the UI is up the log goes to cpms.log next to the state file.\n\n" +
			"Remember that the charger dials us, not the other way round: point the\n" +
			"station's CSMS URL at the address printed on startup.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd, opts)
			if err != nil {
				return err
			}
			statePath := opts.resolvedStatePath()
			logger, ui, closeLog, err := setupLogging(cmd, headless, logLevel, statePath)
			if err != nil {
				return err
			}
			defer closeLog()
			return run(cmd, cfg, statePath, opts.resolvedSocketPath(statePath), logger, ui)
		},
	}

	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn or error")
	cmd.Flags().BoolVar(&headless, "headless", false,
		"print log lines to stderr instead of showing the terminal UI (the default when stdout is not a terminal)")
	return cmd
}

// run serves until ctx ends. ui is nil when running headless.
func run(cmd *cobra.Command, cfg *config.Config, statePath, socketPath string, logger *slog.Logger, ui *uiSession) error {
	// OCPP 2.0.1 is a separate adapter that does not exist yet. Failing here
	// beats accepting the config and then rejecting the charger's handshake
	// with a confusing "no common version".
	for i, ch := range cfg.Chargers {
		if ch.OCPPVersion != string(ocpp.Version16) {
			path := "charger"
			if len(cfg.Chargers) > 1 {
				path = fmt.Sprintf("chargers[%d]", i)
			}
			return fmt.Errorf("OCPP %s is not implemented yet; set %s.ocpp_version to \"1.6\"",
				ch.OCPPVersion, path)
		}
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
	// Unregistering gives up at call_timeout, well before the CLI does (see
	// socketWait), so the CLI hears what happened.
	ctl, err := socket.Listen(socket.Options{
		Path:              socketPath,
		Config:            cfg,
		UnregisterTimeout: maxCallTimeout(cfg),
		Log:               logger,
	})
	if err != nil {
		return err
	}
	defer ctl.Close()

	// Reservations and transactions from the previous run come back here,
	// with their expiry timers re-armed.
	svc := core.New(cfg, core.WithStore(store), core.WithLogger(logger))
	defer svc.Close()
	handler := v16.NewHandler(cfg, svc, logger)

	// commands is built right after the server and before it listens, so
	// OnConnect never sees it nil.
	var commands *control.Service
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
		// A station that reconnects without rebooting need not report its
		// connectors, so ask, or every EVSE reads UNKNOWN until one changes.
		OnConnect: func(ctx context.Context, cp ocpp.ChargePoint) {
			commands.RefreshStatusOnConnect(ctx, cp)
		},
		// Only configured stations are accepted, each with its own timeouts.
		Accept:         chargerPolicy{cfg}.accept,
		CallTimeoutFor: chargerPolicy{cfg}.callTimeout,
		IdleTimeoutFor: chargerPolicy{cfg}.idleTimeout,
		Log:            logger,
	})
	if err != nil {
		return err
	}

	// Everything that acts on the station goes through control. The one-shot
	// commands reach it through the socket.
	commands = control.New(control.Options{
		Core: svc, Commands: server, Config: cfg, Log: logger,
	})
	if err := server.Start(); err != nil {
		return err
	}
	ctl.Attach(commands, svc)

	// OCPI starts last: it is another way in to control, like the socket.
	// Whether TOKEN_A or TOKEN_C gets in depends on the registration in
	// state.json, which the credentials module keeps.
	registrar := credentials.New(credentials.Options{
		Store:        store,
		TokenA:       cfg.OCPI.TokenA,
		TokenC:       cfg.OCPI.TokenC,
		CountryCode:  cfg.OCPI.CountryCode,
		PartyID:      cfg.OCPI.PartyID,
		BusinessName: cfg.OCPIBusinessName(),
		Log:          logger,
	})
	ctl.AttachOCPI(registrar)
	ocpiServer, err := startOCPI(ocpiDeps{Config: cfg, Core: svc, Log: logger, Credentials: registrar})
	if err != nil {
		commands.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return err
	}

	// The events are logged in both modes: on stderr headless, and in the
	// log file under the TUI, which shows them on its own.
	events, unsubscribe := svc.Subscribe("log")
	defer unsubscribe()

	// Signal handling stays registered until run returns, so a second Ctrl-C
	// during shutdown is swallowed, whether the first was a signal or `q`.
	// Calling NotifyContext's stop earlier would restore the default action,
	// which kills cpms with a Reserve's cancel still to be sent. Quitting the
	// UI therefore cancels ctx itself, below, and leaves the signals alone.
	sigCtx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancelRun := context.WithCancel(sigCtx)
	defer cancelRun()

	// The pusher mirrors EVSE status changes to the OCPI partner. It lives
	// on ctx, so shutdown cancels a PATCH that is on the wire.
	pusher, err := push.New(push.Options{Config: cfg, Core: svc, Registration: registrar, Log: logger})
	if err != nil {
		commands.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = ocpiServer.Shutdown(shutdownCtx)
		_ = server.Shutdown(shutdownCtx)
		return err
	}
	pusher.Start(ctx)
	ctl.AttachPush(pusher)
	ctl.Serve(ctx)

	var startup []string
	for _, ch := range cfg.Chargers {
		startup = append(startup, fmt.Sprintf("cpms listening for %s on ws://%s/ocpp/%s", ch.ID, server.Addr(), ch.ID))
	}
	startup = append(startup,
		fmt.Sprintf("OCPI versions endpoint %s (listening on %s)", ocpiServer.VersionsURL(), ocpiServer.Addr()),
		"control socket "+ctl.Path())
	// How to leave depends on what is on the screen: the UI has a key, the
	// headless process a signal.
	leave := "press Ctrl-C to stop"
	if ui != nil {
		leave = "press q to quit"
	}
	if len(cfg.Chargers) > 1 {
		startup = append(startup, fmt.Sprintf("waiting for %d charge points to connect; %s", len(cfg.Chargers), leave))
	} else {
		startup = append(startup, "waiting for the charge point to connect; "+leave)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			logger.Info(e.String(), "kind", string(e.Kind), "charge_point", e.ChargePointID)
		}
	}()

	var uiErr error
	if ui == nil {
		for _, l := range startup {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		<-ctx.Done()
	} else {
		// The startup lines go to the file too, as headless would have printed
		// them, and to the pane as its first lines.
		for _, l := range startup {
			logger.Info(l)
		}
		// Quitting cancels ctx, exactly as SIGINT does, so everything below
		// is the one shutdown path.
		uiErr = tui.Run(ctx, tui.Options{
			Config: cfg, Core: svc, Control: commands,
			Registration: registrar, Push: pusher,
			Describe: func(op socket.Op, subject string, err error) string { return describe(cfg, op, subject, err) },
			Stop:     cancelRun,
			Logs:     ui.sink, LogPath: ui.logPath, Startup: startup,
			In: ui.in, Out: ui.out,
		})
		cancelRun()
	}
	fmt.Fprintln(cmd.OutOrStdout(), "shutting down")
	// Each listener gets its own grace, started when its turn comes:
	// commands.Close below can take up to four times call_timeout (a Reserve
	// and the cancel it starts each wait for a turn and then for an answer),
	// which would spend a shared one before the OCPP server got to use it.
	ocpiCtx, cancelOCPI := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelOCPI()

	// ctx is cancelled, so the pusher is already stopping its workers and
	// cancelling any PATCH in flight.
	if err := pusher.Wait(ocpiCtx); err != nil {
		logger.Warn("the OCPI status push did not stop within the grace", "error", err)
	}
	// Stop taking commands before the station connection goes away, so no
	// command is half-sent into a closing socket.
	if err := ocpiServer.Shutdown(ocpiCtx); err != nil {
		logger.Warn("OCPI shutdown did not complete cleanly", "error", err)
	}
	ctl.Close()
	// Reserve cancels in the background; let those finish while the station
	// is still connected.
	commands.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown did not complete cleanly", "error", err)
	}

	unsubscribe()
	<-done
	return uiErr
}

// ocpiDeps is everything startOCPI wires into the OCPI server. Later modules
// (registration, commands) add fields here instead of growing the parameter
// list, so work on them does not collide.
type ocpiDeps struct {
	Config *config.Config
	// Core is read by Locations for the live EVSE status.
	Core *core.Service
	Log  *slog.Logger
	// Credentials is the registration: it decides what each token grants and
	// serves the credentials module.
	Credentials *credentials.Registrar
}

// startOCPI serves OCPI on server.ocpi_bind under the path of
// ocpi.public_base_url, with the modules that exist so far.
func startOCPI(d ocpiDeps) (*ocpi.Server, error) {
	cfg := d.Config
	s, err := ocpi.New(ocpi.Options{
		Bind:          cfg.Server.OCPIBind,
		PublicBaseURL: cfg.OCPI.PublicBaseURL,
		Auth:          d.Credentials.Authenticator(),
		Log:           d.Log,
	})
	if err != nil {
		return nil, err
	}
	if err := d.Credentials.Mount(s); err != nil {
		return nil, err
	}
	// Locations needs module access, which only registration grants.
	sender, err := locations.NewSender(locations.Options{
		Config: cfg, Core: d.Core, URL: s.VersionURL() + "/locations",
	})
	if err != nil {
		return nil, err
	}
	if err := s.Register(sender.Module()); err != nil {
		return nil, err
	}
	if err := s.Start(); err != nil {
		return nil, err
	}
	return s, nil
}

// parseLevel is the slog level for --log-level.
func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q; want debug, info, warn or error", level)
	}
}

// newLogger builds the structured logger the protocol layers write to.
func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	l, err := parseLevel(level)
	if err != nil {
		return nil, err
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
