package socket_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi/credentials"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// stuckOCPI is a registration whose Unregister waits for its context, as the
// real one does behind a registration in progress.
type stuckOCPI struct{}

func (stuckOCPI) Registration() *state.OCPIRegistration { return nil }
func (stuckOCPI) Unregister(ctx context.Context) (credentials.Unregistered, error) {
	<-ctx.Done()
	return credentials.Unregistered{}, fmt.Errorf("waiting for a registration in progress: %w", ctx.Err())
}

// Unregister is bounded by the server, below the CLI's own timeout, so the
// CLI is told what happened rather than giving up locally while the server
// goes on. Running out of time is a refusal: nothing was changed.
func TestUnregisterTimesOutBeforeTheClientDoes(t *testing.T) {
	t.Parallel()
	path := serveWith(t, context.Background(), fakeStation{}, func(o *socket.Options) {
		o.OCPI = stuckOCPI{}
		o.UnregisterTimeout = 50 * time.Millisecond
	})

	// The client would wait a hundred times as long.
	_, err := socket.Call(context.Background(), path, socket.Request{Op: socket.OpOCPIUnregister}, 5*time.Second)
	var se *socket.Error
	if !errors.As(err, &se) || se.Code != socket.CodeRefused || !strings.Contains(se.Message, "in progress") {
		t.Fatalf("err = %v, want a refusal that says a registration is in progress", err)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cpms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStation answers every command with the configured result or error.
type fakeStation struct {
	res ocpp.Result
	err error
}

func (f fakeStation) ID() string            { return "CP" }
func (f fakeStation) Version() ocpp.Version { return ocpp.Version16 }
func (f fakeStation) ReserveNow(context.Context, ocpp.ReserveRequest) (ocpp.Result, error) {
	return f.res, f.err
}
func (f fakeStation) CancelReservation(context.Context, int) (ocpp.Result, error) {
	return f.res, f.err
}
func (f fakeStation) RemoteStart(context.Context, ocpp.RemoteStartRequest) (ocpp.Result, error) {
	return f.res, f.err
}
func (f fakeStation) RemoteStop(context.Context, string) (ocpp.Result, error) { return f.res, f.err }
func (f fakeStation) UnlockConnector(context.Context, string) (ocpp.Result, error) {
	return f.res, f.err
}
func (f fakeStation) TriggerStatus(context.Context, string) (ocpp.Result, error) { return f.res, f.err }
func (f fakeStation) Capabilities(context.Context) (ocpp.Capabilities, error) {
	return ocpp.Capabilities{}, nil
}
func (f fakeStation) SetChargingProfile(context.Context, ocpp.ChargingProfile) (ocpp.Result, error) {
	return f.res, f.err
}
func (f fakeStation) ClearChargingProfile(context.Context, ocpp.ClearProfileRequest) (ocpp.Result, error) {
	return f.res, f.err
}
func (f fakeStation) GetCompositeSchedule(context.Context, string, time.Duration, *ocpp.RateUnit) (ocpp.CompositeSchedule, error) {
	return ocpp.CompositeSchedule{Status: f.res.Status, Raw: f.res.Raw}, f.err
}
func (f fakeStation) Probe(context.Context) (ocpp.ProbeReport, error) {
	return ocpp.ProbeReport{Version: ocpp.Version16}, f.err
}
func (f fakeStation) ChangeConfiguration(context.Context, string, string) (ocpp.Result, error) {
	return f.res, f.err
}

type commands struct{ station ocpp.ChargePoint }

func (c commands) Command(string) (ocpp.ChargePoint, error) {
	if c.station == nil {
		return nil, ocpp.ErrNotConnected
	}
	return c.station, nil
}

// serve starts a socket server over a station that answers with res/err, and
// returns the socket path.
func serve(t *testing.T, station ocpp.ChargePoint) string {
	t.Helper()
	return serveCtx(t, context.Background(), station)
}

func serveCtx(t *testing.T, ctx context.Context, station ocpp.ChargePoint) string {
	t.Helper()
	return serveWith(t, ctx, station, func(*socket.Options) {})
}

// serveWith is serveCtx with a say in the server's options.
func serveWith(t *testing.T, ctx context.Context, station ocpp.ChargePoint, tune func(*socket.Options)) string {
	t.Helper()

	cfg := &config.Config{
		Charger:  config.Charger{ID: "CP"},
		Auth:     config.Auth{DefaultIDTag: "TAG"},
		Location: config.Location{EVSEs: []config.EVSE{{UID: "EVSE-1", OCPPConnectorID: 1}}},
	}
	c := core.New(cfg, core.WithLogger(discard()))
	t.Cleanup(c.Close)
	ctl := control.New(control.Options{Core: c, Commands: commands{station}, Config: cfg, Log: discard()})

	path := filepath.Join(shortDir(t), "cpms.sock")
	opts := socket.Options{Path: path, ChargePointID: "CP", Log: discard()}
	tune(&opts)
	srv, err := socket.Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Attach(ctl, c)
	if opts.OCPI != nil {
		srv.AttachOCPI(opts.OCPI)
	}
	srv.Serve(ctx)
	t.Cleanup(srv.Close)
	return path
}

func call(t *testing.T, path string, req socket.Request) (json.RawMessage, error) {
	t.Helper()
	return socket.Call(context.Background(), path, req, 5*time.Second)
}

// The table that decides what a script calling cpms sees. Every station
// answer and every local refusal has exactly one code.
func TestErrorCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   socket.Op
		st   ocpp.ChargePoint
		want socket.Code
	}{
		{"rejected", socket.OpReserve, fakeStation{res: ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}}, socket.CodeRejected},
		{"undefined status", socket.OpReserve, fakeStation{res: ocpp.Result{Status: ocpp.CommandUnknown, Raw: "Maybe"}}, socket.CodeRejected},
		{"occupied", socket.OpReserve, fakeStation{res: ocpp.Result{Status: ocpp.CommandOccupied, Raw: "Occupied"}}, socket.CodeOccupied},
		{"faulted", socket.OpReserve, fakeStation{res: ocpp.Result{Status: ocpp.CommandFaulted, Raw: "Faulted"}}, socket.CodeFaulted},
		{"unavailable", socket.OpReserve, fakeStation{res: ocpp.Result{Status: ocpp.CommandUnavailable, Raw: "Unavailable"}}, socket.CodeFaulted},
		{"unlock failed", socket.OpUnlock, fakeStation{res: ocpp.Result{Status: ocpp.CommandUnlockFailed, Raw: "UnlockFailed"}}, socket.CodeUnlockFailed},
		{"not supported", socket.OpUnlock, fakeStation{res: ocpp.Result{Status: ocpp.CommandNotSupported, Raw: "NotSupported"}}, socket.CodeUnlockFailed},
		{"timeout", socket.OpReserve, fakeStation{err: fmt.Errorf("ReserveNow: %w", ocpp.ErrTimeout)}, socket.CodeTimeout},
		{"not connected", socket.OpReserve, nil, socket.CodeNotConnected},
		// Not a timeout: the command never went out.
		{"busy, not sent", socket.OpReserve, fakeStation{err: fmt.Errorf("ReserveNow: %w", ocpp.ErrNotSent)}, socket.CodeBusy},
		{"busy, not sent (unlock)", socket.OpUnlock, fakeStation{err: fmt.Errorf("UnlockConnector: %w", ocpp.ErrNotSent)}, socket.CodeBusy},
		{"busy, not sent (probe)", socket.OpProbe, fakeStation{err: fmt.Errorf("GetConfiguration: %w", ocpp.ErrNotSent)}, socket.CodeBusy},
		{"nothing to stop", socket.OpStop, fakeStation{}, socket.CodeRefused},
		{"nothing to cancel", socket.OpCancel, fakeStation{}, socket.CodeRefused},
		{"call error", socket.OpReserve, fakeStation{err: ocpp.Errorf(ocpp.ErrInternalError, "boom")}, socket.CodeInternal},
		{"probe timeout", socket.OpProbe, fakeStation{err: fmt.Errorf("GetConfiguration: %w", ocpp.ErrTimeout)}, socket.CodeTimeout},
		{"probe not connected", socket.OpProbe, nil, socket.CodeNotConnected},
		{"call error not implemented", socket.OpReserve, fakeStation{err: ocpp.Errorf(ocpp.ErrNotImplemented, "no")}, socket.CodeUnlockFailed},
		{"call error not supported", socket.OpUnlock, fakeStation{err: fmt.Errorf("UnlockConnector: %w", ocpp.Errorf(ocpp.ErrNotSupported, "no"))}, socket.CodeUnlockFailed},
		// An unknown configuration key is a refusal, not a missing command.
		{"config key not supported", socket.OpConfigSet, fakeStation{res: ocpp.Result{Status: ocpp.CommandNotSupported, Raw: "NotSupported"}}, socket.CodeRejected},
		{"config rejected", socket.OpConfigSet, fakeStation{res: ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}}, socket.CodeRejected},
		{"config not connected", socket.OpConfigSet, nil, socket.CodeNotConnected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := serve(t, tc.st)

			_, err := call(t, path, socket.Request{Op: tc.op, EVSE: "EVSE-1", Key: "HeartbeatInterval", Value: "60"})
			var se *socket.Error
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want a *socket.Error", err)
			}
			if se.Code != tc.want {
				t.Errorf("code = %s, want %s (message %q)", se.Code, tc.want, se.Message)
			}
			// A CALLERROR has no status; the CLI needs its code to name the answer.
			if strings.HasPrefix(tc.name, "call error ") && tc.want == socket.CodeUnlockFailed &&
				(se.Status != "" || se.CallError == "") {
				t.Errorf("status = %q, call_error = %q, want no status and the CALLERROR code", se.Status, se.CallError)
			}
		})
	}

	t.Run("already reserved", func(t *testing.T) {
		t.Parallel()
		path := serve(t, fakeStation{res: ocpp.Result{Status: ocpp.CommandAccepted}})
		if _, err := call(t, path, socket.Request{Op: socket.OpReserve, EVSE: "EVSE-1"}); err != nil {
			t.Fatal(err)
		}
		_, err := call(t, path, socket.Request{Op: socket.OpReserve, EVSE: "EVSE-1"})
		var se *socket.Error
		if !errors.As(err, &se) || se.Code != socket.CodeRefused {
			t.Errorf("second reserve err = %v, want code %s", err, socket.CodeRefused)
		}
	})
}

// gatedStation holds ReserveNow until released and reports a cancelled
// context as the error, like a real command on the wire would.
type gatedStation struct {
	fakeStation
	entered chan struct{}
	release chan struct{}
}

func (g gatedStation) ReserveNow(ctx context.Context, _ ocpp.ReserveRequest) (ocpp.Result, error) {
	close(g.entered)
	select {
	case <-g.release:
		return ocpp.Result{Status: ocpp.CommandAccepted}, nil
	case <-ctx.Done():
		return ocpp.Result{}, ctx.Err()
	}
}

// Ctrl-C on `cpms run` stops accepting new requests but must not cancel a
// command already sent to the station: the station may act on it, and the
// reservation would then be dropped locally.
func TestShutdownDoesNotCancelCommandsOnTheWire(t *testing.T) {
	t.Parallel()

	g := gatedStation{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := serveCtx(t, ctx, g)

	errc := make(chan error, 1)
	go func() {
		_, err := call(t, path, socket.Request{Op: socket.OpReserve, EVSE: "EVSE-1"})
		errc <- err
	}()

	<-g.entered
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(g.release)

	if err := <-errc; err != nil {
		t.Fatalf("reserve after shutdown began: %v", err)
	}
}

func TestBadRequestsAreUsageErrors(t *testing.T) {
	t.Parallel()
	path := serve(t, fakeStation{res: ocpp.Result{Status: ocpp.CommandAccepted}})

	for name, req := range map[string]socket.Request{
		"unknown op":   {Op: "explode"},
		"bad duration": {Op: socket.OpReserve, EVSE: "EVSE-1", ExpiresIn: "soon"},
		"unknown EVSE": {Op: socket.OpUnlock, EVSE: "NOPE"},
		"no key":       {Op: socket.OpConfigSet, Value: "60"},
	} {
		_, err := call(t, path, req)
		var se *socket.Error
		if !errors.As(err, &se) || se.Code != socket.CodeUsage {
			t.Errorf("%s: err = %v, want code %s", name, err, socket.CodeUsage)
		}
	}

	// A client speaking another protocol version is refused, not misread.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(`{"v":99,"op":"status"}` + "\n"))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(line, `"code":"usage"`) || !strings.Contains(line, "version 99") {
		t.Errorf("v99 request answered %q, want a usage error naming the version", line)
	}
}

func TestListenRefusesWhileAnotherServes(t *testing.T) {
	t.Parallel()
	path := serve(t, nil)

	_, err := socket.Listen(socket.Options{Path: path, Log: discard()})
	if !errors.Is(err, socket.ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	// The live instance's socket must still work.
	if _, err := call(t, path, socket.Request{Op: socket.OpStatus}); err != nil {
		t.Errorf("first instance stopped answering: %v", err)
	}
}

func TestListenLeavesNonSocketFilesAlone(t *testing.T) {
	t.Parallel()

	path := filepath.Join(shortDir(t), "cpms.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := socket.Listen(socket.Options{Path: path, Log: discard()}); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("err = %v, want a not-a-socket error", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "not a socket" {
		t.Error("the file was modified")
	}
}

func TestSocketPathLengthIsChecked(t *testing.T) {
	t.Parallel()

	long := "/tmp/" + strings.Repeat("x", socket.MaxPathLen) + ".sock"
	_, err := socket.Listen(socket.Options{Path: long, Log: discard()})
	if err == nil || !strings.Contains(err.Error(), "--socket") {
		t.Errorf("Listen err = %v, want one pointing at --socket", err)
	}
	_, err = socket.Call(context.Background(), long, socket.Request{Op: socket.OpStatus}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "--socket") {
		t.Errorf("Call err = %v, want one pointing at --socket", err)
	}
}

func TestSocketIsPrivateAndRemovedOnClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(shortDir(t), "cpms.sock")
	srv, err := socket.Listen(socket.Options{Path: path, Log: discard()})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("socket mode = %o, want 600", perm)
		}
	}

	srv.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file still there after Close: %v", err)
	}
}

func TestCallWithoutServerIsNotRunning(t *testing.T) {
	t.Parallel()

	_, err := socket.Call(context.Background(), filepath.Join(shortDir(t), "cpms.sock"),
		socket.Request{Op: socket.OpStatus}, time.Second)
	if !errors.Is(err, socket.ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}

// RebootRequired is a success: the station took the value. The CLI words it
// as a warning, so the literal status has to reach it.
func TestConfigSetPassesTheStationsAnswerOn(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"Accepted", "RebootRequired"} {
		status := ocpp.CommandAccepted
		if raw == "RebootRequired" {
			status = ocpp.CommandRebootRequired
		}
		path := serve(t, fakeStation{res: ocpp.Result{Status: status, Raw: raw}})
		out, err := call(t, path, socket.Request{Op: socket.OpConfigSet, Key: "MeterValuesSampledData", Value: "Power.Active.Import"})
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var r socket.ConfigSetResult
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatal(err)
		}
		if r != (socket.ConfigSetResult{Key: "MeterValuesSampledData", Value: "Power.Active.Import", Status: raw}) {
			t.Errorf("%s: result = %+v", raw, r)
		}
	}
}

// The smart-charging operations have their own code for "the station cannot
// do that", and a profile that cannot be sent is a usage error.
func TestProfileErrorCodes(t *testing.T) {
	t.Parallel()

	kw := 20.0
	limit := &socket.LimitRequest{KW: &kw}
	cases := []struct {
		name string
		req  socket.Request
		st   ocpp.ChargePoint
		want socket.Code
	}{
		{"not supported", socket.Request{Op: socket.OpLimitSet, Limit: limit},
			fakeStation{res: ocpp.Result{Status: ocpp.CommandNotSupported, Raw: "NotSupported"}}, socket.CodeNotSupported},
		{"call error not implemented", socket.Request{Op: socket.OpLimitShow},
			fakeStation{err: ocpp.Errorf(ocpp.ErrNotImplemented, "no")}, socket.CodeNotSupported},
		{"rejected", socket.Request{Op: socket.OpLimitSet, Limit: limit},
			fakeStation{res: ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}}, socket.CodeRejected},
		{"clear unknown", socket.Request{Op: socket.OpLimitClear},
			fakeStation{res: ocpp.Result{Status: ocpp.CommandUnknownProfile, Raw: "Unknown"}}, socket.CodeRejected},
		{"timeout", socket.Request{Op: socket.OpProfileSet, Profile: &socket.Profile{Purpose: "tx-default",
			Schedule: socket.ProfileSchedule{Unit: "A", Periods: []socket.ProfilePeriod{{Limit: 16}}}}},
			fakeStation{err: ocpp.ErrTimeout}, socket.CodeTimeout},
		{"not sent", socket.Request{Op: socket.OpLimitSet, Limit: limit},
			fakeStation{err: fmt.Errorf("SetChargingProfile: %w", ocpp.ErrNotSent)}, socket.CodeBusy},
		{"tx without a session", socket.Request{Op: socket.OpLimitSet, Limit: &socket.LimitRequest{KW: &kw, Purpose: "tx"}},
			fakeStation{}, socket.CodeRefused},
		{"both units", socket.Request{Op: socket.OpLimitSet, Limit: &socket.LimitRequest{KW: &kw, Amps: &kw}},
			fakeStation{}, socket.CodeUsage},
		{"tx-default on the station", socket.Request{Op: socket.OpLimitSet, Station: true, Limit: limit},
			fakeStation{}, socket.CodeUsage},
		{"unknown purpose", socket.Request{Op: socket.OpLimitSet, Limit: &socket.LimitRequest{KW: &kw, Purpose: "max"}},
			fakeStation{}, socket.CodeUsage},
		{"bad unit", socket.Request{Op: socket.OpLimitShow, Schedule: &socket.ScheduleRequest{Unit: "kW"}},
			fakeStation{}, socket.CodeUsage},
		{"no limit", socket.Request{Op: socket.OpLimitSet}, fakeStation{}, socket.CodeUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := serve(t, tc.st)

			tc.req.EVSE = "EVSE-1"
			_, err := call(t, path, tc.req)
			var se *socket.Error
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want a *socket.Error", err)
			}
			if se.Code != tc.want {
				t.Errorf("code = %s, want %s (message %q)", se.Code, tc.want, se.Message)
			}
		})
	}
}

// An unanswered profile carries its id to the CLI, which names it.
func TestUnansweredProfileCarriesItsID(t *testing.T) {
	t.Parallel()
	path := serve(t, fakeStation{err: ocpp.ErrTimeout})

	kw := 20.0
	_, err := call(t, path, socket.Request{Op: socket.OpLimitSet, EVSE: "EVSE-1", Limit: &socket.LimitRequest{KW: &kw}})
	var se *socket.Error
	if !errors.As(err, &se) || se.Code != socket.CodeTimeout || se.ProfileID == 0 {
		t.Fatalf("err = %#v, want a timeout naming the profile id", err)
	}
}
