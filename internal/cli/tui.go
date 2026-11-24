package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/tui"
)

// DefaultLogFile is the file the log goes to while the TUI owns the screen.
// It sits next to the state file, like the control socket.
const DefaultLogFile = "cpms.log"

// uiSession is what `cpms run` needs to put the TUI on the screen. A nil
// *uiSession means headless: log lines to stderr, as before the TUI existed.
type uiSession struct {
	sink    *tui.LogSink
	logPath string
	in      io.Reader
	out     io.Writer
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// setupLogging chooses between the TUI and headless and builds the logger to
// match. Nothing may reach stderr while the TUI owns the terminal, so there
// the log goes to a file next to the state file, with warnings mirrored into
// the log pane. close releases the file; it is always safe to call.
//
// The TUI needs a terminal to draw on and to read keys from. Without one
// (cpms run > log, under systemd, in CI) it runs headless, and says so once.
func setupLogging(cmd *cobra.Command, headless bool, level, statePath string) (logger *slog.Logger, ui *uiSession, closeLog func(), err error) {
	closeLog = func() {}
	out, in := cmd.OutOrStdout(), cmd.InOrStdin()

	if !headless {
		switch {
		case !isTerminal(out):
			fmt.Fprintln(cmd.ErrOrStderr(), "stdout is not a terminal; running headless")
		case !isTerminal(in):
			fmt.Fprintln(cmd.ErrOrStderr(), "stdin is not a terminal; running headless")
		default:
			ui = &uiSession{
				sink:    tui.NewLogSink(),
				logPath: filepath.Join(filepath.Dir(statePath), DefaultLogFile),
				in:      in,
				out:     out,
			}
		}
	}

	if ui == nil {
		logger, err = newLogger(cmd.ErrOrStderr(), level)
		return logger, nil, closeLog, err
	}

	file, err := logHandler(ui.logPath, level)
	if err != nil {
		return nil, nil, closeLog, err
	}
	closeLog = func() { _ = file.f.Close() }
	return slog.New(teeHandler{file.h, ui.sink.Handler()}), ui, closeLog, nil
}

type logFile struct {
	f *os.File
	h slog.Handler
}

// logHandler appends to path at the given level. Appending, not truncating:
// the file is where to look after a crash, and a restart must not erase it.
func logHandler(path, level string) (*logFile, error) {
	lv, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	// The state directory may be new: this runs before state.Open would
	// create it, and creates it as state.LockPath does.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the log file: %w", err)
	}
	return &logFile{f: f, h: slog.NewTextHandler(f, &slog.HandlerOptions{Level: lv})}, nil
}

// teeHandler sends each record to every handler that wants it.
type teeHandler []slog.Handler

// Enabled reports whether any handler wants the level.
func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

// Handle passes the record to each handler that wants it.
func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range t {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

// WithAttrs applies the attributes to every handler.
func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

// WithGroup applies the group to every handler.
func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(name)
	}
	return out
}
