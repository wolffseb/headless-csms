package core

import (
	"sort"
	"strconv"

	"github.com/wolffseb/cli-cpms/internal/state"
)

// persist writes the parts of the domain state that must survive a restart:
// active reservations, active transactions and the id counters. Everything
// else in the store (the OCPI registration) is left as it is.
//
// It writes the whole of those parts each time rather than patching them.
// That keeps the file in step with memory no matter which change triggered
// the write: if two changes race, the later write carries both, because
// persistMu makes each write read the state only once it is its turn.
func (s *Service) persist() error {
	if s.store == nil {
		return nil
	}

	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.RLock()
	reservations, transactions := s.persistableLocked()
	nextReservation, nextTransaction, nextRemoteStart := s.nextReservationID, s.nextTransactionID, s.nextRemoteStartID
	s.mu.RUnlock()

	return s.store.Update(func(st *state.State) error {
		st.NextReservationID = nextReservation
		st.NextTransactionID = nextTransaction
		st.NextRemoteStartID = nextRemoteStart
		st.Reservations = reservations
		st.Transactions = transactions
		return nil
	})
}

// persistOrLog is persist for changes that have already happened at the
// station, where failing the caller would not undo anything.
func (s *Service) persistOrLog(what string, id int) {
	if err := s.persist(); err != nil {
		s.log.Error("could not persist "+what, "id", id, "error", err)
	}
}

// persistableLocked converts what survives a restart to the store's DTOs, in a
// stable order so that an unchanged state writes an unchanged file.
func (s *Service) persistableLocked() ([]state.Reservation, []state.Transaction) {
	reservations := []state.Reservation{}
	for _, r := range s.reservations {
		if r.Status != ReservationActive {
			continue
		}
		reservations = append(reservations, state.Reservation{
			ID: r.ID, OCPIReservationID: r.OCPIReservationID,
			ChargePointID: r.ChargePointID, EVSEUID: r.EVSEUID, IDTag: r.IDTag,
			ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt,
			Source: r.Source, ResponseURL: r.ResponseURL,
		})
	}
	sort.Slice(reservations, func(i, j int) bool { return reservations[i].ID < reservations[j].ID })

	var active []*Transaction
	cpOf := make(map[*Transaction]string)
	for _, cp := range s.cps {
		for _, tx := range cp.transactions {
			if tx.Active() {
				active = append(active, tx)
				cpOf[tx] = cp.id
			}
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].seq < active[j].seq })

	transactions := make([]state.Transaction, 0, len(active))
	for _, tx := range active {
		transactions = append(transactions, state.Transaction{
			ID: tx.ID, ChargePointID: cpOf[tx], EVSEUID: tx.EVSEUID, ConnectorID: tx.ConnectorID,
			IDTag: tx.IDTag, MeterStart: tx.MeterStart, StartedAt: tx.StartedAt,
			ReservationID: tx.ReservationID,
		})
	}
	return reservations, transactions
}

// load restores what an earlier run persisted.
//
// A reservation that expired while cpms was down is dropped, as is one for an
// EVSE that is no longer in config. If anything was dropped, the cleaned-up
// state is written back straight away, so the file never claims a
// reservation core does not hold.
func (s *Service) load() {
	st := s.store.Get()

	// A fresh file has zero counters; ids start at 1.
	s.nextReservationID = max(st.NextReservationID, 1)
	s.nextTransactionID = max(st.NextTransactionID, 1)
	s.nextRemoteStartID = max(st.NextRemoteStartID, 1)

	for _, t := range st.Transactions {
		cp := s.ensure(t.ChargePointID)
		cp.addEVSE(t.EVSEUID, t.ConnectorID)
		s.txSeq++
		cp.transactions[t.ID] = &Transaction{
			ID: t.ID, EVSEUID: t.EVSEUID, ConnectorID: t.ConnectorID,
			IDTag: t.IDTag, MeterStart: t.MeterStart, StartedAt: t.StartedAt,
			ReservationID: t.ReservationID,
			seq:           s.txSeq,
		}
		// Same guard for 1.6 ids, which are ours and numeric.
		if n, err := strconv.Atoi(t.ID); err == nil {
			s.nextTransactionID = max(s.nextTransactionID, n+1)
		}
	}

	now := s.now()
	dropped := false
	for _, r := range st.Reservations {
		connectorID, known := s.connectorID(r.EVSEUID)
		switch {
		case !r.ExpiresAt.After(now):
			s.log.Info("dropping a reservation that expired while cpms was not running",
				"reservation_id", r.ID, "evse", r.EVSEUID, "expired_at", r.ExpiresAt)
			dropped = true
			continue
		case !known:
			s.log.Warn("dropping a reservation for an EVSE that is no longer configured",
				"reservation_id", r.ID, "evse", r.EVSEUID)
			dropped = true
			continue
		}
		res := &reservation{Reservation: Reservation{
			ID: r.ID, OCPIReservationID: r.OCPIReservationID,
			ChargePointID: r.ChargePointID, EVSEUID: r.EVSEUID, ConnectorID: connectorID,
			IDTag: r.IDTag, ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt,
			Source: r.Source, ResponseURL: r.ResponseURL, Status: ReservationActive,
		}}
		s.reservations[r.ID] = res
		s.armLocked(res)
		// A file edited by hand, or from before a counter existed, must not
		// lead to an id being handed out twice.
		s.nextReservationID = max(s.nextReservationID, r.ID+1)
	}

	if dropped {
		if err := s.persist(); err != nil {
			s.log.Error("could not write back the state after dropping reservations", "error", err)
		}
	}
}
