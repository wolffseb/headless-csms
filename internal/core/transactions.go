package core

import (
	"strings"
	"time"
)

// TransactionStart is what a station reports when a session begins.
type TransactionStart struct {
	// ID is the transaction id: one we handed out (1.6) or the station's own
	// (2.0.1).
	ID          string
	ConnectorID int
	IDTag       string
	MeterStart  int
	// ReservationID is the reservation the station says the session redeems,
	// or 0 when it did not say.
	ReservationID int
	At            time.Time
}

// NextTransactionID hands out a transaction id for a protocol where the
// central system chooses them (OCPP 1.6). Ids are never reused, including
// across restarts.
//
// The station is waiting for the answer, so a failed write is logged rather
// than returned: refusing a StartTransaction makes the station retry it
// forever, which is worse than an id that might be handed out again after a
// crash.
func (s *Service) NextTransactionID() int {
	s.mu.Lock()
	id := s.nextTransactionID
	s.nextTransactionID++
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		s.log.Error("could not persist the transaction id counter", "transaction_id", id, "error", err)
	}
	return id
}

// NextRemoteStartID hands out an id that ties a remote start to the session
// it causes. OCPP 2.0.1 sends it; 1.6 has nowhere to put it, but the counter
// advances anyway so callers need not care which version they talk to.
//
// Unlike NextTransactionID this is on the command side, before anything is
// sent, so a failed write is returned and the command should not go out.
func (s *Service) NextRemoteStartID() (int, error) {
	s.mu.Lock()
	id := s.nextRemoteStartID
	s.nextRemoteStartID++
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		return 0, err
	}
	return id, nil
}

// StartTransaction records a session the station has started.
//
// If the session redeems a reservation, the reservation ends as consumed.
// The station saying which one (reservationId) is the reliable signal. Some
// stations leave it out, so a reservation on the same EVSE for the same tag
// counts too. A different tag leaves the reservation alone: someone else
// plugged in, and the reserved driver may still be on the way.
//
// Like every charger-driven change, this never fails because of the store: a
// failed write is logged.
func (s *Service) StartTransaction(cpID string, t TransactionStart) Transaction {
	if t.At.IsZero() {
		t.At = s.now()
	}

	s.mu.Lock()
	cp := s.ensure(cpID)
	uid := s.evseUID(cpID, t.ConnectorID)
	cp.addEVSE(uid, t.ConnectorID)

	if old, ok := cp.transactions[t.ID]; ok && old.Active() {
		s.log.Warn("station reused an active transaction id; replacing it",
			"charge_point", cpID, "transaction_id", t.ID)
	}

	s.txSeq++
	tx := &Transaction{
		ID: t.ID, EVSEUID: uid, ConnectorID: t.ConnectorID,
		IDTag: t.IDTag, MeterStart: t.MeterStart, StartedAt: t.At,
		ReservationID: t.ReservationID,
		seq:           s.txSeq,
	}

	var events []Event
	if r := s.redeemedLocked(cpID, uid, t); r != nil {
		tx.ReservationID = r.ID
		events = append(events, s.endLocked(r, ReservationConsumed))
	}
	cp.transactions[t.ID] = tx
	cp.lastSeen = s.now()
	out := *tx
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		s.log.Error("could not persist a started transaction", "charge_point", cpID,
			"transaction_id", t.ID, "error", err)
	}

	s.bus.publish(Event{
		Kind: EventTransactionStarted, At: t.At, ChargePointID: cpID,
		EVSEUID: uid, TransactionID: t.ID, IDTag: t.IDTag,
	})
	s.publishAll(events)
	return out
}

// redeemedLocked finds the active reservation a starting session redeems, if
// any.
func (s *Service) redeemedLocked(cpID, evseUID string, t TransactionStart) *reservation {
	if t.ReservationID != 0 {
		if r, ok := s.reservations[t.ReservationID]; ok && r.ChargePointID == cpID && r.Status == ReservationActive {
			return r
		}
	}
	for _, r := range s.reservations {
		if r.ChargePointID == cpID && r.EVSEUID == evseUID && r.Status == ReservationActive &&
			strings.EqualFold(r.IDTag, t.IDTag) {
			return r
		}
	}
	return nil
}

// StopTransaction closes a transaction. Unknown ids are reported so the caller
// can answer the charger appropriately.
func (s *Service) StopTransaction(cpID, txID string, meterStop int, reason string, at time.Time) (Transaction, bool) {
	if at.IsZero() {
		at = s.now()
	}

	s.mu.Lock()
	cp := s.ensure(cpID)
	tx, ok := cp.transactions[txID]
	if !ok || !tx.Active() {
		s.mu.Unlock()
		return Transaction{}, false
	}
	tx.StoppedAt = at
	tx.MeterStop = meterStop
	tx.Reason = reason
	cp.lastSeen = s.now()
	out := *tx
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		s.log.Error("could not persist a stopped transaction", "charge_point", cpID,
			"transaction_id", txID, "error", err)
	}

	s.bus.publish(Event{
		Kind: EventTransactionStopped, At: at, ChargePointID: cpID,
		EVSEUID: out.EVSEUID, TransactionID: txID, IDTag: out.IDTag, Detail: reason,
	})
	return out, true
}

// ActiveTransactions returns the running transactions on an EVSE. There
// should be at most one; a caller seeing more has found a bug and should say
// so rather than pick one.
func (s *Service) ActiveTransactions(evseUID string) []Transaction {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Transaction
	for _, cp := range s.cps {
		for _, tx := range cp.transactions {
			if tx.EVSEUID == evseUID && tx.Active() {
				out = append(out, *tx)
			}
		}
	}
	return out
}
