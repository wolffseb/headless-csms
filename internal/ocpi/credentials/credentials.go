// Package credentials is the OCPI 2.3.0 credentials module and the
// registration handshake, with cpms as the Receiver: the counterparty starts
// it with TOKEN_A, we call back with its TOKEN_B, and from then on it uses
// TOKEN_C.
//
// Registration state lives in state.json (state.OCPI), so it survives a
// restart. Our own tokens, A and C, live in config and never change at
// runtime; see the PUT handler for what that means for the spec.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/client"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// Store is the part of state.Store the module needs, so a test can make its
// writes fail.
type Store interface {
	Get() state.State
	Update(fn func(*state.State) error) error
}

// Options configure a Registrar.
type Options struct {
	// Store holds the registration.
	Store Store
	// TokenA and TokenC are ours, from config: TOKEN_A opens registration,
	// TOKEN_C is what the counterparty uses once registered.
	TokenA, TokenC string
	// CountryCode, PartyID and BusinessName describe us in our credentials
	// object.
	CountryCode, PartyID, BusinessName string
	// HTTP sends the calls to the counterparty; nil means a default client.
	// Every call is bounded by client.DefaultTimeout.
	HTTP *http.Client
	// Log never sees a token.
	Log *slog.Logger
	// Now stamps the registration; nil means time.Now.
	Now func() time.Time
}

// Registrar is the credentials module and the registration it manages.
type Registrar struct {
	opts   Options
	log    *slog.Logger
	tokenA ocpi.Secret
	tokenC ocpi.Secret
	// client calls the counterparty with the stored TOKEN_B.
	client *client.Client

	// mu guards inflight and makes every check-then-change of the registration
	// atomic. It is only ever held for a state read or write, never across a
	// body read or a call to the counterparty: a slow partner must not be able
	// to pin it.
	mu sync.Mutex
	// inflight is non-nil while a POST or PUT calls the counterparty back, and
	// is closed when it is done. It is the single-flight "registration in
	// progress" state: other changes are refused, Unregister waits on it.
	inflight chan struct{}
	// versionsURL is ours. Mount sets it before the server starts, and it is
	// only read after.
	versionsURL string
}

// New builds a Registrar. Mount it on the server, and give the server its
// Authenticator.
func New(opts Options) *Registrar {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	r := &Registrar{
		opts:   opts,
		log:    opts.Log,
		tokenA: ocpi.NewSecret(opts.TokenA),
		tokenC: ocpi.NewSecret(opts.TokenC),
	}
	r.client = client.New(client.Options{HTTP: opts.HTTP, Log: opts.Log, Token: r.tokenB})
	return r
}

// Authenticator grants access by registration state:
//
//	                TOKEN_A        TOKEN_C
//	not registered  registration   none
//	registered      none           modules
//
// It reads the store on every request, so a registration or unregistration
// takes effect on the very next request.
func (r *Registrar) Authenticator() ocpi.Authenticator {
	return ocpi.AuthenticatorFunc(func(token string) ocpi.Access {
		return r.accessFor(r.opts.Store.Get().OCPI, token)
	})
}

// accessFor is what token grants while reg is the registration.
func (r *Registrar) accessFor(reg *state.OCPIRegistration, token string) ocpi.Access {
	if reg != nil {
		if r.tokenC.Matches(token) {
			return ocpi.AccessModules
		}
		return ocpi.AccessNone
	}
	if r.tokenA.Matches(token) {
		return ocpi.AccessRegistration
	}
	return ocpi.AccessNone
}

// holds reports whether the token req carries is still the right one for
// reg: TOKEN_A while unregistered, TOKEN_C once registered. The Authenticator
// decided on the state at the time the request arrived; by the time a handler
// runs, a registration, update or unregistration may have changed it, and a
// token that was good then must not be honoured against the new state. It
// fails closed when the request carries no token.
func (r *Registrar) holds(req *http.Request, reg *state.OCPIRegistration) bool {
	token, ok := ocpi.TokenFromContext(req.Context())
	return ok && r.accessFor(reg, token) != ocpi.AccessNone
}

// Mount registers the credentials module on s.
func (r *Registrar) Mount(s *ocpi.Server) error {
	r.versionsURL = s.VersionsURL()
	return s.Register(ocpi.Module{
		Identifier: "credentials",
		// The role has no function for credentials; the spec advises SENDER.
		Role:   ocpi.RoleSender,
		Access: ocpi.AccessRegistration,
		Routes: []ocpi.Route{
			{Method: http.MethodGet, Handler: r.serveGet},
			{Method: http.MethodPost, Handler: r.servePost},
			{Method: http.MethodPut, Handler: r.servePut},
			{Method: http.MethodDelete, Handler: r.serveDelete},
		},
	})
}

// Registration is the current registration, or nil. It holds TOKEN_B: show
// it to nobody.
func (r *Registrar) Registration() *state.OCPIRegistration {
	return r.opts.Store.Get().OCPI
}

// Client calls the registered counterparty with its TOKEN_B, read fresh on
// every call. Calls made while unregistered carry an empty token, which the
// counterparty refuses.
func (r *Registrar) Client() *client.Client { return r.client }

func (r *Registrar) tokenB() string {
	if reg := r.opts.Store.Get().OCPI; reg != nil {
		return reg.TokenB
	}
	return ""
}

// errNotRegistered is what Unregister returns when there is nothing to undo.
var errNotRegistered = errors.New("not registered with an OCPI partner")

// ErrNotRegistered reports whether err means there was no registration.
func ErrNotRegistered(err error) bool { return errors.Is(err, errNotRegistered) }

// Unregistered is what Unregister did.
type Unregistered struct {
	// Party is the counterparty that was registered, e.g. "DE*ABC".
	Party string
	// NotifyError is why telling the counterparty failed, or empty when it
	// was told (or lists no credentials endpoint to tell).
	NotifyError string
}

// Unregister forgets the registration, so TOKEN_A opens registration again
// and the counterparty can start over. It is the operator's way to
// re-initiate the handshake: we are the Receiver, so we cannot start one
// ourselves.
//
// A registration or update in progress is waited for, so the two cannot
// undo each other. ctx bounds that wait: when it ends first, nothing has
// changed and the error wraps ctx's. The caller picks a deadline shorter than
// its own, so that what it reports is what happened.
//
// The registration is cleared first, then the counterparty is told with a
// DELETE on its credentials endpoint, as the spec's unregistration. Telling
// it is best effort: the operator asked to forget, and a counterparty that
// was not told finds out on its next call with TOKEN_C. Running out of time
// during the DELETE is such a failure: the registration is already gone.
func (r *Registrar) Unregister(ctx context.Context) (Unregistered, error) {
	reg, err := r.forget(ctx)
	if err != nil {
		return Unregistered{}, err
	}

	out := Unregistered{Party: Party(reg)}
	r.log.Info("ocpi unregistered by the operator", "party", out.Party)
	if ep, ok := findEndpoint(reg.Endpoints, "credentials", ""); ok {
		if _, err := r.client.WithToken(reg.TokenB).Delete(ctx, ep.URL); err != nil {
			out.NotifyError = err.Error()
			r.log.Warn("could not tell the OCPI partner we unregistered", "party", out.Party, "error", err)
		}
	}
	return out, nil
}

// forget clears the registration once no registration is in progress, and
// returns what it cleared.
func (r *Registrar) forget(ctx context.Context) (*state.OCPIRegistration, error) {
	for {
		r.mu.Lock()
		if r.inflight == nil {
			defer r.mu.Unlock()
			reg := r.opts.Store.Get().OCPI
			if reg == nil {
				return nil, errNotRegistered
			}
			err := r.opts.Store.Update(func(st *state.State) error {
				st.OCPI = nil
				return nil
			})
			return reg, err
		}
		done := r.inflight
		r.mu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for a registration in progress: %w", ctx.Err())
		}
	}
}

// Party names the counterparty by its roles' country code and party id, e.g.
// "DE*ABC", or "DE*ABC, NL*XYZ" for a platform with several.
func Party(reg *state.OCPIRegistration) string {
	seen := map[string]bool{}
	var parties []string
	for _, role := range reg.Roles {
		p := role.CountryCode + "*" + role.PartyID
		if !seen[p] {
			seen[p] = true
			parties = append(parties, p)
		}
	}
	return strings.Join(parties, ", ")
}

// PushEndpoint is the counterparty's Locations receiver, which status changes
// are pushed to, if it has one.
func PushEndpoint(reg *state.OCPIRegistration) (string, bool) {
	ep, ok := findEndpoint(reg.Endpoints, "locations", string(ocpi.RoleReceiver))
	return ep.URL, ok
}

// findEndpoint looks up a module endpoint; an empty role matches any.
func findEndpoint(eps []state.Endpoint, identifier, role string) (state.Endpoint, bool) {
	for _, ep := range eps {
		if ep.Identifier == identifier && (role == "" || ep.Role == role) {
			return ep, true
		}
	}
	return state.Endpoint{}, false
}

// serveGet answers with our credentials. The token in them is the one the
// caller authenticated with, which the registration state decides: TOKEN_A
// before registration and TOKEN_C after. It is read from one snapshot of the
// state, so a caller whose token went stale while the request was on its way
// is refused instead of being handed the other one.
func (r *Registrar) serveGet(w http.ResponseWriter, req *http.Request) {
	reg := r.opts.Store.Get().OCPI
	if !r.holds(req, reg) {
		ocpi.WriteUnauthorized(w)
		return
	}
	token := r.opts.TokenA
	if reg != nil {
		token = r.opts.TokenC
	}
	ocpi.WriteOK(w, r.ours(token))
}

// begin starts a change that calls the counterparty: POST registers, PUT
// updates. It re-checks, under the lock and against the state as it is now,
// that the caller's token is still the right one and the method is allowed,
// and claims the single-flight slot. On success it returns the registration
// before the change and a finish func to call when done. Otherwise it has
// answered.
//
// A change refused because another is in progress, and only then (the state's
// own refusals come first), is answered with HTTP 200
// and OCPI status 2000 "retry shortly", the way every other registration
// failure here is answered. The spec has no status for it, and 405 would be
// wrong: the method is allowed, just not right now. The partner's retry is
// the remedy, and the change in progress is not disturbed.
func (r *Registrar) begin(w http.ResponseWriter, req *http.Request) (old *state.OCPIRegistration, finish func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old = r.opts.Store.Get().OCPI
	if !r.holds(req, old) {
		ocpi.WriteUnauthorized(w)
		return nil, nil, false
	}
	// The 405s come before busy: a change in progress leaves a registration
	// that exists still existing, and one that does not still unregistered
	// until it lands, so a refused method is refused whatever it does and
	// "retry shortly" would send the partner after something that cannot work.
	switch {
	case req.Method == http.MethodPost && old != nil:
		r.notAllowed(w, true, "already registered; use PUT to update, or DELETE to unregister first")
		return nil, nil, false
	case req.Method == http.MethodPut && old == nil:
		r.notAllowed(w, false, "not registered; use POST to register")
		return nil, nil, false
	}
	if r.busy(w) {
		return nil, nil, false
	}

	done := make(chan struct{})
	r.inflight = done
	return old, func() {
		r.mu.Lock()
		r.inflight = nil
		r.mu.Unlock()
		close(done)
	}, true
}

// busy answers a change refused because another is in progress. The caller
// holds mu.
func (r *Registrar) busy(w http.ResponseWriter) bool {
	if r.inflight == nil {
		return false
	}
	ocpi.WriteError(w, http.StatusOK, ocpi.StatusClientError,
		"another registration change is in progress; retry shortly")
	return true
}

// servePost is registration: call the counterparty back with its TOKEN_B,
// store what it offers, and hand it TOKEN_C.
func (r *Registrar) servePost(w http.ResponseWriter, req *http.Request) {
	// The body is read before anything is locked: how long it takes is the
	// sender's business.
	creds, ok := decodeCredentials(w, req)
	if !ok {
		return
	}
	_, finish, ok := r.begin(w, req)
	if !ok {
		return
	}
	defer finish()

	// Nothing is stored until both calls back have succeeded: a token B
	// without endpoints is worse than no registration.
	reg, code, msg := r.fetch(req.Context(), creds)
	if code != 0 {
		r.log.Warn("ocpi registration failed", "versions_url", creds.URL, "status_code", int(code), "reason", msg)
		ocpi.WriteError(w, http.StatusOK, code, msg)
		return
	}
	reg.RegisteredAt = r.opts.Now()

	if err := r.opts.Store.Update(func(st *state.State) error {
		st.OCPI = reg
		return nil
	}); err != nil {
		r.log.Error("ocpi registration could not be stored", "error", err)
		ocpi.WriteError(w, http.StatusInternalServerError, ocpi.StatusServerError, "registration could not be stored")
		return
	}

	r.logRegistered("ocpi registered", reg)
	ocpi.WriteOK(w, r.ours(r.opts.TokenC))
}

// servePut updates the registration: a new TOKEN_B and freshly fetched
// endpoints.
//
// Deviation from the spec, by decision: OCPI says a PUT must answer with a
// newly generated TOKEN_C. Ours lives in config, which cpms never writes, and
// a generated one would have to live in state.json as a second source of
// truth for our own token. So the PUT answers with the same TOKEN_C. Rotating
// it is an operator action: change ocpi.token_c, restart, and have the
// counterparty register again.
func (r *Registrar) servePut(w http.ResponseWriter, req *http.Request) {
	creds, ok := decodeCredentials(w, req)
	if !ok {
		return
	}
	old, finish, ok := r.begin(w, req)
	if !ok {
		return
	}
	defer finish()

	reg, code, msg := r.fetch(req.Context(), creds)
	if code != 0 {
		r.log.Warn("ocpi registration update failed", "versions_url", creds.URL, "status_code", int(code), "reason", msg)
		ocpi.WriteError(w, http.StatusOK, code, msg)
		return
	}
	// "Registered since" stays the first registration.
	reg.RegisteredAt = old.RegisteredAt

	if err := r.opts.Store.Update(func(st *state.State) error {
		st.OCPI = reg
		return nil
	}); err != nil {
		r.log.Error("ocpi registration update could not be stored", "error", err)
		ocpi.WriteError(w, http.StatusInternalServerError, ocpi.StatusServerError, "registration could not be stored")
		return
	}

	r.logRegistered("ocpi registration updated", reg)
	ocpi.WriteOK(w, r.ours(r.opts.TokenC))
}

// serveDelete is the counterparty unregistering: TOKEN_C stops working and
// TOKEN_A works again. It only touches state, so it holds the lock for the
// write.
func (r *Registrar) serveDelete(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reg := r.opts.Store.Get().OCPI
	if !r.holds(req, reg) {
		ocpi.WriteUnauthorized(w)
		return
	}
	if reg == nil {
		r.notAllowed(w, false, "not registered")
		return
	}
	if r.busy(w) {
		return
	}
	if err := r.opts.Store.Update(func(st *state.State) error {
		st.OCPI = nil
		return nil
	}); err != nil {
		r.log.Error("ocpi unregistration could not be stored", "error", err)
		ocpi.WriteError(w, http.StatusInternalServerError, ocpi.StatusServerError, "unregistration could not be stored")
		return
	}
	r.log.Info("ocpi unregistered by the partner", "party", Party(reg))
	ocpi.WriteOK(w, nil)
}

// notAllowed is the spec's HTTP 405 for a method the registration state does
// not allow, with the methods it does.
func (r *Registrar) notAllowed(w http.ResponseWriter, registered bool, msg string) {
	allow := "GET, POST"
	if registered {
		allow = "GET, PUT, DELETE"
	}
	w.Header().Set("Allow", allow)
	ocpi.WriteError(w, http.StatusMethodNotAllowed, ocpi.StatusClientError, msg)
}

func (r *Registrar) logRegistered(msg string, reg *state.OCPIRegistration) {
	r.log.Info(msg, "party", Party(reg), "versions_url", reg.VersionsURL, "endpoints", len(reg.Endpoints))
	if _, ok := PushEndpoint(reg); !ok {
		// Pull-only is a valid integration; it just means nothing to push to.
		r.log.Warn("the OCPI partner lists no locations receiver; status changes will not be pushed",
			"party", Party(reg))
	}
}

// ours is our credentials object, carrying token.
func (r *Registrar) ours(token string) credentialsObject {
	return credentialsObject{
		Token: token,
		URL:   r.versionsURL,
		Roles: []credentialsRole{{
			Role:            "CPO",
			BusinessDetails: businessDetails{Name: r.opts.BusinessName},
			PartyID:         r.opts.PartyID,
			CountryCode:     r.opts.CountryCode,
		}},
	}
}

// fetch calls the counterparty back with its TOKEN_B: its versions, then the
// details of 2.3.0. A non-zero code is the OCPI status to answer with.
func (r *Registrar) fetch(ctx context.Context, creds credentialsObject) (*state.OCPIRegistration, ocpi.StatusCode, string) {
	c := r.client.WithToken(creds.Token)

	resp, err := c.Get(ctx, creds.URL)
	if err != nil {
		return nil, ocpi.StatusClientAPIUnusable, "could not get your versions: " + describe(err)
	}
	var versions []versionEntry
	if err := resp.DecodeData(&versions); err != nil {
		return nil, ocpi.StatusClientAPIUnusable, "your versions endpoint did not answer with a list of versions"
	}
	var detailsURL string
	for _, v := range versions {
		if v.Version == ocpi.Version {
			detailsURL = v.URL
		}
	}
	if detailsURL == "" {
		return nil, ocpi.StatusUnsupportedVersion, "you offer no OCPI " + ocpi.Version + ", the only version we speak"
	}
	if !absoluteURL(detailsURL) {
		return nil, ocpi.StatusClientAPIUnusable, fmt.Sprintf("your %s version URL %q is not an absolute URL", ocpi.Version, detailsURL)
	}

	resp, err = c.Get(ctx, detailsURL)
	if err != nil {
		return nil, ocpi.StatusClientAPIUnusable, "could not get your " + ocpi.Version + " version details: " + describe(err)
	}
	var details versionDetails
	if err := resp.DecodeData(&details); err != nil {
		return nil, ocpi.StatusClientAPIUnusable, "your version details endpoint did not answer with version details"
	}

	endpoints := make([]state.Endpoint, 0, len(details.Endpoints))
	for _, ep := range details.Endpoints {
		endpoints = append(endpoints, state.Endpoint{Identifier: ep.Identifier, Role: ep.Role, URL: ep.URL})
	}
	roles := make([]state.PartyRole, 0, len(creds.Roles))
	for _, role := range creds.Roles {
		roles = append(roles, state.PartyRole{CountryCode: role.CountryCode, PartyID: role.PartyID, Role: role.Role})
	}
	return &state.OCPIRegistration{
		TokenB:      creds.Token,
		VersionsURL: creds.URL,
		Version:     ocpi.Version,
		Endpoints:   endpoints,
		Roles:       roles,
	}, 0, ""
}

// describe says why a call back failed, for the status message the
// counterparty reads.
func describe(err error) string {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return "unreachable"
}

// decodeCredentials reads and checks the counterparty's credentials object.
// On failure it has answered HTTP 400 with 2001.
func decodeCredentials(w http.ResponseWriter, req *http.Request) (credentialsObject, bool) {
	var creds credentialsObject
	if !ocpi.DecodeJSON(w, req, &creds) {
		return creds, false
	}
	if msg := creds.problem(); msg != "" {
		ocpi.WriteError(w, http.StatusBadRequest, ocpi.StatusInvalidParameters, msg)
		return creds, false
	}
	return creds, true
}

// MaxTokenLen is the length of a credentials token: OCPI's string(64).
const MaxTokenLen = 64

// ValidToken reports whether token is what OCPI allows for a credentials
// token: 1 to 64 printable non-whitespace ASCII characters (U+0021-U+007E).
func ValidToken(token string) bool {
	if token == "" || len(token) > MaxTokenLen {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

// problem says what is wrong with a credentials object, or "" when nothing
// is.
func (c credentialsObject) problem() string {
	switch {
	case c.Token == "":
		return "token is required"
	case !ValidToken(c.Token):
		return "token must be at most 64 printable ASCII characters without whitespace"
	case c.URL == "":
		return "url is required"
	case !absoluteURL(c.URL):
		return "url must be an absolute http or https URL"
	case len(c.Roles) == 0:
		return "roles must list at least one role"
	}
	for i, role := range c.Roles {
		if role.Role == "" || role.PartyID == "" || role.CountryCode == "" {
			return fmt.Sprintf("roles[%d] needs role, party_id and country_code", i)
		}
	}
	return ""
}

func absoluteURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// credentialsObject is the OCPI Credentials object.
type credentialsObject struct {
	Token      string            `json:"token"`
	URL        string            `json:"url"`
	HubPartyID string            `json:"hub_party_id,omitempty"`
	Roles      []credentialsRole `json:"roles"`
}

// credentialsRole is the OCPI CredentialsRole class.
type credentialsRole struct {
	Role            string          `json:"role"`
	BusinessDetails businessDetails `json:"business_details"`
	PartyID         string          `json:"party_id"`
	CountryCode     string          `json:"country_code"`
}

// businessDetails is the part of OCPI BusinessDetails we send. Theirs may
// carry a website and a logo, which we do not keep.
type businessDetails struct {
	Name string `json:"name"`
}

// versionEntry is the OCPI Version class, as the counterparty lists it.
type versionEntry struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// versionDetails is the data of the counterparty's version details.
type versionDetails struct {
	Version   string `json:"version"`
	Endpoints []struct {
		Identifier string `json:"identifier"`
		Role       string `json:"role"`
		URL        string `json:"url"`
	} `json:"endpoints"`
}
