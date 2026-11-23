// Package push mirrors EVSE status changes to the registered OCPI partner
// with PATCH calls on its Locations receiver.
//
// The design is a per-EVSE dirty flag, not a queue of events:
//
//   - One subscriber goroutine reads the core bus. On an EVSE status change it
//     marks that EVSE dirty and goes back to reading. It does no I/O, so it
//     keeps up and the bus does not drop for it in practice.
//   - One worker goroutine per EVSE owns the debounce and backoff timer. When
//     its flag is set it waits out the debounce window, then reads the
//     EVSE's current status and last_updated from core and sends them.
//
// The payload is the state at send time, never an old event's target status.
// Rapid flapping therefore coalesces into one PATCH, and an older status can
// never overwrite a newer one.
package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpi/client"
	"github.com/wolffseb/cli-cpms/internal/ocpi/credentials"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// subscriberName is how the push subscriber shows up in core's drop counts.
const subscriberName = "ocpi-push"

// Backoff bounds. The delay before retry n is baseBackoff·2^(n-1), plus up to
// a quarter of that as jitter, and never more than maxBackoff.
const (
	baseBackoff = time.Second
	maxBackoff  = 30 * time.Second
)

// Core is the part of core the pusher reads. *core.Service satisfies it.
type Core interface {
	Subscribe(name string) (<-chan core.Event, func())
	Snapshot() core.Snapshot
	// Drops is cumulative and never resets.
	Drops() map[string]uint64
}

// Registration is the part of the credentials module the pusher reads, at
// send time. *credentials.Registrar satisfies it.
type Registration interface {
	Registration() *state.OCPIRegistration
	Client() *client.Client
}

// Stopper is a scheduled callback that can be cancelled, as *time.Timer is.
type Stopper interface {
	Stop() bool
}

// Options configure a Pusher.
type Options struct {
	// Config names the Location and its EVSEs (one worker each), our country
	// code and party id for the URL, and the push tuning.
	Config *config.Config
	Core   Core
	// Registration is read at send time, so a registration made while running
	// starts pushing with the next change and an unregistration stops it.
	Registration Registration
	Log          *slog.Logger

	// AfterFunc schedules the debounce and backoff waits; nil means
	// time.AfterFunc. Tests drive it with internal/clocktest.
	AfterFunc func(time.Duration, func()) Stopper
	// Now stamps Status; nil means time.Now.
	Now func() time.Time
	// Backoff is the wait before retry n (1 for the first); nil means the
	// exponential default with jitter.
	Backoff func(retry int) time.Duration
}

// Status is what `cpms status` shows about the push.
type Status struct {
	// LastSuccess is when the last PATCH was accepted; zero if none yet.
	LastSuccess time.Time
	// LastError is why the last failing attempt failed, and LastErrorAt when.
	// A later success does not clear them: compare the two times.
	LastError   string
	LastErrorAt time.Time
	// Pending is how many EVSEs have a change waiting out its debounce or
	// backoff, or being sent.
	Pending int
}

// Pusher pushes EVSE status changes. Build it with New and run it with
// Start.
type Pusher struct {
	opts    Options
	log     *slog.Logger
	after   func(time.Duration, func()) Stopper
	now     func() time.Time
	backoff func(int) time.Duration

	workers []*worker
	byUID   map[string]*worker

	pending atomic.Int64

	mu     sync.Mutex
	status Status

	wg sync.WaitGroup
}

// worker is one EVSE's push state. Its timer is a local variable of run, not a
// field: only that goroutine ever touches it.
type worker struct {
	p   *Pusher
	uid string
	// charger is the id of the charger the EVSE hangs on.
	charger string
	// wake is the dirty flag: capacity one, set without blocking.
	wake chan struct{}
}

// New builds a Pusher. Nothing runs until Start.
func New(opts Options) (*Pusher, error) {
	if opts.Config == nil || opts.Core == nil || opts.Registration == nil {
		return nil, errors.New("push: Config, Core and Registration are required")
	}
	p := &Pusher{opts: opts, log: opts.Log, after: opts.AfterFunc, now: opts.Now, backoff: opts.Backoff}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.after == nil {
		p.after = func(d time.Duration, fn func()) Stopper { return time.AfterFunc(d, fn) }
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.backoff == nil {
		p.backoff = defaultBackoff
	}
	// EVSEs are static config, so the workers are too.
	p.byUID = make(map[string]*worker, len(opts.Config.Location.EVSEs))
	for _, e := range opts.Config.Location.EVSEs {
		w := &worker{p: p, uid: e.UID, charger: chargerOf(opts.Config, e), wake: make(chan struct{}, 1)}
		p.workers = append(p.workers, w)
		p.byUID[e.UID] = w
	}
	return p, nil
}

// chargerOf is the id of the charger e hangs on: an EVSE with no charger set
// belongs to the only charger, as in config (a Config built by hand may leave
// it empty).
func chargerOf(cfg *config.Config, e config.EVSE) string {
	if e.Charger == "" && len(cfg.Chargers) == 1 {
		return cfg.Chargers[0].ID
	}
	return e.Charger
}

// defaultBackoff is 1s, 2s, 4s… capped at 30s, with up to 25% jitter so that
// several EVSEs failing together do not retry in lockstep.
func defaultBackoff(retry int) time.Duration {
	d := baseBackoff
	for i := 1; i < retry && d < maxBackoff; i++ {
		d *= 2
	}
	d += time.Duration(rand.Int64N(int64(d)/4 + 1))
	return min(d, maxBackoff)
}

// Start subscribes to the bus and starts the workers. It returns once the
// subscription is in place, so no change after it is missed. Everything stops
// when ctx ends; Wait for it.
func (p *Pusher) Start(ctx context.Context) {
	events, unsubscribe := p.opts.Core.Subscribe(subscriberName)
	for _, w := range p.workers {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			w.run(ctx)
		}()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer unsubscribe()
		p.subscribe(ctx, events)
	}()
}

// Wait blocks until every goroutine of the pusher has returned, or ctx ends.
func (p *Pusher) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status reports the push's health.
func (p *Pusher) Status() Status {
	p.mu.Lock()
	st := p.status
	p.mu.Unlock()
	st.Pending = int(p.pending.Load())
	return st
}

// subscribe is the bus reader. It only marks EVSEs dirty: any HTTP here would
// let a slow partner back the bus up, which is the very drop the dirty flag
// exists to make harmless.
func (p *Pusher) subscribe(ctx context.Context, events <-chan core.Event) {
	var seenDrops uint64
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e.Kind == core.EventEVSEStatusChanged {
				// Found by uid (globally unique); the event must come from
				// the charger the EVSE is configured on, so a made-up
				// station's connector cannot wake a worker.
				if w, ok := p.byUID[e.EVSEUID]; ok && w.charger == e.ChargePointID {
					w.markDirty()
				}
			}
			// Drops is cumulative, so only an increase since the last look
			// means events were lost. A lost event costs one redundant PATCH
			// per EVSE, not a stale partner.
			if n := p.opts.Core.Drops()[subscriberName]; n > seenDrops {
				seenDrops = n
				p.log.Warn("the OCPI push missed bus events; pushing every EVSE again", "drops", n)
				for _, w := range p.workers {
					w.markDirty()
				}
			}
		}
	}
}

func (w *worker) markDirty() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// outcome is what one send attempt came to.
type outcome int

const (
	sent      outcome = iota // the partner took it, or there was nothing to send
	retry                    // try again after a backoff
	abandoned                // give up on this change
)

// run is the worker loop. The timer channel is nil while idle; a nil channel
// never becomes ready in a select, which is all "idle" has to mean.
//
// The debounce window is fixed: a change that arrives while a timer is armed
// does not re-arm it, so an EVSE that flaps forever still gets sent once per
// window instead of starving.
func (w *worker) run(ctx context.Context) {
	var (
		fire    <-chan struct{}
		stop    Stopper
		retries int
	)
	// arm schedules the one timer this worker has. Each arming gets its own
	// channel, so a fire that lost a race with Stop cannot be mistaken for
	// the next one.
	arm := func(d time.Duration) {
		c := make(chan struct{}, 1)
		stop = w.p.after(d, func() { c <- struct{}{} })
		fire = c
	}
	idle := func() {
		fire, stop, retries = nil, nil, 0
		w.p.pending.Add(-1)
	}
	defer func() {
		if stop != nil {
			stop.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			if fire == nil {
				w.p.pending.Add(1)
				arm(w.p.opts.Config.OCPI.Push.Debounce.Duration())
			}
			// Otherwise a debounce or backoff is already running, and what
			// it sends will include this change.
		case <-fire:
			fire, stop = nil, nil
			// Ordering: the wake token is consumed BEFORE the snapshot is
			// read. A change before this point is in the snapshot, and its
			// token is gone, so it costs no second PATCH. A change after it
			// sets a fresh token that the loop sees once this send returns,
			// and starts another round. Reading the snapshot first would
			// lose that change: its token would be swallowed here while the
			// snapshot predates it.
			select {
			case <-w.wake:
			default:
			}
			switch w.send(ctx, retries) {
			case retry:
				retries++
				arm(w.p.backoff(retries))
			default:
				idle()
			}
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// payload is the PATCH body: the OCPI EVSE fields this push carries.
type payload struct {
	Status      core.EVSEStatus `json:"status"`
	LastUpdated string          `json:"last_updated"`
}

// send makes one attempt to push the EVSE's current state. retries is how many
// retries came before it.
func (w *worker) send(ctx context.Context, retries int) outcome {
	p := w.p
	cfg := p.opts.Config

	reg := p.opts.Registration.Registration()
	if reg == nil {
		p.log.Debug("not pushing an EVSE status: not registered with an OCPI partner", "evse", w.uid)
		return sent
	}
	endpoint, ok := credentials.PushEndpoint(reg)
	if !ok {
		p.log.Debug("not pushing an EVSE status: the partner lists no locations receiver", "evse", w.uid)
		return sent
	}

	cp, ok := p.opts.Core.Snapshot().ChargePoint(w.charger)
	if !ok {
		return sent
	}
	var evse core.EVSESnapshot
	for _, e := range cp.EVSEs {
		if e.UID == w.uid {
			evse, ok = e, true
			break
		}
	}
	if !ok {
		return sent
	}

	target := patchURL(endpoint, cfg, w.uid)
	// last_updated is when the status changed, not now: a retried PATCH
	// still describes the moment it changed.
	body := payload{Status: evse.Status, LastUpdated: ocpi.FormatDateTime(evse.LastUpdated)}

	_, err := p.opts.Registration.Client().Patch(ctx, target, body)
	switch {
	case err == nil:
		p.mu.Lock()
		p.status.LastSuccess = p.now()
		p.mu.Unlock()
		p.log.Debug("pushed EVSE status", "evse", w.uid, "status", string(evse.Status))
		return sent
	case ctx.Err() != nil:
		// Shutting down: the request was cancelled, the partner did nothing
		// wrong, and nobody needs an error for it.
		return abandoned
	}

	p.recordError(err)
	if permanent(err) {
		p.log.Error("OCPI status push failed and will not be retried",
			"evse", w.uid, "status", string(evse.Status), "reason", describe(err), "error", err)
		return abandoned
	}
	if retries >= cfg.OCPI.Push.MaxRetries {
		p.log.Error("OCPI status push failed, giving up",
			"evse", w.uid, "status", string(evse.Status), "attempts", retries+1, "reason", describe(err), "error", err)
		return abandoned
	}
	p.log.Debug("OCPI status push failed, will retry", "evse", w.uid, "retry", retries+1, "error", err)
	return retry
}

func (p *Pusher) recordError(err error) {
	p.mu.Lock()
	p.status.LastError = shortReason(err)
	p.status.LastErrorAt = p.now()
	p.mu.Unlock()
}

// shortReason is why an attempt failed, for Status and so for the one line of
// `cpms status`, which cuts it short. The client's transport error opens with
// the full PATCH URL, twice, which would be all of the line; the cause is the
// innermost part ("dial tcp …: connect: connection refused"). The error log
// names the EVSE, so the URL is not lost.
func shortReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// permanent reports whether retrying cannot help: the partner refused the
// request itself. 408 and 429 are 4xx that mean "later", and everything that
// is not an answer (timeouts, refused connections) or not a success envelope
// is retried.
func permanent(err error) bool {
	var e *client.Error
	if !errors.As(err, &e) {
		return false
	}
	s := e.HTTPStatus
	return s >= 400 && s < 500 && s != http.StatusRequestTimeout && s != http.StatusTooManyRequests
}

// describe names the last HTTP status or OCPI code for the error log.
func describe(err error) string {
	var e *client.Error
	if errors.As(err, &e) {
		if e.StatusCode != 0 {
			return fmt.Sprintf("HTTP %d, OCPI status_code %d", e.HTTPStatus, e.StatusCode)
		}
		return fmt.Sprintf("HTTP %d", e.HTTPStatus)
	}
	return "no answer"
}

// patchURL is {locations_endpoint_url}/{country_code}/{party_id}/{location_id}/{evse_uid},
// with the country code and party id ours, as the CPO's.
func patchURL(endpoint string, cfg *config.Config, evseUID string) string {
	return strings.TrimRight(endpoint, "/") + "/" + strings.Join([]string{
		url.PathEscape(cfg.OCPI.CountryCode),
		url.PathEscape(cfg.OCPI.PartyID),
		url.PathEscape(cfg.Location.ID),
		url.PathEscape(evseUID),
	}, "/")
}
