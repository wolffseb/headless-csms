package socket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"
	"time"
)

// ErrNotRunning means nothing is listening on the socket: `cpms run` is not
// running, or is running with a different socket path.
var ErrNotRunning = errors.New("cpms run is not reachable")

// Call sends one request and waits for the response, at most until timeout.
//
// The timeout should exceed the station's call timeout, so that a slow
// station surfaces as the daemon's CodeTimeout rather than the client giving
// up first; it exists so that a hung daemon cannot hang the CLI forever.
//
// A response with ok=false is returned as *Error.
func Call(ctx context.Context, path string, req Request, timeout time.Duration) (json.RawMessage, error) {
	if err := CheckPath(path); err != nil {
		return nil, err
	}
	req.V = Version

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		reason := "cannot connect to " + path
		switch {
		case errors.Is(err, fs.ErrNotExist):
			reason = "no control socket at " + path
		case errors.Is(err, syscall.ECONNREFUSED):
			reason = "nothing is listening on " + path
		}
		return nil, fmt.Errorf("%w: %s", ErrNotRunning, reason)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("sending to cpms run: %w", err)
	}

	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("no answer from cpms run within %s: %w", timeout, err)
	}

	var resp Response
	if err := json.Unmarshal(reply, &resp); err != nil {
		return nil, fmt.Errorf("unreadable answer from cpms run: %w", err)
	}
	if !resp.OK {
		if resp.Error == nil {
			return nil, &Error{Code: CodeInternal, Message: "cpms run failed without saying why"}
		}
		return nil, resp.Error
	}
	return resp.Result, nil
}
