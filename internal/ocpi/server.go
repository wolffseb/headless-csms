package ocpi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// Options configure a Server.
type Options struct {
	// Bind is the listen address, e.g. "0.0.0.0:8080". A port of 0 picks a
	// free one, which tests rely on.
	Bind string
	// PublicBaseURL is the OCPI root we advertise, e.g.
	// "http://192.168.1.10:8080/ocpi". Its path is also the prefix every
	// route is served under, so what we advertise and what we serve cannot
	// disagree.
	PublicBaseURL string
	// Auth decides what each credentials token grants. Required.
	Auth Authenticator
	// Log gets one debug line per request. It never sees a token.
	Log *slog.Logger
	// Now stamps response envelopes; nil means time.Now. Tests pin it.
	Now func() time.Time
	// ReadTimeout, WriteTimeout and IdleTimeout bound reading a whole
	// request, writing the response, and an idle keep-alive connection. Zero
	// means the defaults below; tests shorten them.
	ReadTimeout, WriteTimeout, IdleTimeout time.Duration
}

// Defaults for the HTTP timeouts. Write is generous because a module handler
// may call the peer back before it answers.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
)

// Server serves OCPI over HTTP.
type Server struct {
	opts Options
	log  *slog.Logger

	// base is the advertised root with its path cleaned, and prefix is that
	// path ("" for a root URL). Both come from Options.PublicBaseURL once.
	base   string
	prefix string

	// ctx is the server's lifetime: every request context ends with it, and
	// it is cancelled when Shutdown begins.
	ctx    context.Context
	cancel context.CancelFunc

	mux  *http.ServeMux
	http *http.Server
	ln   net.Listener
	wg   sync.WaitGroup

	mu      sync.Mutex
	started bool
	modules []Module
}

// New builds a Server with the versions endpoints mounted. Modules are added
// with Register before Start.
func New(opts Options) (*Server, error) {
	if opts.Auth == nil {
		return nil, errors.New("ocpi: Auth is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	base, prefix, err := splitBaseURL(opts.PublicBaseURL)
	if err != nil {
		return nil, err
	}

	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = DefaultReadTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ctx:    ctx,
		cancel: cancel,
		opts:   opts,
		log:    opts.Log,
		base:   base,
		prefix: prefix,
		mux:    http.NewServeMux(),
	}
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       opts.ReadTimeout,
		WriteTimeout:      opts.WriteTimeout,
		IdleTimeout:       opts.IdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	// The versions module needs no Module entry: it is not listed in version
	// details, and every token that may do anything may read it.
	if err := s.handle(http.MethodGet, prefix+"/versions", AccessRegistration, s.serveVersions); err != nil {
		return nil, err
	}
	if err := s.handle(http.MethodGet, prefix+"/"+Version, AccessRegistration, s.serveVersionDetails); err != nil {
		return nil, err
	}
	return s, nil
}

// splitBaseURL cleans the operator-typed base URL once, so the advertised
// URLs and the route prefix both derive from the same value.
func splitBaseURL(raw string) (base, prefix string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("ocpi: public base URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("ocpi: public base URL %q needs a scheme and a host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("ocpi: public base URL %q must not have a query or fragment", raw)
	}
	prefix = path.Clean("/" + u.Path)
	if prefix == "/" {
		prefix = ""
	}
	// Braces would be read as ServeMux wildcards.
	if strings.ContainsAny(prefix, "{}") {
		return "", "", fmt.Errorf("ocpi: public base URL %q has braces in its path", raw)
	}
	u.Path, u.RawPath = prefix, ""
	return u.String(), prefix, nil
}

// VersionsURL is the URL handed to the counterparty out of band: the
// starting point of every OCPI connection.
func (s *Server) VersionsURL() string { return s.base + "/versions" }

// VersionURL is the version details URL for 2.3.0.
func (s *Server) VersionURL() string { return s.base + "/" + Version }

// Register mounts a module: its routes are served and it is listed in version
// details. It fails after Start, or when the module clashes with one already
// mounted.
func (s *Server) Register(m Module) error {
	if err := m.validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("ocpi: module %s registered after Start", m.Identifier)
	}
	for _, have := range s.modules {
		if have.Identifier == m.Identifier && have.Role == m.Role {
			return fmt.Errorf("ocpi: module %s/%s is already registered", m.Identifier, m.Role)
		}
	}

	root := s.prefix + "/" + Version + "/" + m.Path
	for _, r := range m.Routes {
		p := root
		if sub := strings.Trim(r.Path, "/"); sub != "" {
			p += "/" + sub
		}
		if err := s.handle(r.Method, p, m.Access, r.Handler); err != nil {
			return fmt.Errorf("ocpi: module %s: %w", m.Identifier, err)
		}
	}
	s.modules = append(s.modules, m)
	return nil
}

// handle mounts h behind the access check, both with and without a trailing
// slash: OCPI peers disagree about it (the spec's own pagination examples use
// one), and a 404 over a slash is miserable to debug.
func (s *Server) handle(method, pattern string, need Access, h http.HandlerFunc) (err error) {
	// ServeMux panics on a conflicting pattern; a module clashing with
	// another is a wiring error to report, not a crash.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("route %s %s: %v", method, pattern, p)
		}
	}()

	guarded := s.authorize(need, h)
	s.mux.Handle(method+" "+pattern, guarded)
	s.mux.Handle(method+" "+pattern+"/{$}", guarded)
	return nil
}

// authorize lets a request through only if its token grants at least need.
func (s *Server) authorize(need Access, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := tokenFromRequest(r)
		if !ok || s.opts.Auth.Access(token) < need {
			// One answer for a missing header, bad Base64, an unknown token and
			// a token without enough access: which one it was is a hint for
			// whoever is guessing.
			WriteUnauthorized(w)
			return
		}
		// The handler may have to check the token again against state that
		// changed since this decision; see TokenFromContext.
		h.ServeHTTP(w, r.WithContext(withToken(r.Context(), token)))
	})
}

// Start binds the listener and serves in the background. Registering modules
// is closed from here on.
func (s *Server) Start() error {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.opts.Bind)
	if err != nil {
		return fmt.Errorf("ocpi: listening on %s: %w", s.opts.Bind, err)
	}
	s.ln = ln

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("ocpi server stopped", "error", err)
		}
	}()

	s.log.Info("ocpi listening", "addr", ln.Addr().String(), "versions_url", s.VersionsURL())
	return nil
}

// Addr is the address actually bound, which matters when Bind used port 0.
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.opts.Bind
	}
	return s.ln.Addr().String()
}

// Context is the server's lifetime: it is cancelled when Shutdown begins.
// Module goroutines that outlive a request (command result deliveries) hang
// off it so they stop with the server.
func (s *Server) Context() context.Context { return s.ctx }

// Shutdown cancels the lifetime context, so in-flight handlers and module
// goroutines are told to stop, then stops accepting requests and waits for
// those in flight.
func (s *Server) Shutdown(ctx context.Context) error {
	s.cancel()
	err := s.http.Shutdown(ctx)

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ServeHTTP is the transport around every route: request ids, the body cap,
// the request log, and envelopes for requests no route matches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Lenient in what we accept, strict in what we send: a peer that omits
	// either id still gets both back.
	requestID := headerOrNewID(r, "X-Request-ID")
	correlationID := headerOrNewID(r, "X-Correlation-ID")
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Correlation-ID", correlationID)

	// Handlers read the ids, and the server's lifetime, from the context. The
	// lifetime is joined here as well as through BaseContext, so it holds
	// however the Server is mounted.
	ctx, cancel := context.WithCancel(WithCorrelationID(WithRequestID(r.Context(), requestID), correlationID))
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	r = r.WithContext(ctx)

	rw := &responseWriter{ResponseWriter: w, now: s.opts.Now}
	r.Body = http.MaxBytesReader(rw, r.Body, MaxBodyBytes)

	if h, pattern := s.mux.Handler(r); pattern == "" {
		s.unmatched(rw, r, h)
	} else {
		s.mux.ServeHTTP(rw, r)
	}

	// The path only: no headers, so never the Authorization one.
	s.log.Debug("ocpi request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", rw.status(),
		"duration", time.Since(start),
		"request_id", requestID,
		"correlation_id", correlationID,
	)
}

// unmatched answers a request no route takes. ServeMux knows whether that is
// a 404 or a 405 (and which methods to put in Allow), so its answer is
// captured and re-sent as an envelope.
func (s *Server) unmatched(w http.ResponseWriter, r *http.Request, h http.Handler) {
	c := &capture{header: http.Header{}}
	h.ServeHTTP(c, r)
	for _, k := range []string{"Allow", "Location"} {
		if v := c.header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	status := c.code
	if status == 0 {
		status = http.StatusNotFound
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		WriteError(w, status, StatusClientError, http.StatusText(status))
		return
	}
	w.WriteHeader(status)
}

func (s *Server) serveVersions(w http.ResponseWriter, _ *http.Request) {
	WriteOK(w, []versionEntry{{Version: Version, URL: s.VersionURL()}})
}

func (s *Server) serveVersionDetails(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	endpoints := make([]endpoint, 0, len(s.modules))
	for _, m := range s.modules {
		endpoints = append(endpoints, endpoint{
			Identifier: m.Identifier,
			Role:       m.Role,
			URL:        s.VersionURL() + "/" + m.Path,
		})
	}
	s.mu.Unlock()

	WriteOK(w, versionDetails{Version: Version, Endpoints: endpoints})
}

// versionEntry is the OCPI Version class.
type versionEntry struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// versionDetails is the data of the version details endpoint.
type versionDetails struct {
	Version   string     `json:"version"`
	Endpoints []endpoint `json:"endpoints"`
}

// endpoint is the OCPI Endpoint class.
type endpoint struct {
	Identifier string `json:"identifier"`
	Role       Role   `json:"role"`
	URL        string `json:"url"`
}

func headerOrNewID(r *http.Request, name string) string {
	if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
		return v
	}
	return newUUID()
}

// newUUID returns a random (version 4) UUID, the form OCPI advises for
// request and correlation ids. It is small enough not to need a dependency.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms.
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// responseWriter remembers the status for the request log and carries the
// server's clock to the envelope helpers.
type responseWriter struct {
	http.ResponseWriter
	now  func() time.Time
	code int
}

// WriteHeader records the first status and passes it on.
func (w *responseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write counts as an implicit 200 when no status was set, and passes the bytes on.
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *responseWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func (w *responseWriter) ocpiNow() time.Time { return w.now() }

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// capture records what ServeMux's own 404/405/redirect handler would send.
type capture struct {
	header http.Header
	code   int
}

// Header returns the headers the captured handler set.
func (c *capture) Header() http.Header { return c.header }

// WriteHeader records the first status without sending it.
func (c *capture) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}

// Write discards the body and records a 200 when no status was set.
func (c *capture) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return len(b), nil
}
