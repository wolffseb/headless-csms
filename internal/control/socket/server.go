package socket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// MaxPathLen is the longest socket path accepted. The kernel limit is the
// size of sun_path minus the terminating NUL: 103 bytes on macOS and the
// BSDs, 107 on Linux. The smaller one is enforced everywhere, so a config
// that works on Linux does not fail on a Mac.
const MaxPathLen = 103

// ErrAlreadyRunning means another cpms is serving the socket.
var ErrAlreadyRunning = errors.New("another cpms is already running")

// requestReadTimeout bounds how long a client may take to send its request
// line. The request is one short line; a client that sends nothing is stuck.
const requestReadTimeout = 5 * time.Second

// Options configure a Server.
type Options struct {
	// Path is the socket file.
	Path string
	// Control and Core may be left nil at Listen and supplied with Attach
	// before Serve: the socket is claimed first, as the single-instance
	// guard, before the services it serves exist.
	Control *control.Service
	Core    *core.Service
	// ChargePointID is the configured station, whose state OpStatus reports.
	ChargePointID string
	Log           *slog.Logger
}

// Server answers requests on the socket.
type Server struct {
	opts Options
	ln   net.Listener
	log  *slog.Logger

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// Listen claims the socket path and starts accepting connections only once
// Serve is called. It is the single-instance guard:
//
//   - If something answers on the path, another cpms owns it, and Listen
//     returns ErrAlreadyRunning without touching the file.
//   - If the path is a socket nobody answers on, it is left over from a
//     cpms that did not shut down cleanly, and is replaced.
//   - If the path exists and is not a socket, it is not ours to delete.
func Listen(opts Options) (*Server, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if err := CheckPath(opts.Path); err != nil {
		return nil, err
	}

	if fi, err := os.Lstat(opts.Path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("control socket %s exists and is not a socket; "+
				"move it aside or choose another path with --socket", opts.Path)
		}
		if conn, err := net.DialTimeout("unix", opts.Path, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w (socket %s)", ErrAlreadyRunning, opts.Path)
		}
		opts.Log.Info("removing stale control socket", "path", opts.Path)
		if err := os.Remove(opts.Path); err != nil {
			return nil, fmt.Errorf("removing stale control socket %s: %w", opts.Path, err)
		}
	}

	ln, err := net.Listen("unix", opts.Path)
	if err != nil {
		return nil, fmt.Errorf("listening on control socket %s: %w", opts.Path, err)
	}
	// Go removes the socket file on Close only for listeners it created,
	// which is exactly this one; a live instance's socket is never touched.
	ln.(*net.UnixListener).SetUnlinkOnClose(true)

	// The socket drives a real charger; only the user running cpms may use
	// it. Windows has no file modes, and this is best effort there.
	if err := os.Chmod(opts.Path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restricting control socket %s: %w", opts.Path, err)
	}

	return &Server{opts: opts, ln: ln, log: opts.Log}, nil
}

// CheckPath refuses a socket path the kernel cannot bind, with a message that
// says how to fix it rather than the kernel's "invalid argument".
func CheckPath(path string) error {
	if len(path) > MaxPathLen {
		return fmt.Errorf("control socket path %s is %d bytes, longer than the %d a Unix socket allows; "+
			"choose a shorter one with --socket", path, len(path), MaxPathLen)
	}
	return nil
}

// Path is the socket file.
func (s *Server) Path() string { return s.opts.Path }

// Attach supplies the services requests are served from. Call it before
// Serve.
func (s *Server) Attach(ctl *control.Service, c *core.Service) {
	s.opts.Control, s.opts.Core = ctl, c
}

// Serve answers requests until ctx is cancelled or Close is called. It
// returns immediately; the accept loop runs in the background.
func (s *Server) Serve(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.ln.Accept()
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
					s.log.Warn("control socket accept failed", "error", err)
				}
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handle(ctx, conn)
			}()
		}
	}()

	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
}

// Close stops accepting, waits for requests in flight, and removes the socket
// file.
func (s *Server) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		s.log.Debug("control socket read failed", "error", err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var req Request
	resp := Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = &Error{Code: CodeUsage, Message: "malformed request: " + err.Error()}
	} else {
		resp = s.dispatch(ctx, req)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("encoding control socket response", "error", err)
		return
	}
	if _, err := conn.Write(append(out, '\n')); err != nil {
		s.log.Debug("control socket write failed", "error", err)
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	if req.V != Version {
		return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf(
			"protocol version %d is not supported (this cpms speaks %d); use matching cpms binaries", req.V, Version)})
	}
	// Shutdown stops new requests, not the command already on the wire: the
	// station may act on it, and abandoning it would leave cpms and the
	// station disagreeing. Commands are bounded by the call timeout.
	ctx = context.WithoutCancel(ctx)
	s.log.Debug("control request", "op", string(req.Op), "evse", req.EVSE)

	switch req.Op {
	case OpStatus:
		return ok(BuildStatus(s.opts.Core.Snapshot(), s.opts.ChargePointID))

	case OpProbe:
		report, err := s.opts.Control.Probe(ctx)
		if err != nil {
			return fail(classify(err))
		}
		return ok(BuildProbe(report, s.opts.Core.Snapshot(), s.opts.ChargePointID))

	case OpReserve:
		var expiresIn time.Duration
		if req.ExpiresIn != "" {
			d, err := time.ParseDuration(req.ExpiresIn)
			if err != nil || d <= 0 {
				return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf("invalid expiry %q: want a positive duration such as 15m", req.ExpiresIn)})
			}
			expiresIn = d
		}
		r, err := s.opts.Control.Reserve(ctx, control.ReserveParams{
			EVSEUID: req.EVSE, IDTag: req.IDTag, ExpiresIn: expiresIn, Source: "cli",
		})
		if err != nil {
			return fail(classify(err))
		}
		return ok(ReserveResult{ReservationID: r.ID, EVSE: r.EVSEUID, IDTag: r.IDTag, ExpiresAt: r.ExpiresAt.UTC()})

	case OpCancel:
		return done(s.opts.Control.Cancel(ctx, req.EVSE))
	case OpStart:
		return done(s.opts.Control.Start(ctx, req.EVSE, req.IDTag))
	case OpStop:
		return done(s.opts.Control.Stop(ctx, req.EVSE))
	case OpUnlock:
		return done(s.opts.Control.Unlock(ctx, req.EVSE))

	default:
		return fail(&Error{Code: CodeUsage, Message: fmt.Sprintf("unknown operation %q", req.Op)})
	}
}

func ok(result any) Response {
	data, err := json.Marshal(result)
	if err != nil {
		return fail(&Error{Code: CodeInternal, Message: "encoding result: " + err.Error()})
	}
	return Response{OK: true, Result: data}
}

func fail(e *Error) Response { return Response{Error: e} }

func done(err error) Response {
	if err != nil {
		return fail(classify(err))
	}
	return Response{OK: true}
}

// classify maps control's typed errors to the socket's closed set of codes.
// The CLI maps those codes to exit codes, so this table is where "what went
// wrong" becomes "what the script calling cpms sees".
func classify(err error) *Error {
	e := &Error{Code: CodeInternal, Message: err.Error()}

	var refused *control.RefusedError
	var callErr *ocpp.RPCError
	switch {
	case errors.As(err, &refused):
		e.Status = refused.Raw
		switch refused.Status {
		case ocpp.CommandOccupied:
			e.Code = CodeOccupied
		case ocpp.CommandFaulted, ocpp.CommandUnavailable:
			e.Code = CodeFaulted
		case ocpp.CommandUnlockFailed, ocpp.CommandNotSupported, ocpp.CommandNotImplemented:
			e.Code = CodeUnlockFailed
		default:
			// Rejected, and any status the spec does not define.
			e.Code = CodeRejected
		}
	case errors.As(err, &callErr) &&
		(callErr.Code == ocpp.ErrNotImplemented || callErr.Code == ocpp.ErrNotSupported):
		// The station answered with a CALLERROR instead of a status: the same
		// "cannot do that" as a NotSupported status. There is no status to
		// report, so the CALLERROR code goes in its own field.
		e.Code = CodeUnlockFailed
		e.CallError = string(callErr.Code)
	case errors.Is(err, ocpp.ErrTimeout):
		e.Code = CodeTimeout
	case errors.Is(err, ocpp.ErrNotConnected):
		e.Code = CodeNotConnected
	case errors.Is(err, control.ErrAlreadyReserved),
		errors.Is(err, control.ErrShuttingDown),
		errors.Is(err, control.ErrNoReservation),
		errors.Is(err, control.ErrNoActiveTransaction):
		e.Code = CodeRefused
	case errors.Is(err, ocpp.ErrUnknownEVSE):
		e.Code = CodeUsage
	}
	return e
}
