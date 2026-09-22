// Package csms implements the Charging Station Management System side of
// OCPP-J: the WebSocket server a charge point dials into, the RPC framing on
// top of it, and request/response correlation.
//
// It is deliberately version-agnostic. It routes (charge point, action, raw
// payload) to an ocpp.Handler selected by the negotiated WebSocket
// subprotocol and never learns a message name of its own.
package csms

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// Options configure a Server.
type Options struct {
	// Bind is the listen address, e.g. "0.0.0.0:9000". A port of 0 picks a
	// free one, which tests rely on.
	Bind string
	// Core receives connection and protocol state changes.
	Core *core.Service
	// Handlers maps each supported OCPP version to its adapter. The versions
	// present here are exactly the subprotocols offered during negotiation.
	Handlers map[ocpp.Version]ocpp.Handler
	// Accept decides whether a charge point identity may connect. A nil Accept
	// accepts every identity.
	Accept func(chargePointID string) bool
	// CallTimeout bounds an outbound request.
	CallTimeout time.Duration
	// IdleTimeout is the heartbeat watchdog: a connection with no inbound
	// traffic for this long is dropped.
	IdleTimeout time.Duration
	Log         *slog.Logger
}

// Server accepts OCPP WebSocket connections from charge points.
type Server struct {
	opts Options
	log  *slog.Logger

	upgrader websocket.Upgrader
	http     *http.Server
	ln       net.Listener

	// ctx is cancelled by Shutdown and passed to handlers.
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[string]*Conn
	wg    sync.WaitGroup
}

// New builds a Server. It does not listen until Start is called.
func New(opts Options) (*Server, error) {
	if opts.Core == nil {
		return nil, fmt.Errorf("csms: Core is required")
	}
	if len(opts.Handlers) == 0 {
		return nil, fmt.Errorf("csms: at least one handler is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 30 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 90 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:   opts,
		log:    opts.Log,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[string]*Conn),
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			// Chargers do not send an Origin header, and this listener is a
			// LAN tool, not a browser endpoint.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Start binds the listener and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.opts.Bind)
	if err != nil {
		return fmt.Errorf("csms: listening on %s: %w", s.opts.Bind, err)
	}
	s.ln = ln

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("ocpp server stopped", "error", err)
		}
	}()

	s.log.Info("ocpp listening", "addr", ln.Addr().String(), "versions", s.versionList())
	return nil
}

// Addr is the address actually bound, which matters when Bind used port 0.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.opts.Bind
	}
	return s.ln.Addr().String()
}

// Shutdown closes every connection and waits for the goroutines to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()

	s.mu.Lock()
	for _, c := range s.conns {
		c.close()
	}
	s.mu.Unlock()

	err := s.http.Shutdown(ctx)

	// http.Server.Shutdown does not wait for hijacked connections, which is
	// every WebSocket we have, so the wait group is what actually proves the
	// goroutines are gone.
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()

	select {
	case <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ChargePoint returns the live connection for an identity, if it is connected.
func (s *Server) ChargePoint(id string) (*Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.conns[id]
	return c, ok
}

// Connected lists the identities currently connected.
func (s *Server) Connected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.conns))
	for id := range s.conns {
		out = append(out, id)
	}
	return out
}

// versionList is the subprotocols we offer, in preference order.
func (s *Server) versionList() []string {
	var out []string
	for _, v := range []ocpp.Version{ocpp.Version16, ocpp.Version201} {
		if _, ok := s.opts.Handlers[v]; ok {
			out = append(out, v.Subprotocol())
		}
	}
	return out
}

// ServeHTTP performs the OCPP handshake and then runs the connection.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := chargePointID(r.URL.Path)
	if id == "" {
		s.log.Warn("connection without a charge point id", "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "charge point id missing from path", http.StatusNotFound)
		return
	}
	if s.opts.Accept != nil && !s.opts.Accept(id) {
		s.log.Warn("rejected unknown charge point", "charge_point", id, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unknown charge point", http.StatusNotFound)
		return
	}

	version, subprotocol, ok := s.negotiate(r)
	if !ok {
		// OCPP-J requires a plain HTTP failure with no subprotocol header when
		// there is no version in common; that is what tells the charger to try
		// a different one instead of retrying this handshake forever.
		s.log.Warn("no common ocpp version",
			"charge_point", id, "offered", websocket.Subprotocols(r), "supported", s.versionList())
		http.Error(w, "no supported OCPP subprotocol offered", http.StatusBadRequest)
		return
	}

	upgrader := s.upgrader
	upgrader.Subprotocols = []string{subprotocol}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written a response.
		s.log.Warn("websocket upgrade failed", "charge_point", id, "error", err)
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.serveConn(id, version, ws, r.URL.Path, r.RemoteAddr)
	}()
}

func (s *Server) serveConn(id string, version ocpp.Version, ws *websocket.Conn, urlPath, remote string) {
	conn := newConn(id, version, ws, s.log, s.opts.CallTimeout, s.opts.IdleTimeout)

	// A charger that reconnects before we noticed the old socket die would
	// otherwise leave a stale connection registered under the same identity.
	s.mu.Lock()
	if old, ok := s.conns[id]; ok {
		s.log.Info("replacing existing connection", "charge_point", id)
		old.close()
	}
	s.conns[id] = conn
	s.mu.Unlock()

	s.log.Info("charge point connected",
		"charge_point", id, "version", string(version), "path", urlPath, "remote", remote)
	s.opts.Core.Connected(id)

	reason := conn.run(s.ctx, s.opts.Handlers[version])

	s.mu.Lock()
	// We are only the current connection if nothing replaced us in the
	// meantime.
	current := s.conns[id] == conn
	if current {
		delete(s.conns, id)
	}
	s.mu.Unlock()

	if !current {
		// A superseded connection must not report the station offline: its
		// replacement is already serving this identity, and saying otherwise
		// would blank out a live station's status.
		s.log.Info("superseded connection closed", "charge_point", id, "reason", reason)
		return
	}

	s.log.Info("charge point disconnected", "charge_point", id, "reason", reason)
	s.opts.Core.Disconnected(id, reason)
}

// negotiate picks the first subprotocol the charger offered that we support,
// honouring the charger's preference order.
func (s *Server) negotiate(r *http.Request) (ocpp.Version, string, bool) {
	for _, offered := range websocket.Subprotocols(r) {
		v, ok := ocpp.VersionFromSubprotocol(offered)
		if !ok {
			continue
		}
		if _, supported := s.opts.Handlers[v]; supported {
			return v, offered, true
		}
	}
	return "", "", false
}

// chargePointID takes the identity from the last segment of the request path.
//
// Vendors disagree about the prefix — /ocpp/<id>, /<id>, and
// /steve/websocket/CentralSystemService/<id> are all in the wild — so we
// accept any prefix and log the full path. Being strict here produces a
// charger that dials in and is silently rejected, which is a miserable thing
// to debug on site.
func chargePointID(urlPath string) string {
	cleaned := strings.TrimRight(path.Clean("/"+urlPath), "/")
	id := path.Base(cleaned)
	if id == "/" || id == "." {
		return ""
	}
	return id
}
