package core

import (
	"errors"
	"fmt"
	"sort"
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
)

// Why a reservation ended, as carried in EventReservationEnded.Detail.
const (
	ReservationExpired   = "expired"
	ReservationCancelled = "cancelled"
	ReservationConsumed  = "consumed"
	ReservationRejected  = "rejected"
	ReservationTimeout   = "timeout"
	// ReservationFailed means the command failed for a reason that is neither
	// a refusal nor a timeout: the connection dropped, or the station sent a
	// CALLERROR.
	ReservationFailed = "failed"
)

// ErrAlreadyReserved means the EVSE already holds a reservation, pending or
// active. It is refused locally, before anything is sent.
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

// EndReservation removes a reservation, pending or active, and reports why
// with EventReservationEnded. It returns false if there was no such
// reservation, for instance because it expired a moment earlier.
func (s *Service) EndReservation(id int, reason string) (Reservation, bool) {
	s.mu.Lock()
	r, ok := s.reservations[id]
	if !ok {
		s.mu.Unlock()
		return Reservation{}, false
	}
	event := s.endLocked(r, reason)
	s.mu.Unlock()

	s.persistOrLog("ended reservation", id)
	s.bus.publish(event)
	return r.Reservation, true
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
func (s *Service) endLocked(r *reservation, reason string) Event {
	r.stopTimer()
	delete(s.reservations, r.ID)
	return Event{
		Kind: EventReservationEnded, At: s.now(), ChargePointID: r.ChargePointID,
		EVSEUID: r.EVSEUID, ReservationID: r.ID, IDTag: r.IDTag, Detail: reason,
	}
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
