package tui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// logLineBuffer is how many warnings may queue before the TUI reads them. A
// full queue drops the newest: the file keeps everything, and logging must
// never block the goroutine that logs.
const logLineBuffer = 256

// LogSink carries slog records of WARN and above to the log pane. The TUI
// owns the terminal, so nothing may be written to stderr while it runs; the
// full log goes to a file and the sink mirrors what an operator must see.
type LogSink struct {
	ch chan logLine
}

type logLine struct {
	at   time.Time
	text string
}

// NewLogSink returns an empty sink.
func NewLogSink() *LogSink { return &LogSink{ch: make(chan logLine, logLineBuffer)} }

// Handler is the slog handler that feeds the sink. It only takes WARN and
// above, whatever --log-level says.
func (s *LogSink) Handler() slog.Handler { return &sinkHandler{sink: s} }

type sinkHandler struct {
	sink   *LogSink
	attrs  []slog.Attr
	prefix string
}

// Enabled takes WARN and above.
func (h *sinkHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

// Handle renders the record as "WARN message key=value", the shape the pane
// shows, with values that contain spaces quoted.
func (h *sinkHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		v := a.Value.Resolve().String()
		if strings.ContainsAny(v, " \t\"=") || v == "" {
			v = fmt.Sprintf("%q", v)
		}
		b.WriteString(" " + h.prefix + a.Key + "=" + v)
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)

	at := r.Time
	if at.IsZero() {
		at = time.Now()
	}
	select {
	case h.sink.ch <- logLine{at: at, text: b.String()}:
	default:
	}
	return nil
}

// WithAttrs keeps the attributes to print after each message.
func (h *sinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &c
}

// WithGroup prefixes later attribute keys with the group.
func (h *sinkHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix += name + "."
	return &c
}
