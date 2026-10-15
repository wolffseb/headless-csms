// Package core holds cpms's domain state: which charge points are connected,
// what each EVSE is doing, and which transactions are open.
//
// It is the single source of truth. The OCPP adapters write to it, and the
// TUI, the OCPI Locations module and the OCPI push client read from it or
// subscribe to its event bus. Nothing else keeps mutable charger state.
package core

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// BootInfo is what a charge point told us about itself when it booted.
type BootInfo struct {
	Vendor          string
	Model           string
	SerialNumber    string
	FirmwareVersion string
	At              time.Time
}

// String renders the boot information for a log line.
func (b BootInfo) String() string {
	s := b.Vendor
	if b.Model != "" {
		s += " " + b.Model
	}
	if b.FirmwareVersion != "" {
		s += " fw " + b.FirmwareVersion
	}
	return s
}

// Transaction is one charging session.
type Transaction struct {
	// ID is a string because OCPP 2.0.1 transaction ids are strings the
	// station chooses. 1.6's integers, which we choose, are held in decimal.
	ID          string
	EVSEUID     string
	ConnectorID int
	IDTag       string
	MeterStart  int
	StartedAt   time.Time
	// ReservationID is the reservation this session redeemed, or 0.
	ReservationID int

	// StoppedAt is zero while the transaction is running.
	StoppedAt time.Time
	MeterStop int
	Reason    string

	// seq orders transactions by when we learned of them. String ids do not
	// sort numerically ("10" < "9"), and a station's own ids need not sort
	// at all.
	seq uint64
}

// Active reports whether the transaction is still running.
func (t Transaction) Active() bool { return t.StoppedAt.IsZero() }

// chargePointState is the mutable state of one charge point. It is only ever
// touched while Service.mu is held.
type chargePointState struct {
	id       string
	online   bool
	boot     BootInfo
	lastSeen time.Time

	// station is the status reported for OCPP connector 0, meaning the
	// station as a whole.
	station EVSEStatus

	// order fixes the EVSE ordering: configured EVSEs first, in config order,
	// then any the charger reports that we did not expect.
	order        []string
	connectorIDs map[string]int
	// reported is the per-connector status as sent by the charger, before the
	// station-wide override is applied.
	reported  map[string]EVSEStatus
	errorCode map[string]string
	// effective is what the rest of cpms sees, and what status-change events
	// are diffed against.
	effective map[string]EVSEStatus

	transactions map[string]*Transaction
}

// Service is the domain state and its event bus.
type Service struct {
	cfg       *config.Config
	now       func() time.Time
	afterFunc func(time.Duration, func()) Stopper
	bus       *bus
	log       *slog.Logger

	// store is where active reservations, active transactions and the id
	// counters survive a restart. nil keeps everything in memory.
	store Store
	// persistMu serialises writes to the store, so they land in the order
	// the changes behind them were made. It is never taken while mu is held.
	persistMu sync.Mutex

	mu           sync.RWMutex
	cps          map[string]*chargePointState
	reservations map[int]*reservation
	txSeq        uint64

	// The next id to hand out. Ids start at 1: OCPP 1.6 uses transaction id 0
	// to mean "refused", and a zero reservation id reads as "none".
	nextReservationID int
	nextTransactionID int
	nextRemoteStartID int
}

// Store persists what core must not forget across a restart. *state.Store
// satisfies it.
type Store interface {
	Get() state.State
	Update(fn func(*state.State) error) error
}

// Stopper is a scheduled callback that can be cancelled. *time.Timer
// satisfies it.
type Stopper interface {
	Stop() bool
}

// Option customises a Service.
type Option func(*Service)

// WithClock replaces the clock, so tests can make timestamps deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithAfterFunc replaces how expiry timers are scheduled. Together with
// WithClock it lets a test move time forward instead of waiting for it.
func WithAfterFunc(afterFunc func(time.Duration, func()) Stopper) Option {
	return func(s *Service) { s.afterFunc = afterFunc }
}

// WithStore persists reservations, transactions and id counters, and loads
// them back when the Service is built.
func WithStore(store Store) Option {
	return func(s *Service) { s.store = store }
}

// WithLogger sets where core reports problems it cannot hand back to a
// caller, such as a failed write after the station has already acted.
func WithLogger(log *slog.Logger) Option {
	return func(s *Service) { s.log = log }
}

// New builds a Service for the configured station.
//
// The configured charge point and its EVSEs are registered immediately, all
// UNKNOWN, so that the OCPI Locations module can serve the station before the
// charger has ever dialled in.
//
// With a store, the reservations, transactions and counters it holds are
// loaded back. A reservation that expired while cpms was down is dropped;
// the rest have their expiry timers re-armed.
func New(cfg *config.Config, opts ...Option) *Service {
	s := &Service{
		cfg: cfg,
		now: time.Now,
		afterFunc: func(d time.Duration, fn func()) Stopper {
			return time.AfterFunc(d, fn)
		},
		bus:               newBus(),
		log:               slog.Default(),
		cps:               make(map[string]*chargePointState),
		reservations:      make(map[int]*reservation),
		nextReservationID: 1,
		nextTransactionID: 1,
		nextRemoteStartID: 1,
	}
	for _, opt := range opts {
		opt(s)
	}

	if cfg != nil {
		cp := s.newChargePoint(cfg.Charger.ID)
		for _, e := range cfg.Location.EVSEs {
			cp.addEVSE(e.UID, e.OCPPConnectorID)
		}
		s.cps[cp.id] = cp
	}
	if s.store != nil {
		s.load()
	}
	return s
}

// Close stops the expiry timers. Nothing else in the Service needs shutting
// down; it holds no connections and no goroutines of its own.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.reservations {
		r.stopTimer()
	}
}

// Subscribe registers an event consumer. The returned function unsubscribes.
func (s *Service) Subscribe(name string) (<-chan Event, func()) { return s.bus.Subscribe(name) }

// Drops reports events lost per subscriber, for diagnostics.
func (s *Service) Drops() map[string]uint64 { return s.bus.Drops() }

func (s *Service) newChargePoint(id string) *chargePointState {
	return &chargePointState{
		id:           id,
		station:      StatusAvailable,
		connectorIDs: make(map[string]int),
		reported:     make(map[string]EVSEStatus),
		errorCode:    make(map[string]string),
		effective:    make(map[string]EVSEStatus),
		transactions: make(map[string]*Transaction),
	}
}

func (cp *chargePointState) addEVSE(uid string, connectorID int) {
	if _, ok := cp.effective[uid]; ok {
		return
	}
	cp.order = append(cp.order, uid)
	cp.connectorIDs[uid] = connectorID
	cp.effective[uid] = StatusUnknown
}

// recompute re-derives every effective EVSE status and returns the changes.
// Events are produced only for EVSEs whose effective status actually moved:
// chargers re-send StatusNotification liberally, and without this the OCPI
// push client would PATCH the backend on every repeat.
func (cp *chargePointState) recompute(at time.Time) []Event {
	var events []Event
	for _, uid := range cp.order {
		want := effectiveStatus(cp.online, cp.station, cp.reported[uid])
		got := cp.effective[uid]
		if got == want {
			continue
		}
		cp.effective[uid] = want
		events = append(events, Event{
			Kind:          EventEVSEStatusChanged,
			At:            at,
			ChargePointID: cp.id,
			EVSEUID:       uid,
			From:          got,
			To:            want,
			ErrorCode:     cp.errorCode[uid],
		})
	}
	return events
}

// ensure returns the state for a charge point, creating it if the id is one we
// were not configured for. Unconfigured ids are still tracked so that a
// misconfigured station shows up in the log rather than vanishing.
func (s *Service) ensure(id string) *chargePointState {
	cp, ok := s.cps[id]
	if !ok {
		cp = s.newChargePoint(id)
		s.cps[id] = cp
	}
	return cp
}

// evseUID maps an OCPP connector id to the OCPI EVSE uid it was configured
// against. Connectors we have no mapping for get a synthetic uid so they are
// still observable rather than silently dropped.
func (s *Service) evseUID(cpID string, connectorID int) string {
	if s.cfg != nil && cpID == s.cfg.Charger.ID {
		if e, ok := s.cfg.EVSEByConnectorID(connectorID); ok {
			return e.UID
		}
	}
	return fmt.Sprintf("%s-%d", cpID, connectorID)
}

func (s *Service) publishAll(events []Event) {
	for _, e := range events {
		s.bus.publish(e)
	}
}

// Connected records that a charge point's WebSocket connection is up.
func (s *Service) Connected(cpID string) {
	at := s.now()

	s.mu.Lock()
	cp := s.ensure(cpID)
	cp.online = true
	cp.lastSeen = at
	events := append([]Event{{
		Kind: EventChargePointConnected, At: at, ChargePointID: cpID,
	}}, cp.recompute(at)...)
	s.mu.Unlock()

	s.publishAll(events)
}

// Disconnected records that a charge point is gone, either because the socket
// closed or because the heartbeat watchdog gave up. Every EVSE falls back to
// UNKNOWN: a station we cannot reach tells us nothing about its connectors.
func (s *Service) Disconnected(cpID, reason string) {
	at := s.now()

	s.mu.Lock()
	cp := s.ensure(cpID)
	if !cp.online {
		s.mu.Unlock()
		return
	}
	cp.online = false
	events := append([]Event{{
		Kind: EventChargePointDisconnected, At: at, ChargePointID: cpID, Detail: reason,
	}}, cp.recompute(at)...)
	s.mu.Unlock()

	s.publishAll(events)
}

// Booted records a BootNotification.
func (s *Service) Booted(cpID string, info BootInfo) {
	at := s.now()
	info.At = at

	s.mu.Lock()
	cp := s.ensure(cpID)
	cp.boot = info
	cp.lastSeen = at
	s.mu.Unlock()

	s.bus.publish(Event{
		Kind: EventBootNotified, At: at, ChargePointID: cpID, Detail: info.String(),
	})
}

// Seen records any sign of life from the charge point.
func (s *Service) Seen(cpID string) {
	at := s.now()

	s.mu.Lock()
	s.ensure(cpID).lastSeen = at
	s.mu.Unlock()
}

// Heartbeat records a Heartbeat message.
func (s *Service) Heartbeat(cpID string) {
	at := s.now()

	s.mu.Lock()
	s.ensure(cpID).lastSeen = at
	s.mu.Unlock()

	s.bus.publish(Event{Kind: EventHeartbeat, At: at, ChargePointID: cpID})
}

// SetConnectorStatus records a StatusNotification.
//
// OCPP 1.6 uses connector 0 to address the station as a whole, so that case
// updates the station status and lets recompute push the consequence out to
// every EVSE.
func (s *Service) SetConnectorStatus(cpID string, connectorID int, status EVSEStatus, errorCode string) {
	at := s.now()

	s.mu.Lock()
	cp := s.ensure(cpID)
	cp.lastSeen = at

	var events []Event
	if connectorID == 0 {
		if from := cp.station; from != status {
			cp.station = status
			events = append(events, Event{
				Kind: EventStationStatusChanged, At: at, ChargePointID: cpID,
				From: from, To: status, ErrorCode: errorCode,
			})
		}
	} else {
		uid := s.evseUID(cpID, connectorID)
		cp.addEVSE(uid, connectorID)
		cp.reported[uid] = status
		cp.errorCode[uid] = errorCode
	}
	events = append(events, cp.recompute(at)...)
	s.mu.Unlock()

	s.publishAll(events)
}

// RecordMeterValues notes a meter reading. The values themselves are not
// modelled yet; the summary is what the log and the TUI show.
func (s *Service) RecordMeterValues(cpID string, connectorID int, summary string) {
	at := s.now()

	s.mu.Lock()
	cp := s.ensure(cpID)
	uid := s.evseUID(cpID, connectorID)
	cp.addEVSE(uid, connectorID)
	cp.lastSeen = at
	s.mu.Unlock()

	s.bus.publish(Event{
		Kind: EventMeterValues, At: at, ChargePointID: cpID, EVSEUID: uid, Detail: summary,
	})
}

// EVSEStatus returns the effective status of one EVSE.
func (s *Service) EVSEStatus(uid string) (EVSEStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, cp := range s.cps {
		if st, ok := cp.effective[uid]; ok {
			return st, true
		}
	}
	return "", false
}

// IsOnline reports whether a charge point is currently connected.
func (s *Service) IsOnline(cpID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cp, ok := s.cps[cpID]
	return ok && cp.online
}

// EVSESnapshot is one EVSE as seen by a reader.
type EVSESnapshot struct {
	UID         string
	ConnectorID int
	Status      EVSEStatus
	ErrorCode   string
}

// ChargePointSnapshot is one charge point as seen by a reader.
type ChargePointSnapshot struct {
	ID            string
	Online        bool
	Boot          BootInfo
	LastSeen      time.Time
	StationStatus EVSEStatus
	EVSEs         []EVSESnapshot
	// Transactions holds the active ones and those stopped since this process
	// started, in the order they began. Only active ones survive a restart.
	Transactions []Transaction
	// Reservations holds pending and active reservations, by id.
	Reservations []Reservation
}

// Snapshot is a consistent, immutable view of the whole domain state.
type Snapshot struct {
	ChargePoints []ChargePointSnapshot
}

// ChargePoint returns one charge point from the snapshot.
func (s Snapshot) ChargePoint(id string) (ChargePointSnapshot, bool) {
	for _, cp := range s.ChargePoints {
		if cp.ID == id {
			return cp, true
		}
	}
	return ChargePointSnapshot{}, false
}

// Snapshot copies the current state. Readers get a value they can hold and
// render at leisure without keeping a lock or racing the protocol layer.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := Snapshot{ChargePoints: make([]ChargePointSnapshot, 0, len(s.cps))}
	for _, cp := range s.cps {
		snap := ChargePointSnapshot{
			ID:            cp.id,
			Online:        cp.online,
			Boot:          cp.boot,
			LastSeen:      cp.lastSeen,
			StationStatus: cp.station,
			EVSEs:         make([]EVSESnapshot, 0, len(cp.order)),
			Transactions:  make([]Transaction, 0, len(cp.transactions)),
		}
		for _, uid := range cp.order {
			snap.EVSEs = append(snap.EVSEs, EVSESnapshot{
				UID:         uid,
				ConnectorID: cp.connectorIDs[uid],
				Status:      cp.effective[uid],
				ErrorCode:   cp.errorCode[uid],
			})
		}
		for _, tx := range cp.transactions {
			snap.Transactions = append(snap.Transactions, *tx)
		}
		sort.Slice(snap.Transactions, func(i, j int) bool {
			return snap.Transactions[i].seq < snap.Transactions[j].seq
		})
		snap.Reservations = s.reservationsLocked(cp.id)
		out.ChargePoints = append(out.ChargePoints, snap)
	}
	sort.Slice(out.ChargePoints, func(i, j int) bool {
		return out.ChargePoints[i].ID < out.ChargePoints[j].ID
	})
	return out
}
