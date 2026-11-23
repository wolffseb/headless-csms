// Package tui is the terminal UI of `cpms run`: the stations and their EVSEs
// on one screen, and keys to act on the selected one.
//
// It calls control in-process, like the socket does, and never keeps charger
// state of its own: the view is rendered from a core snapshot that is taken
// again on every bus event and every second. Update never blocks; commands
// run in a tea.Cmd and come back as a message.
package tui

import (
	"context"
	"errors"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi/push"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// Controller is the part of control the TUI acts through.
// *control.Service satisfies it; tests pass a fake.
type Controller interface {
	Reserve(ctx context.Context, p control.ReserveParams) (core.Reservation, error)
	Cancel(ctx context.Context, evseUID string) error
	Start(ctx context.Context, evseUID, idTag string) error
	Stop(ctx context.Context, evseUID, txID string) error
	Unlock(ctx context.Context, evseUID string) error
	TriggerStatus(ctx context.Context, evseUID string) error
}

// Registration is the OCPI registration, for the header.
type Registration interface {
	Registration() *state.OCPIRegistration
}

// PushStatus is the health of the OCPI status push, for the header.
type PushStatus interface {
	Status() push.Status
}

// Options configure the UI.
type Options struct {
	Config  *config.Config
	Core    *core.Service
	Control Controller
	// Registration and Push feed the header. Either may be nil: the header
	// then says nothing about it (or "not registered").
	Registration Registration
	Push         PushStatus
	// Describe words a failed command. cli passes the one-shot CLI's, so the
	// two never disagree.
	Describe func(op socket.Op, subject string, err error) string
	// Stop is called when the operator quits. It cancels the context that
	// SIGINT cancels, so quitting takes the same shutdown path.
	Stop func()
	// Logs carries warnings from slog into the log pane; may be nil.
	Logs *LogSink
	// LogPath is shown in the help, as where the full log is.
	LogPath string
	// Startup is shown first in the log pane: what `cpms run` printed on start.
	Startup []string

	// Now and Location default to time.Now and time.Local; tests fix them.
	Now      func() time.Time
	Location *time.Location

	// In and Out are the terminal; nil means stdin and stdout.
	In  io.Reader
	Out io.Writer
}

// Run shows the UI until the operator quits or ctx ends. It returns nil for
// both; an error means the terminal could not be used.
func Run(ctx context.Context, opts Options) error {
	m := New(ctx, opts)
	defer m.Close()

	// Signals are the caller's: SIGINT and SIGTERM cancel ctx, which quits
	// below. Bubble Tea's own handler would race that and can deadlock its
	// shutdown (it sends a QuitMsg nobody reads any more).
	popts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithoutSignalHandler()}
	if opts.In != nil {
		popts = append(popts, tea.WithInput(opts.In))
	}
	if opts.Out != nil {
		popts = append(popts, tea.WithOutput(opts.Out))
	}
	p := tea.NewProgram(m, popts...)

	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			p.Quit()
		case <-finished:
		}
	}()

	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrInterrupted) && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	return nil
}
