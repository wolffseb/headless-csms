package core

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// EventKind identifies what happened.
type EventKind string

// The events published on the bus.
const (
	EventChargePointConnected    EventKind = "charge_point_connected"
	EventChargePointDisconnected EventKind = "charge_point_disconnected"
	EventBootNotified            EventKind = "boot_notified"
	EventHeartbeat               EventKind = "heartbeat"
	EventStationStatusChanged    EventKind = "station_status_changed"
	EventEVSEStatusChanged       EventKind = "evse_status_changed"
	EventTransactionStarted      EventKind = "transaction_started"
	EventTransactionStopped      EventKind = "transaction_stopped"
	EventMeterValues             EventKind = "meter_values"
)

// Event is one thing that happened, in a shape a log line, a TUI row and the
// OCPI push client can all read.
//
// It is deliberately one flat struct rather than an interface hierarchy: every
// consumer wants to render a timeline, and a type switch per row buys nothing.
// Fields not relevant to a Kind are zero.
type Event struct {
	Kind          EventKind
	At            time.Time
	ChargePointID string

	// EVSEUID is set for per-EVSE events.
	EVSEUID string
	// From and To are set for status changes.
	From, To EVSEStatus
	// ErrorCode carries the OCPP errorCode from a StatusNotification.
	ErrorCode string

	// TransactionID is set for transaction events.
	TransactionID int
	// IDTag is the RFID tag involved, when there is one.
	IDTag string

	// Detail is a short human-readable extra, e.g. a disconnect reason.
	Detail string
}

// String renders the event as one log line.
func (e Event) String() string {
	switch e.Kind {
	case EventEVSEStatusChanged:
		s := fmt.Sprintf("%s %s→%s", e.EVSEUID, e.From, e.To)
		if e.ErrorCode != "" && e.ErrorCode != "NoError" {
			s += " (" + e.ErrorCode + ")"
		}
		return s
	case EventStationStatusChanged:
		return fmt.Sprintf("%s station %s→%s", e.ChargePointID, e.From, e.To)
	case EventChargePointConnected:
		return e.ChargePointID + " connected"
	case EventChargePointDisconnected:
		s := e.ChargePointID + " disconnected"
		if e.Detail != "" {
			s += ": " + e.Detail
		}
		return s
	case EventBootNotified:
		return fmt.Sprintf("%s booted (%s)", e.ChargePointID, e.Detail)
	case EventHeartbeat:
		return e.ChargePointID + " heartbeat"
	case EventTransactionStarted:
		return fmt.Sprintf("%s transaction %d started on %s (tag %s)",
			e.ChargePointID, e.TransactionID, e.EVSEUID, e.IDTag)
	case EventTransactionStopped:
		return fmt.Sprintf("%s transaction %d stopped", e.ChargePointID, e.TransactionID)
	case EventMeterValues:
		return fmt.Sprintf("%s meter values: %s", e.EVSEUID, e.Detail)
	default:
		return string(e.Kind)
	}
}

// subscriberBuffer is how many events a slow consumer may fall behind before
// it starts losing them.
const subscriberBuffer = 256

type subscriber struct {
	name  string
	ch    chan Event
	drops atomic.Uint64
}

// bus fans events out to subscribers.
//
// Publishing never blocks. A subscriber that stops reading — a TUI stuck
// mid-render, a push client waiting on a dead backend — loses events and has
// its drop counter incremented; it must not be able to stall the WebSocket
// reader that is publishing.
type bus struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

func newBus() *bus {
	return &bus{subs: make(map[*subscriber]struct{})}
}

// Subscribe registers a consumer and returns its channel plus a function that
// unsubscribes and closes the channel. The name appears in drop warnings.
func (b *bus) Subscribe(name string) (<-chan Event, func()) {
	s := &subscriber{name: name, ch: make(chan Event, subscriberBuffer)}

	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.ch)
		})
	}
	return s.ch, cancel
}

func (b *bus) publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			s.drops.Add(1)
		}
	}
}

// Drops reports how many events each subscriber has lost, for diagnostics.
func (b *bus) Drops() map[string]uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make(map[string]uint64, len(b.subs))
	for s := range b.subs {
		if n := s.drops.Load(); n > 0 {
			out[s.name] = n
		}
	}
	return out
}
