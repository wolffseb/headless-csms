package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// txAttempts is how often a transaction-related message is sent to a CSMS
// that fails to process it (see retryable) before it is given up, which is
// what OCPP 1.6 section 3.7.1 lets a station do. It stands in for the
// TransactionMessageAttempts key, which the simulator does not offer.
const txAttempts = 3

// txMessage is a transaction-related message: StartTransaction, MeterValues
// or StopTransaction. These are the messages OCPP 1.6 has a station keep and
// deliver in order when the CSMS cannot be reached, with the timestamps they
// were made with.
type txMessage struct {
	action string
	start  *v16.StartTransactionReq
	meter  *v16.MeterValuesReq
	stop   *v16.StopTransactionReq

	// localID is the placeholder transaction id a StartTransaction opened
	// its session with. The CSMS's answer replaces it in every queued message
	// and on the connector.
	localID int
	// attempts counts the answers that said the CSMS failed to process it,
	// and the timeouts.
	attempts int
	// detached is set, under s.mu, on a StartTransaction whose caller has
	// returned with the session running and the message still queued. What
	// becomes of the message is then the queue's to act on (see
	// giveUpStartLocked and assignLocked); until then the caller sees it.
	detached bool

	// Once the message has left the queue: answered holds the CSMS's answer,
	// or err why it was given up.
	answered bool
	answer   json.RawMessage
	err      error
}

func (m *txMessage) payload() any {
	switch {
	case m.start != nil:
		return m.start
	case m.meter != nil:
		return m.meter
	default:
		return m.stop
	}
}

// transactionID is the transaction a MeterValues or StopTransaction refers
// to, as a pointer so that it can be rewritten, or nil for a
// StartTransaction.
func (m *txMessage) transactionID() *int {
	switch {
	case m.meter != nil:
		return m.meter.TransactionID
	case m.stop != nil:
		return &m.stop.TransactionID
	}
	return nil
}

// outcome is what one attempt at delivering a message came to.
type outcome int

const (
	// delivered: the CSMS answered with a CALLRESULT.
	delivered outcome = iota
	// refused: the message cannot be sent at all, as with a payload that
	// does not encode. It is given up.
	refused
	// retryable: the CSMS failed to process it (any CALLERROR), or did not
	// answer in time. It is sent again, up to txAttempts.
	retryable
	// lost: the connection went, or the simulator closed. It stays queued,
	// and the attempt does not count.
	lost
)

// classify sorts a CALL's error.
//
// Every CALLERROR counts as the CSMS failing to process the message. OCPP 1.6
// section 3.7.1 lets a station skip a transaction-related message only if
// the CSMS repeatedly reports such a failure, and the OCPP-J specification
// makes no exception for the codes that blame the message (FormationViolation
// and the like): a station cannot tell a CSMS bug from a bad message, and
// skipping early would lose a session's record. So all of them are sent
// again, up to txAttempts, which also bounds how long a message that will
// never be accepted holds up the queue behind it.
func classify(err error) outcome {
	var rpcErr *ocpp.RPCError
	switch {
	case err == nil:
		return delivered
	case errors.As(err, &rpcErr), errors.Is(err, ocpp.ErrTimeout):
		return retryable
	case errors.Is(err, ocpp.ErrNotConnected), errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return lost
	}
	// Anything else is a failure on the station's side (a payload that cannot
	// be encoded, say) and will not get better by trying again.
	return refused
}

// deliverTxLocked sends a transaction-related message, or queues it. The
// caller holds callMu.
//
// Without Reconnect it is a plain CALL, as before there was a queue. With
// Reconnect the message joins the queue, and if the station is online the
// queue is delivered at once; the message has then either been answered, or
// been given up, or it stays queued because the connection went. With
// nothing booted it stays queued for the next connection.
func (s *Simulator) deliverTxLocked(ctx context.Context, m *txMessage) {
	if !s.opts.Reconnect {
		m.answer, m.err = s.callLocked(ctx, m.action, m.payload())
		m.answered = m.err == nil
		if m.answered && m.start != nil {
			s.mu.Lock()
			s.assignLocked(m)
			s.mu.Unlock()
		}
		return
	}

	s.mu.Lock()
	s.queue = append(s.queue, m)
	signalLocked(&s.queueChanged)
	conn := s.online
	s.mu.Unlock()

	if conn != nil {
		_ = s.drainLocked(conn)
	}
}

// drainLocked delivers the queue on conn, oldest first, one CALL at a time,
// and marks conn online once the queue is empty. The caller holds callMu, so
// nothing else goes out until the queue is delivered or the connection is
// gone: that is what keeps a replay ahead of every message made after the
// reconnect.
//
// A message leaves the queue only once the CSMS has answered it, or has
// failed to process it txAttempts times, or cannot be sent at all (see
// classify). It runs on the simulator's own context rather than a caller's,
// so a caller giving up does not leave a message half delivered; Close stops
// it.
func (s *Simulator) drainLocked(conn *ocppj.Conn) error {
	ctx := s.closeCtx
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			if s.link == conn {
				s.online = conn
			}
			s.mu.Unlock()
			return nil
		}
		m := s.queue[0]
		// A placeholder id is the station's own and goes on no wire. Every
		// session's StartTransaction is ahead of its other messages and
		// replaces the placeholder when answered, or takes them with it when
		// given up, so this is a guard against a bug rather than a case.
		if id := m.transactionID(); id != nil && *id < 0 {
			s.popLocked()
			m.err = fmt.Errorf("simulator: %s carries the placeholder transaction id %d", m.action, *id)
			s.log.Error("dropping a message with a placeholder transaction id", "action", m.action, "id", *id)
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()

		answer, err := conn.Call(ctx, m.action, m.payload())
		switch classify(err) {
		case delivered:
			s.mu.Lock()
			s.popLocked()
			m.answered, m.answer = true, answer
			if m.start != nil {
				s.assignLocked(m)
			}
			s.mu.Unlock()
			continue

		case retryable:
			m.attempts++
			if m.attempts < txAttempts {
				s.log.Warn("csms failed to process a transaction message; retrying",
					"action", m.action, "attempt", m.attempts, "error", err)
				if !s.sleep(ctx, s.opts.ReconnectAfterFunc, s.opts.ReconnectBackoff(m.attempts), conn.Done()) {
					return ocpp.ErrNotConnected
				}
				continue
			}

		case lost:
			s.mu.Lock()
			if s.online == conn {
				s.online = nil
			}
			s.mu.Unlock()
			return err
		}

		// Out of attempts, or cannot be sent.
		s.log.Warn("giving up a transaction message", "action", m.action, "attempts", m.attempts+1, "error", err)
		s.mu.Lock()
		s.popLocked()
		m.err = err
		if m.start != nil {
			s.giveUpStartLocked(m)
		}
		s.mu.Unlock()
	}
}

// popLocked removes the head of the queue. The caller holds s.mu.
func (s *Simulator) popLocked() {
	s.queue[0] = nil
	s.queue = s.queue[1:]
	signalLocked(&s.queueChanged)
}

// assignLocked applies the transaction id the CSMS gave an answered
// StartTransaction: the session's queued MeterValues and StopTransaction,
// and the connector if the session is still running, take it in place of the
// placeholder, as a real station does. The caller holds s.mu.
func (s *Simulator) assignLocked(m *txMessage) {
	var conf v16.StartTransactionConf
	if err := decodeInto(m.answer, &conf); err != nil {
		s.log.Warn("unreadable StartTransaction answer", "error", err)
		return
	}
	for _, q := range s.queue {
		if id := q.transactionID(); id != nil && *id == m.localID {
			*id = conf.TransactionID
		}
	}
	for _, c := range s.connectors {
		if c.transactionID == m.localID {
			c.transactionID = conf.TransactionID
		}
	}
}

// giveUpStartLocked deals with a StartTransaction that was given up: the CSMS
// never opened the session, so it has nothing to put the session's queued
// messages against, and they are forgotten. If the session is still running
// and nobody waits on the start any more (it was started offline), it ends
// locally too: a station cannot go on charging under an id the CSMS does not
// have, and sending the placeholder would make up a transaction. The CSMS may
// in fact have opened it and only its answers been lost; the specification
// gives the station no way to tell. The caller holds s.mu.
func (s *Simulator) giveUpStartLocked(m *txMessage) {
	kept := s.queue[:0]
	for _, q := range s.queue {
		if id := q.transactionID(); id != nil && *id == m.localID {
			s.log.Warn("dropping a message of a session the csms never opened", "action", q.action)
			continue
		}
		kept = append(kept, q)
	}
	clear(s.queue[len(kept):])
	s.queue = kept

	if !m.detached {
		return
	}
	for id, c := range s.connectors {
		if c.transactionID == m.localID {
			s.log.Warn("ending a session started offline: its StartTransaction was given up",
				"connector", id, "placeholder_id", m.localID, "error", m.err)
			s.endSessionLocked(c)
		}
	}
}

// endSessionLocked ends the session running on c without a message to the
// CSMS: the meter stream stops and the connector is Available again. It does
// not report the status: this runs while a connection is being brought up,
// and the status round that follows says where each connector ended up. The
// caller holds s.mu.
func (s *Simulator) endSessionLocked(c *connector) {
	stopMeterLocked(c)
	c.transactionID = 0
	c.status = v16.StatusAvailable
}

// queuedLocked counts the queued messages of one action. The caller holds
// s.mu.
func (s *Simulator) queuedLocked(action string) int {
	n := 0
	for _, q := range s.queue {
		if q.action == action {
			n++
		}
	}
	return n
}

// offlineTx is ScenarioOfflineTx's script, up to the redial; see there. conn
// is the first connection, the one whose drop it waits for.
func (s *Simulator) offlineTx(ctx context.Context, conn *ocppj.Conn) {
	log := s.log.With("scenario", string(ScenarioOfflineTx))

	if err := s.StartTransaction(ctx, 1); err != nil {
		log.Warn("could not start the first transaction", "error", err)
		return
	}
	log.Info("charging; waiting for the connection to drop")
	select {
	case <-conn.Done():
	case <-s.done:
		return
	case <-ctx.Done():
		return
	}

	// The meter stream does the sampling. If the CSMS has switched it off,
	// there is nothing to wait for.
	for {
		s.mu.Lock()
		queued := s.queuedLocked(v16.ActionMeterValues)
		streaming := s.meterInterval > 0
		queueChanged, meterChanged := s.queueChanged, s.meterChanged
		s.mu.Unlock()
		if queued >= offlineSamples || !streaming {
			break
		}
		select {
		case <-queueChanged:
		case <-meterChanged:
		case <-s.done:
			return
		case <-ctx.Done():
			return
		}
	}

	for _, step := range []func(context.Context, int) error{s.StopTransaction, s.StartTransaction, s.StopTransaction} {
		if err := step(ctx, 1); err != nil {
			log.Warn("offline session step failed", "error", err)
			return
		}
	}

	s.mu.Lock()
	queued := len(s.queue)
	s.mu.Unlock()
	log.Info("offline part done; reconnecting", "queued", queued)
}
