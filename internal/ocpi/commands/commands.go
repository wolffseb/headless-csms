// Package commands is the OCPI 2.3.0 Commands module with cpms as the CPO
// (the Receiver): the partner POSTs a command, and the station does it.
//
// Commands are asynchronous, which drives the design:
//
//  1. The POST is answered at once with a CommandResponse: "I can pass this
//     on" (ACCEPTED, with the time after which the partner may stop waiting) or
//     a refusal that needs no station, such as an unknown EVSE or an offline
//     charger. Nothing is sent to the station for a refusal.
//  2. An accepted command runs in the background, through control, which is
//     the only way cpms acts on a station. Its outcome is POSTed as a
//     CommandResult to the command's response_url, retried with the same
//     backoff as the status push, in a goroutine of its own, so a partner that
//     is slow to take one result never holds up the next command.
//
// A reservation that was active and then ends because the CPO cancelled it
// (cpms cancel, or another tag starting a session on the EVSE) is reported
// the same way, as CANCELED_RESERVATION, which is the spec's "reservation
// canceled by the CPO" flow. Expiry and redemption tell the partner nothing:
// it learns of both from the Locations push.
package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/client"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// subscriberName is how the reservation watcher shows up in core's drop
// counts.
const subscriberName = "ocpi-commands"

// MinReservationLead is how far ahead a reservation must end for RESERVE_NOW
// to be passed on: one that ends sooner would be expired by the station
// before it could be used.
const MinReservationLead = time.Minute

// maxIDTagLength is the OCPP 1.6 idTag limit, which a token's uid becomes.
const maxIDTagLength = 20

// maxOCPIIDLength is CiString(36), the type of the ids a command carries.
const maxOCPIIDLength = 36

// responseTimeoutSlack is what the CommandResponse's timeout adds to the
// longest a command can take, for the result to travel.
const responseTimeoutSlack = 5 * time.Second

// Controller is the part of control the module acts through.
// *control.Service satisfies it.
type Controller interface {
	Reserve(ctx context.Context, p control.ReserveParams) (core.Reservation, error)
	CancelForPartner(ctx context.Context, evseUID string, reservationID int) error
	Start(ctx context.Context, evseUID, idTag string) error
	Stop(ctx context.Context, evseUID, txID string) error
	Unlock(ctx context.Context, evseUID string) error
}

// Core is the part of core the module reads and writes. *core.Service
// satisfies it.
type Core interface {
	Subscribe(name string) (<-chan core.Event, func())
	Snapshot() core.Snapshot
	ReservationByOCPIID(ocpiID string) (core.Reservation, bool)
	AuthoriseTag(tag string, ttl time.Duration)
}

// Partner is the registered counterparty, whose response_urls get the results.
// *credentials.Registrar satisfies it.
//
// A response_url belongs to the registration it arrived under. Its results are
// posted with that registration's TOKEN_B, or not at all: after an
// unregistration, and another partner registering, the old URL must never be
// sent the new partner's token.
type Partner interface {
	// Registered identifies the current registration; the zero time means none.
	Registered() time.Time
	// ClientFor calls the partner with the TOKEN_B of the registration, or
	// reports false if that is no longer the current one.
	ClientFor(registration time.Time) (*client.Client, bool)
	// AllowsResponseURL reports whether the registration is still current and
	// the URL is on a host the partner gave in it. A command naming any other
	// URL is refused: TOKEN_C is the same for every registration, so a former
	// partner that still holds it must not get the current one's token.
	AllowsResponseURL(registration time.Time, responseURL string) bool
}

// Stopper is a scheduled callback that can be cancelled, as *time.Timer is.
type Stopper interface {
	Stop() bool
}

// Options configure a Receiver.
type Options struct {
	// Config names the Location and its EVSEs, and carries the call timeouts
	// and the retry budget (ocpi.push.max_retries).
	Config  *config.Config
	Core    Core
	Control Controller
	Partner Partner
	Log     *slog.Logger

	// AfterFunc schedules the wait before a retry; nil means time.AfterFunc.
	// Tests drive it with internal/clocktest.
	AfterFunc func(time.Duration, func()) Stopper
	// Now is the clock the reservation's expiry is judged on; nil means
	// time.Now.
	Now func() time.Time
	// Backoff is the wait before retry n (1 for the first); nil means
	// client.Backoff.
	Backoff func(retry int) time.Duration
}

// Receiver serves the Commands Receiver interface.
type Receiver struct {
	opts    Options
	log     *slog.Logger
	after   func(time.Duration, func()) Stopper
	now     func() time.Time
	backoff func(int) time.Duration

	// ctx is the OCPI server's lifetime, set by Mount: background commands
	// and result deliveries hang off it, not off the request that started
	// them, which ends the moment the CommandResponse is written.
	ctx context.Context
	wg  sync.WaitGroup
}

// New builds a Receiver. Mount it on the server.
func New(opts Options) (*Receiver, error) {
	if opts.Config == nil || opts.Core == nil || opts.Control == nil || opts.Partner == nil {
		return nil, errors.New("commands: Config, Core, Control and Partner are required")
	}
	r := &Receiver{opts: opts, log: opts.Log, after: opts.AfterFunc, now: opts.Now, backoff: opts.Backoff}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.after == nil {
		r.after = func(d time.Duration, fn func()) Stopper { return time.AfterFunc(d, fn) }
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.backoff == nil {
		r.backoff = client.Backoff
	}
	return r, nil
}

// Mount registers the module on s, and starts watching reservations. The
// watcher subscribes before this returns, so a reservation ending after it is
// never missed. Everything stops with the server's lifetime context; Wait for
// it after Shutdown.
func (r *Receiver) Mount(s *ocpi.Server) error {
	r.ctx = s.Context()
	events, unsubscribe := r.opts.Core.Subscribe(subscriberName)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer unsubscribe()
		r.watchReservations(events)
	}()

	return s.Register(ocpi.Module{
		Identifier: "commands",
		Role:       ocpi.RoleReceiver,
		Access:     ocpi.AccessModules,
		Routes:     []ocpi.Route{{Method: http.MethodPost, Path: "{command}", Handler: r.serve}},
	})
}

// Wait blocks until every background command, result delivery and the watcher
// have returned, or ctx ends.
func (r *Receiver) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// displayText is OCPI's DisplayText.
type displayText struct {
	Language string `json:"language"`
	Text     string `json:"text"`
}

func text(msg string) []displayText {
	if msg == "" {
		return nil
	}
	return []displayText{{Language: "en", Text: msg}}
}

// commandResponse is OCPI's CommandResponse.
type commandResponse struct {
	Result Response `json:"result"`
	// Timeout is the seconds after which the partner may assume no result is
	// coming. A response that is not ACCEPTED has no result coming, so it
	// says 0.
	Timeout int           `json:"timeout"`
	Message []displayText `json:"message,omitempty"`
}

// commandResult is OCPI's CommandResult.
type commandResult struct {
	Result  Result        `json:"result"`
	Message []displayText `json:"message,omitempty"`
}

// body is every field a command's POST can carry; each command reads the ones
// it has. Unknown fields are ignored, as OCPI requires.
type body struct {
	ResponseURL   string `json:"response_url"`
	Token         *token `json:"token"`
	ExpiryDate    string `json:"expiry_date"`
	ReservationID string `json:"reservation_id"`
	LocationID    string `json:"location_id"`
	EVSEUID       string `json:"evse_uid"`
	ConnectorID   string `json:"connector_id"`
	SessionID     string `json:"session_id"`

	// registration is the partner registration the command arrived under,
	// which its response_url belongs to.
	registration time.Time
}

// token is the part of OCPI's Token a command needs: the uid is what a charge
// point identifies the token by, and becomes the OCPP idTag.
type token struct {
	UID string `json:"uid"`
}

// refusal is an answer given at once, before anything is sent to a station.
type refusal struct {
	response Response
	// code is the OCPI status code; 2003 for an unknown location, else 1000.
	code    ocpi.StatusCode
	message string
}

func reject(format string, args ...any) *refusal {
	return &refusal{response: ResponseRejected, code: ocpi.StatusSuccess, message: fmt.Sprintf(format, args...)}
}

// plan is a command that passed the checks that need no station.
type plan struct {
	// charger is whose call timeout bounds the command; empty means the
	// longest of them.
	charger string
	// calls is how many station calls in a row the command can wait for, each
	// bounded by ocpp.CallBound; 0 means one. A CANCEL_RESERVATION that waits
	// for a RESERVE_NOW still out makes two.
	calls int
	// run does the command, through control. Its error is turned into the
	// result by ResultFor.
	run func(ctx context.Context) error
}

// serve handles POST {commands}/{command}.
func (r *Receiver) serve(w http.ResponseWriter, req *http.Request) {
	command := req.PathValue("command")
	var prepare func(*body) (*plan, *refusal)
	switch command {
	case "RESERVE_NOW":
		prepare = r.reserveNow
	case "CANCEL_RESERVATION":
		prepare = r.cancelReservation
	case "START_SESSION":
		prepare = r.startSession
	case "STOP_SESSION":
		prepare = r.stopSession
	case "UNLOCK_CONNECTOR":
		prepare = r.unlockConnector
	default:
		// CommandType is an open enum: a command we have not heard of is not
		// an error in the request.
		respond(w, ocpi.StatusSuccess, commandResponse{
			Result: ResponseNotSupported, Message: text(fmt.Sprintf("command %q is not supported", command)),
		})
		return
	}

	var b body
	if !ocpi.DecodeJSON(w, req, &b) {
		return
	}
	if problem := b.problem(command); problem != "" {
		ocpi.WriteError(w, http.StatusBadRequest, ocpi.StatusInvalidParameters, problem)
		return
	}

	b.registration = r.opts.Partner.Registered()
	if !r.opts.Partner.AllowsResponseURL(b.registration, b.ResponseURL) {
		// Nothing is sent to it and nothing is done: the URL's owner is not
		// the registered partner, as far as we can tell.
		r.log.Warn("refusing an OCPI command: its response_url is not on a host of the registered partner",
			"command", command, "response_host", hostOf(b.ResponseURL))
		ocpi.WriteError(w, http.StatusBadRequest, ocpi.StatusInvalidParameters,
			"response_url must be on a host the registered partner gave in its credentials (its versions URL or an endpoint)")
		return
	}
	p, ref := prepare(&b)
	if ref != nil {
		r.log.Info("refusing an OCPI command", "command", command, "response", string(ref.response), "reason", ref.message)
		respond(w, ref.code, commandResponse{Result: ref.response, Message: text(ref.message)})
		return
	}

	timeout := int(math.Ceil(p.bound(r).Seconds()) + responseTimeoutSlack.Seconds())
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.execute(ocpi.WithCorrelationID(r.ctx, ocpi.CorrelationID(req.Context())), command, b.ResponseURL, b.registration, p)
	}()
	respond(w, ocpi.StatusSuccess, commandResponse{Result: ResponseAccepted, Timeout: timeout})
}

// hostOf is the host of a URL for the log, never the path or query: those may
// carry a secret.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

// bound is the longest the plan's calls to the station can take.
func (p *plan) bound(r *Receiver) time.Duration {
	return time.Duration(max(p.calls, 1)) * ocpp.CallBound(r.callTimeout(p.charger))
}

func respond(w http.ResponseWriter, code ocpi.StatusCode, resp commandResponse) {
	msg := "Success"
	if code != ocpi.StatusSuccess {
		msg = "Unknown location"
	}
	ocpi.WriteStatus(w, code, msg, resp)
}

// problem is what is wrong with the request itself (HTTP 400, 2001): a field
// the command needs is missing or cannot be read.
func (b *body) problem(command string) string {
	if u, err := url.Parse(b.ResponseURL); err != nil || b.ResponseURL == "" || !u.IsAbs() ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "response_url must be an absolute http or https URL"
	}
	var missing string
	switch command {
	case "RESERVE_NOW":
		switch {
		case b.Token == nil || b.Token.UID == "":
			missing = "token.uid"
		case b.ExpiryDate == "":
			missing = "expiry_date"
		case b.ReservationID == "":
			missing = "reservation_id"
		case b.LocationID == "":
			missing = "location_id"
		}
		if _, err := ocpi.ParseDateTime(b.ExpiryDate); missing == "" && err != nil {
			return "expiry_date is not an OCPI DateTime"
		}
	case "CANCEL_RESERVATION":
		if b.ReservationID == "" {
			missing = "reservation_id"
		}
	case "START_SESSION":
		switch {
		case b.Token == nil || b.Token.UID == "":
			missing = "token.uid"
		case b.LocationID == "":
			missing = "location_id"
		}
	case "STOP_SESSION":
		if b.SessionID == "" {
			missing = "session_id"
		}
	case "UNLOCK_CONNECTOR":
		switch {
		case b.LocationID == "":
			missing = "location_id"
		case b.EVSEUID == "":
			missing = "evse_uid"
		case b.ConnectorID == "":
			missing = "connector_id"
		}
	}
	if missing != "" {
		return missing + " is required for " + command
	}
	for name, id := range map[string]string{
		"reservation_id": b.ReservationID, "location_id": b.LocationID, "evse_uid": b.EVSEUID,
		"connector_id": b.ConnectorID, "session_id": b.SessionID,
	} {
		if utf8.RuneCountInString(id) > maxOCPIIDLength {
			return name + " is longer than 36 characters"
		}
	}
	return ""
}

// callTimeout is how long a call to the charger waits for its answer; with no
// charger named, the longest of them.
func (r *Receiver) callTimeout(chargerID string) time.Duration {
	of := func(ch config.Charger) time.Duration {
		if d := ch.CallTimeout.Duration(); d > 0 {
			return d
		}
		return config.DefaultCallTimeout
	}
	if ch, ok := r.opts.Config.ChargerByID(chargerID); ok {
		return of(ch)
	}
	longest := config.DefaultCallTimeout
	for _, ch := range r.opts.Config.Chargers {
		longest = max(longest, of(ch))
	}
	return longest
}

// execute runs an accepted command and posts how it went.
func (r *Receiver) execute(ctx context.Context, command, responseURL string, registration time.Time, p *plan) {
	// The call timeouts are what normally end a command. This only backstops
	// a station adapter that lacks them, so it must not tie with them.
	runCtx, cancel := context.WithTimeout(ctx, p.bound(r)+responseTimeoutSlack)
	err := p.run(runCtx)
	cancel()

	result, msg := ResultFor(err)
	if err != nil {
		r.log.Info("an OCPI command did not succeed", "command", command, "result", string(result), "error", err)
	}
	r.deliver(ctx, command, responseURL, registration, commandResult{Result: result, Message: text(msg)})
}

// deliver POSTs a CommandResult to its response_url, and again, after a
// backoff, if the partner could not take it, up to ocpi.push.max_retries
// times. A partner that refuses the request itself (4xx) is not asked again.
// It runs in the caller's goroutine: callers start one per result.
//
// The URL belongs to a registration, and is posted to, with that registration's
// TOKEN_B, only while it is the current one. If the partner was unregistered
// or replaced in the meantime (including between two attempts), the result is
// dropped and logged: its URL is a former partner's, and the new partner's
// token must not be sent there.
func (r *Receiver) deliver(ctx context.Context, what, responseURL string, registration time.Time, res commandResult) {
	for retries := 0; ; retries++ {
		partner, ok := r.opts.Partner.ClientFor(registration)
		if !ok {
			r.log.Warn("dropping a command result: the partner registration it was for has changed",
				"command", what, "result", string(res.Result))
			return
		}
		_, err := partner.Post(ctx, responseURL, res)
		switch {
		case err == nil:
			r.log.Debug("posted a command result", "command", what, "result", string(res.Result))
			return
		case ctx.Err() != nil:
			// Shutting down.
			return
		case client.Permanent(err):
			r.log.Error("the partner refused a command result and it will not be retried",
				"command", what, "result", string(res.Result), "reason", client.Describe(err), "error", err)
			return
		case retries >= r.opts.Config.OCPI.Push.MaxRetries:
			r.log.Error("could not post a command result, giving up",
				"command", what, "result", string(res.Result), "attempts", retries+1,
				"reason", client.Describe(err), "error", err)
			return
		}
		r.log.Debug("posting a command result failed, will retry",
			"command", what, "retry", retries+1, "error", err)
		if !r.sleep(ctx, r.backoff(retries+1)) {
			return
		}
	}
}

// sleep waits d on the receiver's clock; it reports false if ctx ends first.
func (r *Receiver) sleep(ctx context.Context, d time.Duration) bool {
	fired := make(chan struct{})
	timer := r.after(d, func() { close(fired) })
	defer timer.Stop()
	select {
	case <-fired:
		return true
	case <-ctx.Done():
		return false
	}
}

// watchReservations tells the partner when the CPO ends a reservation the
// partner holds. core publishes the end with the reservation's response_url
// only for one that was active, and CANCEL_RESERVATION ends with a reason of
// its own, so what is left to report is ours: cpms cancel (or the TUI), and
// another tag starting a session on the reserved EVSE.
func (r *Receiver) watchReservations(events <-chan core.Event) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e.Kind != core.EventReservationEnded || e.ResponseURL == "" {
				continue
			}
			var msg string
			switch e.Detail {
			case core.ReservationCancelled:
				msg = "the reservation was cancelled by the CPO"
			case core.ReservationFailed:
				msg = "the reservation was cancelled by the CPO: a session started on the EVSE with another tag"
			default:
				continue
			}
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.deliver(ocpi.WithCorrelationID(r.ctx, ocpi.NewID()), "CANCELED_RESERVATION", e.ResponseURL, e.ResponseRegistration,
					commandResult{Result: ResultCanceledReservation, Message: text(msg)})
			}()
		}
	}
}
