package core

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReservationStatus is where a reservation is in its lifecycle.
type ReservationStatus string

// The reservation statuses. A reservation that has ended is removed, so there
// is no "ended" status: the EventReservationEnded event records how it ended.
const (
	// ReservationPending means ReserveNow has been sent and the station has
	// not answered. Pending reservations are never persisted: if cpms dies
	// with one in flight, the outcome is unknown, and forgetting it is the
	// honest choice.
	ReservationPending ReservationStatus = "pending"
	// ReservationActive means the station accepted it.
	ReservationActive ReservationStatus = "active"
	// ReservationCancelling means ReserveNow got no usable answer, so the
	// station may still hold the reservation, and a CancelReservation is on
	// its way. It no longer counts as a reservation we hold (it is not
	// ActiveReservation, never expires and is never persisted), but it
	// occupies the EVSE so that a second ReserveNow cannot overtake the
	// cancel and be answered Occupied.
	ReservationCancelling ReservationStatus = "cancelling"
)

// Why a reservation ended, as carried in EventReservationEnded.Detail.
const (
	ReservationExpired   = "expired"
	ReservationCancelled = "cancelled"
	// ReservationCancelledByPartner is a cancellation the OCPI partner asked
	// for with CANCEL_RESERVATION. It is its own reason, not "cancelled", so
	// that the partner is not also told that the CPO cancelled.
	ReservationCancelledByPartner = "cancelled by partner"
	ReservationConsumed           = "consumed"
	ReservationRejected           = "rejected"
	ReservationTimeout            = "timeout"
	// ReservationFailed means the reservation ended for a reason that is
	// neither a refusal nor a timeout. Either the command failed (the
	// connection dropped, the station sent a CALLERROR, or the station was
	// too busy for the command to be sent), or a session
	// started on the EVSE with another tag, so the station no longer holds the
	// reservation or a group tag consumed it.
	ReservationFailed = "failed"
)

// ErrAlreadyReserved means the EVSE already holds a reservation: pending,
// active or being cancelled. It is refused locally, before anything is sent.
var ErrAlreadyReserved = errors.New("EVSE already has a reservation")

// Reservation holds an EVSE for an RFID tag until it expires.
type Reservation struct {
	// ID is the OCPP reservation id: an integer we choose, never reused.
	ID int
	// OCPIReservationID is set when the reservation came in over OCPI, which
	// identifies reservations by its own string id.
	OCPIReservationID string
	ChargePointID     string
	EVSEUID           string
	ConnectorID       int
	IDTag             string
	ExpiresAt         time.Time
	CreatedAt         time.Time
	// Source is who asked for it: "cli", "tui" or "ocpi".
	Source string
	// ResponseURL is where OCPI wants the outcome of its command posted.
	ResponseURL string
	Status      ReservationStatus
}

// reservation is a Reservation plus the expiry timer that only core sees.
type reservation struct {
	Reservation
	timer Stopper
	// prior is, while a replacement is pending, the reservation it replaces
	// as the station still holds it. It is what the file lists, what its
	// driver may redeem, and what RevertReplacement puts back.
	prior *Reservation
}

func (r *reservation) stopTimer() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// BeginReservation records a reservation about to be sent to the station and
// returns it with its id. It is pending until ActivateReservation or
// EndReservation.
//
// The id counter is persisted before this returns. If that fails, the
// reservation is dropped and the error returned, so the caller sends nothing:
// a reservation we cannot remember is worse than one we did not make.
func (s *Service) BeginReservation(r Reservation) (Reservation, error) {
	s.mu.Lock()
	connectorID, ok := s.connectorID(r.EVSEUID)
	if !ok {
		s.mu.Unlock()
		return Reservation{}, fmt.Errorf("unknown EVSE %q", r.EVSEUID)
	}
	if s.reservationOnLocked(r.EVSEUID) != nil {
		s.mu.Unlock()
		return Reservation{}, fmt.Errorf("%s: %w", r.EVSEUID, ErrAlreadyReserved)
	}

	r.ID = s.nextReservationID
	s.nextReservationID++
	r.ConnectorID = connectorID
	r.CreatedAt = s.now()
	r.Status = ReservationPending
	s.reservations[r.ID] = &reservation{Reservation: r}
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		s.mu.Lock()
		delete(s.reservations, r.ID)
		s.mu.Unlock()
		return Reservation{}, fmt.Errorf("recording the reservation: %w", err)
	}
	return r, nil
}

// BeginReplacement is BeginReservation for a reservation the OCPI partner
// already holds: r.OCPIReservationID names an active reservation (compared
// case-insensitively, as OCPI's CiString is), and that one is replaced, keeping
// its OCPP id, because a ReserveNow with an id the station holds replaces it
// there too. It is pending again until ActivateReservation or EndReservation,
// carrying r's tag, expiry, source and response URL; the expiry timer is
// stopped. The reservation it replaces is kept (PriorReservation): until the
// station has answered it is what the file lists and what its tag may
// redeem, and RevertReplacement brings it back.
//
// It reports false, with no error, when no reservation has that OCPI id, so
// the caller goes on with BeginReservation. A reservation that is pending or
// being cancelled counts as held, and so does one on another EVSE than r's:
// both come back as ErrAlreadyReserved, wrapped, because the station would be
// asked to move a reservation, which 1.6 does not define.
func (s *Service) BeginReplacement(r Reservation) (Reservation, bool, error) {
	s.mu.Lock()
	old := s.reservationByOCPIIDLocked(r.OCPIReservationID)
	if old == nil {
		s.mu.Unlock()
		return Reservation{}, false, nil
	}
	if old.Status != ReservationActive {
		s.mu.Unlock()
		return Reservation{}, false, fmt.Errorf("reservation %q is still being set up: %w", r.OCPIReservationID, ErrAlreadyReserved)
	}
	if old.EVSEUID != r.EVSEUID {
		s.mu.Unlock()
		return Reservation{}, false, fmt.Errorf("reservation %q is held on %s, not %s: %w",
			r.OCPIReservationID, old.EVSEUID, r.EVSEUID, ErrAlreadyReserved)
	}

	before := old.Reservation
	old.prior = &before
	old.stopTimer()
	old.IDTag, old.ExpiresAt, old.Source, old.ResponseURL = r.IDTag, r.ExpiresAt, r.Source, r.ResponseURL
	old.CreatedAt = s.now()
	old.Status = ReservationPending
	out := old.Reservation
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		// Put the held reservation back: nothing was sent.
		s.mu.Lock()
		if cur, ok := s.reservations[before.ID]; ok && cur.Status == ReservationPending {
			cur.Reservation, cur.prior = before, nil
			s.armLocked(cur)
		}
		s.mu.Unlock()
		return Reservation{}, false, fmt.Errorf("recording the reservation: %w", err)
	}
	return out, true, nil
}

// PriorReservation is the reservation a pending replacement replaces, as the
// station held it before. It reports false for any other reservation.
func (s *Service) PriorReservation(id int) (Reservation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.reservations[id]
	if !ok || r.Status != ReservationPending || r.prior == nil {
		return Reservation{}, false
	}
	return *r.prior, true
}

// RevertReplacement gives up a pending replacement because the station never
// saw it, or refused it: the reservation it replaced is active again, as it
// was, with its expiry re-armed, and is returned. Nothing is published, since
// nothing ended. It reports false if id is not a pending replacement.
func (s *Service) RevertReplacement(id int) (Reservation, bool) {
	s.mu.Lock()
	r, ok := s.reservations[id]
	if !ok || r.Status != ReservationPending || r.prior == nil {
		s.mu.Unlock()
		return Reservation{}, false
	}
	r.Reservation, r.prior = *r.prior, nil
	s.armLocked(r)
	out := r.Reservation
	s.mu.Unlock()

	s.persistOrLog("reverted replacement", id)
	return out, true
}

// ReservationByOCPIID finds a reservation by the id the OCPI partner gave it,
// compared case-insensitively, in any status.
func (s *Service) ReservationByOCPIID(ocpiID string) (Reservation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r := s.reservationByOCPIIDLocked(ocpiID)
	if r == nil {
		return Reservation{}, false
	}
	return r.Reservation, true
}

func (s *Service) reservationByOCPIIDLocked(ocpiID string) *reservation {
	if ocpiID == "" {
		return nil
	}
	for _, r := range s.reservations {
		if strings.EqualFold(r.OCPIReservationID, ocpiID) {
			return r
		}
	}
	return nil
}

// ActivateReservation marks a pending reservation as accepted by the
// station, persists it and arms its expiry.
//
// The station already holds the reservation by now, so a failed write is
// logged rather than undone.
func (s *Service) ActivateReservation(id int) (Reservation, bool) {
	s.mu.Lock()
	r, ok := s.reservations[id]
	if !ok || r.Status != ReservationPending {
		s.mu.Unlock()
		return Reservation{}, false
	}
	r.Status = ReservationActive
	r.prior = nil
	s.armLocked(r)
	out := r.Reservation
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		s.log.Error("could not persist an accepted reservation; it will be lost on restart",
			"reservation_id", id, "error", err)
	}
	s.bus.publish(Event{
		Kind: EventReservationCreated, At: s.now(), ChargePointID: out.ChargePointID,
		EVSEUID: out.EVSEUID, ReservationID: out.ID, IDTag: out.IDTag,
		Detail: out.ExpiresAt.UTC().Format(time.RFC3339),
	})
	return out, true
}

// EndReservation removes a reservation, in any status, and reports why
// with EventReservationEnded. It returns false if there was no such
// reservation, for instance because it expired a moment earlier.
func (s *Service) EndReservation(id int, reason string) (Reservation, bool) {
	s.mu.Lock()
	r, ok := s.reservations[id]
	if !ok {
		s.mu.Unlock()
		return Reservation{}, false
	}
	cancelling := r.Status == ReservationCancelling
	event := s.endLocked(r, reason)
	s.mu.Unlock()

	if cancelling {
		// Already reported by OrphanReservation, and never persisted.
		return r.Reservation, true
	}
	s.persistOrLog("ended reservation", id)
	s.bus.publish(event)
	return r.Reservation, true
}

// OrphanReservation marks a pending reservation whose ReserveNow timed out or
// failed as cancelling and reports why with EventReservationEnded, as
// EndReservation would: from here on nothing claims we hold it. The record
// stays, so the EVSE stays occupied, until the caller has an answer to its
// CancelReservation and calls EndReservation, which then removes the record
// without a second event. It returns false if there is no such pending
// reservation.
func (s *Service) OrphanReservation(id int, reason string) (Reservation, bool) {
	s.mu.Lock()
	r, ok := s.reservations[id]
	if !ok || r.Status != ReservationPending {
		s.mu.Unlock()
		return Reservation{}, false
	}
	r.stopTimer()
	r.Status = ReservationCancelling
	out := r.Reservation
	s.mu.Unlock()

	s.bus.publish(Event{
		Kind: EventReservationEnded, At: s.now(), ChargePointID: out.ChargePointID,
		EVSEUID: out.EVSEUID, ReservationID: out.ID, IDTag: out.IDTag, Detail: reason,
	})
	return out, true
}

// ActiveReservation returns the active reservation on an EVSE, if any.
func (s *Service) ActiveReservation(evseUID string) (Reservation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r := s.reservationOnLocked(evseUID)
	if r == nil || r.Status != ReservationActive {
		return Reservation{}, false
	}
	return r.Reservation, true
}

// endLocked removes a reservation and returns the event to publish once the
// lock is released.
//
// The event carries the reservation's response URL only if it was active: the
// partner has heard that it holds one, so the end is news for it. A
// reservation that ends while still pending (a ReserveNow that failed, or a
// replacement that did) is reported through the command's own result.
func (s *Service) endLocked(r *reservation, reason string) Event {
	r.stopTimer()
	delete(s.reservations, r.ID)
	e := Event{
		Kind: EventReservationEnded, At: s.now(), ChargePointID: r.ChargePointID,
		EVSEUID: r.EVSEUID, ReservationID: r.ID, IDTag: r.IDTag, Detail: reason,
	}
	if r.Status == ReservationActive {
		e.ResponseURL = r.ResponseURL
	}
	return e
}

// armLocked schedules a reservation's expiry.
func (s *Service) armLocked(r *reservation) {
	r.stopTimer()
	id, expiresAt := r.ID, r.ExpiresAt
	r.timer = s.afterFunc(expiresAt.Sub(s.now()), func() { s.expire(id, expiresAt) })
}

// expire ends a reservation when its time is up, without any message from
// the station: the station expires its own copy independently.
//
// It runs on a timer goroutine, possibly a moment after the reservation was
// consumed or cancelled, or replaced by one with a later expiry, so it
// re-checks before acting.
func (s *Service) expire(id int, expiresAt time.Time) {
	s.mu.Lock()
	r, ok := s.reservations[id]
	if !ok || r.Status != ReservationActive || !r.ExpiresAt.Equal(expiresAt) {
		s.mu.Unlock()
		return
	}
	event := s.endLocked(r, ReservationExpired)
	s.mu.Unlock()

	s.persistOrLog("expired reservation", id)
	s.bus.publish(event)
}

func (s *Service) reservationOnLocked(evseUID string) *reservation {
	for _, r := range s.reservations {
		if r.EVSEUID == evseUID {
			return r
		}
	}
	return nil
}

// reservationsLocked lists a charge point's reservations by id.
func (s *Service) reservationsLocked(cpID string) []Reservation {
	out := []Reservation{}
	for _, r := range s.reservations {
		if r.ChargePointID == cpID {
			out = append(out, r.Reservation)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// connectorID maps a configured EVSE to its OCPP 1.6 connector id.
func (s *Service) connectorID(evseUID string) (int, bool) {
	if s.cfg == nil {
		return 0, false
	}
	e, ok := s.cfg.EVSEByUID(evseUID)
	return e.OCPPConnectorID, ok
}
