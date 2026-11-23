// Package control is the one place that acts on the station. The one-shot CLI,
// OCPI Commands and the TUI all go through it, so "allocate an id, record it,
// send the command, interpret the answer" is written once.
//
// It knows no OCPP version: it talks to the station through ocpp.ChargePoint
// and records outcomes in core. Every failure comes back as a typed error
// (see errors.go), because the CLI turns them into exit codes and OCPI into
// command results.
package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// DefaultReservationLength is how long a reservation holds an EVSE when the
// caller does not say.
const DefaultReservationLength = 15 * time.Minute

// DefaultBootGrace is how long RefreshStatusOnConnect waits for a
// BootNotification before it asks a newly connected station for its
// statuses. A station that is starting up sends BootNotification as its
// first message, well within this.
const DefaultBootGrace = 5 * time.Second

// Commander hands out the command interface for a connected charge point.
// *csms.Server satisfies it.
type Commander interface {
	Command(id string) (ocpp.ChargePoint, error)
}

// Options configure a Service.
type Options struct {
	Core     *core.Service
	Commands Commander
	Config   *config.Config
	// Now is the clock used to compute expiry times. It defaults to time.Now;
	// tests pass the same fake clock they give core.
	Now func() time.Time
	// BootGrace is how long RefreshStatusOnConnect waits for a
	// BootNotification; zero means DefaultBootGrace.
	BootGrace time.Duration
	// AfterFunc schedules that wait. It defaults to time.AfterFunc; tests
	// pass a fake clock's.
	AfterFunc func(time.Duration, func()) core.Stopper
	Log       *slog.Logger
}

// Service performs actions on the configured stations.
type Service struct {
	core     *core.Service
	commands Commander
	cfg      *config.Config
	now      func() time.Time
	log      *slog.Logger

	bootGrace time.Duration
	afterFunc func(time.Duration, func()) core.Stopper

	// inflight counts Reserve calls and the background cancels they start,
	// so Close can wait for them. mu orders Add before Close's Wait: Reserve
	// adds only while closed is false, and Close sets it before it waits.
	inflight sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	// settling maps the OCPP id of a reservation whose Reserve is under way to
	// a channel closed when that Reserve has returned, which is when the
	// reservation is active or gone. A cancel waits on it. Guarded by mu.
	settling map[int]chan struct{}
	// closing is closed by Close. A cancel that was not sent stops trying
	// again when it sees it.
	closing chan struct{}
}

// New builds a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.BootGrace <= 0 {
		opts.BootGrace = DefaultBootGrace
	}
	if opts.AfterFunc == nil {
		opts.AfterFunc = func(d time.Duration, fn func()) core.Stopper { return time.AfterFunc(d, fn) }
	}
	return &Service{
		core:      opts.Core,
		commands:  opts.Commands,
		cfg:       opts.Config,
		now:       opts.Now,
		log:       opts.Log,
		bootGrace: opts.BootGrace,
		afterFunc: opts.AfterFunc,
		closing:   make(chan struct{}),
		settling:  map[int]chan struct{}{},
	}
}

// ReserveParams describe a reservation to make.
type ReserveParams struct {
	EVSEUID string
	// IDTag is the RFID tag the EVSE is held for. Empty means the configured
	// auth.default_id_tag.
	IDTag string
	// ExpiresAt is when the reservation ends. If it is zero, ExpiresIn from
	// now is used, and if that is zero too, DefaultReservationLength. OCPI
	// sends an absolute time; the CLI and TUI think in durations.
	ExpiresAt time.Time
	ExpiresIn time.Duration
	// Source is who asked: "cli", "tui" or "ocpi".
	Source string
	// OCPIReservationID and ResponseURL are kept for OCPI Commands, which
	// identifies reservations by its own id and wants the outcome posted
	// back.
	OCPIReservationID string
	ResponseURL       string
}

// Close refuses further reservations (ErrShuttingDown), then waits for the
// Reserve calls in flight and the background cancels they started. A Reserve
// takes at most ocpp.CallBound of the call timeout (a turn in the queue, then
// the wait for the answer), and its cancel, which is not tried again once
// Close has been called, takes the same again, so Close can take up to four
// call timeouts when the station is silent and busy. Call it after no more
// commands are accepted and before the station connection is closed, so the
// cancels can still be sent. It is safe to call more than once.
func (s *Service) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.closing)
	}
	s.mu.Unlock()
	s.inflight.Wait()
}

// Reserve holds an EVSE for a tag.
//
// The reservation is recorded before ReserveNow goes out, and only becomes
// active once the station accepts it. A refusal ends it straight away: the
// station never held it. After a timeout or a failure the station may still
// have received the request, so a best-effort CancelReservation follows (see
// cancelOrphan), but Reserve does not wait for it: the caller gets the
// original error at once and the record in core is ended, so nothing in
// status or events claims a reservation we hold. The record stays in core as
// cancelling, which keeps the EVSE occupied (ErrAlreadyReserved) until the
// station has answered the cancel, the connection dropped, or the cancel
// itself got no answer within the call timeout (a timed-out call is over, and
// it is not sent again). In the last two cases the cancel did not get through,
// and the reservation the station may hold is left to expire there, at the
// reservation's expiryDate, so a new reserve on that EVSE can be answered
// Occupied until then.
//
// A ReserveParams.OCPIReservationID that names a reservation we hold replaces
// it: same OCPP id, as a ReserveNow with an id the station holds replaces it
// there. If that fails, no cancel follows (it would drop the original, which
// the station may still hold). Whether the original lives on depends on what
// we know of the station. If it never saw the replacement (ErrNotSent) or
// answered no, it holds the original: we keep it, and the caller reports the
// failure. If the outcome is unknown (a timeout, or the connection went while
// the request was out), the original ends locally, and the tags of both stay
// accepted until their own expiry (see core.AuthoriseTag).
func (s *Service) Reserve(ctx context.Context, p ReserveParams) (core.Reservation, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return core.Reservation{}, ErrShuttingDown
	}
	s.inflight.Add(1)
	s.mu.Unlock()
	defer s.inflight.Done()

	if err := s.checkEVSE(p.EVSEUID); err != nil {
		return core.Reservation{}, err
	}
	cp, err := s.chargePoint(p.EVSEUID, "")
	if err != nil {
		return core.Reservation{}, err
	}

	expiresAt := p.ExpiresAt
	if expiresAt.IsZero() {
		length := p.ExpiresIn
		if length <= 0 {
			length = DefaultReservationLength
		}
		expiresAt = s.now().Add(length)
	}

	want := core.Reservation{
		OCPIReservationID: p.OCPIReservationID,
		ChargePointID:     cp.ID(),
		EVSEUID:           p.EVSEUID,
		IDTag:             s.tagOrDefault(p.IDTag),
		ExpiresAt:         expiresAt,
		Source:            p.Source,
		ResponseURL:       p.ResponseURL,
	}
	// An OCPI reservation_id we hold is replaced under the same OCPP id.
	var (
		r        core.Reservation
		replaced bool
	)
	if p.OCPIReservationID != "" {
		r, replaced, err = s.core.BeginReplacement(want)
	}
	if err == nil && !replaced {
		r, err = s.core.BeginReservation(want)
	}
	if err != nil {
		return core.Reservation{}, err
	}

	// A cancel for this reservation waits until the station has answered.
	settled := make(chan struct{})
	s.mu.Lock()
	s.settling[r.ID] = settled
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.settling, r.ID)
		s.mu.Unlock()
		close(settled)
	}()

	res, err := cp.ReserveNow(ctx, ocpp.ReserveRequest{
		ReservationID: r.ID,
		EVSEUID:       r.EVSEUID,
		IDTag:         r.IDTag,
		ExpiresAt:     r.ExpiresAt,
	})
	if errors.Is(err, ocpp.ErrNotSent) {
		if replaced {
			// The station never saw the replacement, so it still holds the
			// original, which is known, not unknown: keep it.
			s.core.RevertReplacement(r.ID)
			return core.Reservation{}, err
		}
		// The station never saw the ReserveNow, so there is nothing to
		// cancel, and a cancel for a reservation that never went out would
		// only be a second command for a station that is already busy.
		s.core.EndReservation(r.ID, core.ReservationFailed)
		return core.Reservation{}, err
	}
	if err != nil && replaced {
		// The id is the one the station holds from before: a cancel for it
		// (cancelOrphan assumes ids are never reused) would drop the original
		// reservation too, which may well still be good there. The request
		// may or may not have got through, so what the station holds now is
		// unknown: our record goes, and the tags of both reservations stay
		// accepted until their own expiry, so that whichever the station
		// holds can still be redeemed. The caller reports the failure.
		prior, _ := s.core.PriorReservation(r.ID)
		s.core.EndReservation(r.ID, endReason(err))
		now := s.now()
		s.core.AuthoriseTag(prior.IDTag, prior.ExpiresAt.Sub(now))
		s.core.AuthoriseTag(r.IDTag, r.ExpiresAt.Sub(now))
		return core.Reservation{}, err
	}
	if err != nil {
		// Ending the record before the cancel has an answer would free the
		// EVSE for a second ReserveNow that overtakes it and is answered
		// Occupied, so it stays in core as cancelling. Waiting for the
		// answer here would make the caller wait a second call timeout for
		// nothing.
		reason := endReason(err)
		s.core.OrphanReservation(r.ID, reason)
		s.inflight.Add(1) // the counter is above zero here, so this cannot race Close's Wait
		go func() {
			defer s.inflight.Done()
			s.cancelOrphan(ctx, cp, r)
			s.core.EndReservation(r.ID, reason)
		}()
		return core.Reservation{}, err
	}
	if res.Status != ocpp.CommandAccepted {
		if replaced {
			// The station answered, and said no to the replacement: the
			// original is what it holds.
			s.core.RevertReplacement(r.ID)
		} else {
			s.core.EndReservation(r.ID, core.ReservationRejected)
		}
		return core.Reservation{}, refused(OpReserve, r.EVSEUID, res)
	}

	active, ok := s.core.ActivateReservation(r.ID)
	if !ok {
		// Nothing else ends a pending reservation, so this is a bug, not a
		// race worth handling.
		return core.Reservation{}, fmt.Errorf("reservation %d vanished while it was pending", r.ID)
	}
	return active, nil
}

// orphanCancelSlack is how much longer than ocpp.CallBound of the call timeout
// cancelOrphan gives one attempt, so that the connection's own timers (the
// queue timeout, then the call timeout), not this deadline, end an attempt
// that gets no turn or no answer.
const orphanCancelSlack = time.Second

// cancelOrphan asks the station to drop a reservation whose ReserveNow timed
// out or failed. The request may have reached the station even though the
// answer did not reach us, and a reservation we have forgotten would block
// the EVSE until it expired, answering every new reserve with Occupied: for a
// truck whose driver nobody can identify.
//
// Ids are never reused, so cancelling by id cannot hit a later reservation;
// Rejected just means the station never held it. It is best effort: the
// outcome is logged, and the caller still gets the original error. It runs
// in the background (see Reserve and Close) and outlives the caller's
// context, which is typically what ended the wait. The caller ends the
// cancelling record once it returns, so it returns when the station has
// answered, when the connection dropped, when the cancel itself got no answer
// within the call timeout, or once Close has been called. In the last three
// cases the station may still hold the reservation until its expiryDate.
//
// The cancel waits for the call slot behind whatever else is on the wire,
// and a command that waited the whole queue timeout is not sent
// (ocpp.ErrNotSent): the station may still be holding the reservation, so
// the cancel is tried again, queued afresh, until it is sent or one of the
// above happens. Each attempt is bounded by ocpp.CallBound: a turn in the
// queue plus the wait for the answer.
//
// Known limitation: cp stays bound to the connection that carried the
// ReserveNow. If that connection dropped, the cancel fails at once and is not
// retried after the station reconnects, so a reservation it accepted stays
// held at the station until it expires, and new reservations on that EVSE
// are answered Occupied until then. Cancelling on reconnect would need the
// orphan ids persisted (see docs/IDEAS.md).
func (s *Service) cancelOrphan(ctx context.Context, cp ocpp.ChargePoint, r core.Reservation) {
	ctx = context.WithoutCancel(ctx)
	for {
		res, err := s.cancelAttempt(ctx, cp, r.ID)
		switch {
		case err == nil:
			s.log.Info("cancelled a reservation whose ReserveNow got no answer",
				"reservation_id", r.ID, "evse", r.EVSEUID, "status", res.Raw)
			return
		case errors.Is(err, ocpp.ErrNotConnected):
			// Also matches a cancel that was released unsent by the close.
		case errors.Is(err, ocpp.ErrNotSent) && !s.isClosed():
			s.log.Warn("the station stayed busy and the cancel of a reservation it may hold was not sent; trying again",
				"reservation_id", r.ID, "evse", r.EVSEUID, "error", err)
			continue
		}
		s.log.Warn("could not cancel a reservation that may be held at the station",
			"reservation_id", r.ID, "evse", r.EVSEUID, "error", err)
		return
	}
}

// cancelAttempt sends one CancelReservation and waits for it.
func (s *Service) cancelAttempt(ctx context.Context, cp ocpp.ChargePoint, id int) (ocpp.Result, error) {
	ch, _ := s.cfg.ChargerByID(cp.ID())
	if timeout := ch.CallTimeout.Duration(); timeout > 0 {
		// The connection's own timers are what bound a busy or silent
		// station, and their errors name the action and the timeout for the
		// log. This deadline only backstops an implementation without them,
		// so it must not tie with them, or the log would read "context
		// deadline exceeded" whenever it wins the race.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ocpp.CallBound(timeout)+orphanCancelSlack)
		defer cancel()
	}
	return cp.CancelReservation(ctx, id)
}

// isClosed reports whether Close has been called.
func (s *Service) isClosed() bool {
	select {
	case <-s.closing:
		return true
	default:
		return false
	}
}

// Cancel releases the active reservation on an EVSE.
//
// If the station rejects the cancellation, the most likely reason is that it
// no longer holds the reservation (it expired or was redeemed there first).
// Our record is then stale and would block new reservations until it timed
// out, so it ends here as well, and the refusal is reported.
func (s *Service) Cancel(ctx context.Context, evseUID string) error {
	return s.cancel(ctx, evseUID, 0, core.ReservationCancelled)
}

// CancelForPartner is Cancel for a cancellation the OCPI partner asked for,
// of the reservation with the OCPP id reservationID.
//
// The reservation ends as core.ReservationCancelledByPartner, so that the
// partner, who already has the answer to its CANCEL_RESERVATION, is not also
// told that the CPO cancelled.
//
// If that reservation's ReserveNow is still out, the cancel waits for the
// station's answer (ctx bounds the wait), and cancels the reservation if it
// became active. Without the wait there is nothing yet to cancel, and the
// reservation would go active behind the partner's back. It is that
// reservation or nothing: if it is gone, or another one has taken the EVSE,
// the cancel fails with ErrNoReservation and the other is left alone.
func (s *Service) CancelForPartner(ctx context.Context, evseUID string, reservationID int) error {
	return s.cancel(ctx, evseUID, reservationID, core.ReservationCancelledByPartner)
}

func (s *Service) cancel(ctx context.Context, evseUID string, reservationID int, reason string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	if reservationID != 0 {
		if err := s.settled(ctx, reservationID); err != nil {
			return fmt.Errorf("%s: waiting for reservation %d to be set up: %w", evseUID, reservationID, err)
		}
	}
	r, ok := s.core.ActiveReservation(evseUID)
	if !ok || (reservationID != 0 && r.ID != reservationID) {
		return fmt.Errorf("%s: %w", evseUID, ErrNoReservation)
	}
	cp, err := s.chargePoint(evseUID, "")
	if err != nil {
		return err
	}

	res, err := cp.CancelReservation(ctx, r.ID)
	if err != nil {
		return err
	}
	s.core.EndReservation(r.ID, reason)
	if res.Status != ocpp.CommandAccepted {
		s.log.Warn("station refused to cancel a reservation; dropping our record of it anyway",
			"reservation_id", r.ID, "evse", evseUID, "status", res.Raw)
		return refused(OpCancel, evseUID, res)
	}
	return nil
}

// settled waits until the Reserve of reservation id, if one is under way, has
// returned.
func (s *Service) settled(ctx context.Context, id int) error {
	s.mu.Lock()
	ch := s.settling[id]
	s.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start asks the station to start a session on an EVSE for a tag; an empty
// tag means auth.default_id_tag. It returns once the station has accepted the
// request. The session itself is reported separately, when the station sends
// it, and shows up in core.
func (s *Service) Start(ctx context.Context, evseUID, idTag string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	cp, err := s.chargePoint(evseUID, "")
	if err != nil {
		return err
	}
	remoteStartID, err := s.core.NextRemoteStartID()
	if err != nil {
		return fmt.Errorf("recording the remote start: %w", err)
	}

	res, err := cp.RemoteStart(ctx, ocpp.RemoteStartRequest{
		RemoteStartID: remoteStartID,
		EVSEUID:       evseUID,
		IDTag:         s.tagOrDefault(idTag),
	})
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandAccepted {
		return refused(OpStart, evseUID, res)
	}
	return nil
}

// Stop asks the station to end the session running on an EVSE. txID names
// the session; "" means the one active there. It must be active on that
// EVSE in core either way, so a typo cannot stop a session elsewhere.
func (s *Service) Stop(ctx context.Context, evseUID, txID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	txID, err := s.transactionOn(evseUID, txID)
	if err != nil {
		return err
	}
	cp, err := s.chargePoint(evseUID, "")
	if err != nil {
		return err
	}

	res, err := cp.RemoteStop(ctx, txID)
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandAccepted {
		return refused(OpStop, evseUID, res)
	}
	return nil
}

// transactionOn picks the session a command on an EVSE is for: txID if it is
// active there, or else the one active session.
//
// One EVSE runs one session, and core ends an older one when a new one starts
// (#73). Two can still come from a state.json written before that; guessing
// which one is meant could end the wrong session, so the caller has to name
// it.
func (s *Service) transactionOn(evseUID, txID string) (string, error) {
	txs := s.core.ActiveTransactions(evseUID)
	ids := make([]string, len(txs))
	for i, tx := range txs {
		if txID != "" && tx.ID == txID {
			return txID, nil
		}
		ids[i] = tx.ID
	}
	sort.Strings(ids)
	switch {
	case txID != "" && len(ids) == 0:
		return "", fmt.Errorf("%s: transaction %s: %w", evseUID, txID, ErrNoActiveTransaction)
	case txID != "":
		return "", fmt.Errorf("%s: transaction %s: %w here (active: %s)",
			evseUID, txID, ErrNoActiveTransaction, strings.Join(ids, ", "))
	case len(ids) == 0:
		return "", fmt.Errorf("%s: %w", evseUID, ErrNoActiveTransaction)
	case len(ids) == 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%s has %d active transactions (%s); refusing to guess, name one with --tx",
			evseUID, len(ids), strings.Join(ids, ", "))
	}
}

// Unlock releases the cable lock on an EVSE.
func (s *Service) Unlock(ctx context.Context, evseUID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	cp, err := s.chargePoint(evseUID, "")
	if err != nil {
		return err
	}

	res, err := cp.UnlockConnector(ctx, evseUID)
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandUnlocked {
		return refused(OpUnlock, evseUID, res)
	}
	return nil
}

// TriggerStatus asks the station to report an EVSE's status now. The answer
// arrives as an ordinary status change in core.
func (s *Service) TriggerStatus(ctx context.Context, evseUID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	cp, err := s.chargePoint(evseUID, "")
	if err != nil {
		return err
	}

	res, err := cp.TriggerStatus(ctx, evseUID)
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandAccepted {
		return refused(OpTriggerStatus, evseUID, res)
	}
	return nil
}

// RefreshStatusOnConnect asks a station that has just connected for the
// status of every configured EVSE, one TriggerMessage per EVSE, unless it
// sends a BootNotification within the boot grace. It is meant to run as
// csms.Options.OnConnect, with cp bound to the new connection and ctx ending
// with it.
//
// A station that reconnects without rebooting need not report anything
// (OCPP 1.6 asks for StatusNotifications only after a boot), and until it
// does every EVSE reads UNKNOWN in core and over OCPI (#53). One that boots
// reports by itself, so asking as well would only crowd its start-up; a boot
// that comes after the grace gets duplicate statuses, which change nothing.
//
// Each EVSE is asked for by its connector id, not with the connectorId left
// out: OCPP 1.6 section 5.17 reads an absent id as "the station and every
// connector", which adds a connector 0 report nobody needs and relies on a
// part of the spec stations implement unevenly.
//
// It is best effort and never retried: a station that refuses (Rejected,
// NotImplemented, or a CALLERROR because it lacks the RemoteTrigger profile)
// is logged once at INFO, and the EVSEs keep their status until the station
// reports a change. Each call is bounded by the call timeout of the
// connection.
//
// The requests go out one at a time, and ocppj.Conn.Call keeps them to one in
// flight against other callers too (#63): a command sent over the socket in
// the seconds after a reconnect waits for the TriggerMessage in flight, and
// the other way round. A TriggerMessage that does not get its turn within the
// call timeout is not sent (ocpp.ErrNotSent); it is logged like any other
// failure here and not retried.
func (s *Service) RefreshStatusOnConnect(ctx context.Context, cp ocpp.ChargePoint) {
	if _, ok := s.cfg.ChargerByID(cp.ID()); !ok {
		return
	}
	evses := s.cfg.EVSEsOf(cp.ID())
	if !s.sleep(ctx, s.bootGrace) {
		return
	}
	if s.core.BootedSinceConnect(cp.ID()) {
		s.log.Debug("station booted after connecting; not asking for its statuses", "charge_point", cp.ID())
		return
	}

	for _, e := range evses {
		res, err := cp.TriggerStatus(ctx, e.UID)
		var rpcErr *ocpp.RPCError
		switch {
		case err == nil && res.Status == ocpp.CommandAccepted:
			continue
		case err == nil:
			s.log.Info("station will not report its status on request; EVSEs keep their status until it reports a change",
				"charge_point", cp.ID(), "evse", e.UID, "status", res.Raw)
		case errors.As(err, &rpcErr):
			s.log.Info("station will not report its status on request; EVSEs keep their status until it reports a change",
				"charge_point", cp.ID(), "evse", e.UID, "error", err)
		case ctx.Err() != nil, errors.Is(err, ocpp.ErrNotConnected):
			s.log.Debug("connection ended before the station reported its status", "charge_point", cp.ID())
		default:
			s.log.Warn("could not ask the station for its status after it connected",
				"charge_point", cp.ID(), "evse", e.UID, "error", err)
		}
		return
	}
	s.log.Info("asked the station for the status of every EVSE after it connected without booting",
		"charge_point", cp.ID(), "evses", len(evses))
}

// sleep waits d on the service's clock. It reports false, early, if ctx ends
// first.
func (s *Service) sleep(ctx context.Context, d time.Duration) bool {
	fired := make(chan struct{})
	timer := s.afterFunc(d, func() { close(fired) })
	defer timer.Stop()

	select {
	case <-fired:
		return true
	case <-ctx.Done():
		return false
	}
}

// Probe asks a station what it supports and how it is set up. It changes
// nothing, on the station or in core. chargerID may be empty with one charger
// configured.
func (s *Service) Probe(ctx context.Context, chargerID string) (ocpp.ProbeReport, error) {
	cp, err := s.chargePoint("", chargerID)
	if err != nil {
		return ocpp.ProbeReport{}, err
	}
	return cp.Probe(ctx)
}

// ChangeConfiguration sets one of the station's own configuration keys; it
// does not touch config.yaml. Accepted and RebootRequired both mean the
// station took the value, so both come back as a Result for the caller to
// word; anything else is a *RefusedError.
func (s *Service) ChangeConfiguration(ctx context.Context, chargerID, key, value string) (ocpp.Result, error) {
	id, err := s.chargerFor("", chargerID)
	if err != nil {
		return ocpp.Result{}, err
	}
	cp, err := s.chargePoint("", id)
	if err != nil {
		return ocpp.Result{}, err
	}
	cc, ok := cp.(ocpp.ConfigurationChanger)
	if !ok {
		return ocpp.Result{}, fmt.Errorf("OCPP %s: %w", cp.Version(), ErrNoKeyValueConfig)
	}

	res, err := cc.ChangeConfiguration(ctx, key, value)
	if err != nil {
		return ocpp.Result{}, err
	}
	switch res.Status {
	case ocpp.CommandAccepted, ocpp.CommandRebootRequired:
		// Not the value: keys such as AuthorizationKey hold credentials.
		s.log.Info("station configuration changed", "charge_point", id, "key", key, "status", res.Raw)
		return res, nil
	default:
		return ocpp.Result{}, refused(OpConfigSet, key, res)
	}
}

// checkEVSE refuses an EVSE that is not in config before anything else
// happens: no id is allocated and nothing is sent.
func (s *Service) checkEVSE(evseUID string) error {
	if _, ok := s.cfg.EVSEByUID(evseUID); !ok {
		return fmt.Errorf("%w %q", ocpp.ErrUnknownEVSE, evseUID)
	}
	return nil
}

func (s *Service) tagOrDefault(tag string) string {
	if tag == "" {
		return s.cfg.Auth.DefaultIDTag
	}
	return tag
}
