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
	// bootedSinceConnect is whether a BootNotification has arrived over the
	// current connection. A station that reconnects without one kept running
	// and need not report its connectors again (#53).
	bootedSinceConnect bool
	// lastDisconnect is zero until the station has disconnected while this
	// process was running.
	lastDisconnect Disconnect

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
	// changedAt is when each effective status last changed: the moment of its
	// EventEVSEStatusChanged, or when the EVSE was first tracked.
	changedAt map[string]time.Time

	transactions map[string]*Transaction
	// readings is the newest meter reading per EVSE. It is live data only:
	// never persisted, and dropped when the session on the EVSE stops or the
	// station disconnects, so status never shows a stale draw.
	readings map[string]MeterReading
	// stationReading is the newest reading for connector 0, the station as
	// a whole, such as clock-aligned Power.Offered (#64). Like readings, it
	// is dropped when the station disconnects. Nil until there is one.
	stationReading *MeterReading
}

// MeterReading is the newest meter reading for an EVSE, in units that do
// not depend on how the station chose to send them. A nil field was not in
// the reading, or could not be read.
//
// Core keeps one per EVSE and merges each MeterValues into it field by
// field: stations interleave messages with different measurand sets, so a
// value the newest message lacks keeps the one before, and its own time in
// the matching ...At field says how old it is.
type MeterReading struct {
	// At is when the station took the newest reading merged in.
	At     time.Time
	PowerW *float64
	// CurrentA is the highest phase current when the station reports only
	// phases.
	CurrentA *float64
	// PowerOfferedW and CurrentOfferedA are the most the station offers the
	// car: the limit it applies, which is what a charging profile changes.
	PowerOfferedW   *float64
	CurrentOfferedA *float64
	EnergyWh        *int
	// Summary is the newest reading as one line, for the log and the TUI.
	Summary string

	// PowerAt and the rest are when each value was taken, zero for a value
	// never reported. Core sets them; a reading passed to RecordMeterValues
	// leaves them out.
	PowerAt          time.Time
	CurrentAt        time.Time
	PowerOfferedAt   time.Time
	CurrentOfferedAt time.Time
	EnergyAt         time.Time
}

// merge lays next over r: next's values replace their own fields, stamped
// with next.At, and r's other values stay as they were.
func (r MeterReading) merge(next MeterReading) MeterReading {
	take := func(dst **float64, at *time.Time, v *float64) {
		if v != nil {
			*dst, *at = clonePtr(v), next.At
		}
	}
	out := *r.clone()
	take(&out.PowerW, &out.PowerAt, next.PowerW)
	take(&out.CurrentA, &out.CurrentAt, next.CurrentA)
	take(&out.PowerOfferedW, &out.PowerOfferedAt, next.PowerOfferedW)
	take(&out.CurrentOfferedA, &out.CurrentOfferedAt, next.CurrentOfferedA)
	if next.EnergyWh != nil {
		out.EnergyWh, out.EnergyAt = clonePtr(next.EnergyWh), next.At
	}
	out.At, out.Summary = next.At, next.Summary
	return out
}

// clone copies r so that a snapshot shares no pointers with core's state.
func (r MeterReading) clone() *MeterReading {
	c := r
	c.PowerW = clonePtr(r.PowerW)
	c.CurrentA = clonePtr(r.CurrentA)
	c.PowerOfferedW = clonePtr(r.PowerOfferedW)
	c.CurrentOfferedA = clonePtr(r.CurrentOfferedA)
	c.EnergyWh = clonePtr(r.EnergyWh)
	return &c
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
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
	// profiles are the charging profiles the station accepted, as sent.
	profiles []ChargingProfile
	txSeq    uint64
	// tagGrants are the tags an OCPI START_SESSION vouched for, lowercased,
	// and until when (see tags.go). In memory only: the window is minutes.
	tagGrants map[string]time.Time

	// The next id to hand out. Ids start at 1: OCPP 1.6 uses transaction id 0
	// to mean "refused", and a zero reservation id reads as "none".
	nextReservationID     int
	nextTransactionID     int
	nextRemoteStartID     int
	nextChargingProfileID int
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
		tagGrants:         make(map[string]time.Time),
		nextReservationID: 1,
		nextTransactionID: 1,
		nextRemoteStartID: 1,

		nextChargingProfileID: 1,
	}
	for _, opt := range opts {
		opt(s)
	}

	if cfg != nil {
		at := s.now()
		for _, ch := range cfg.Chargers {
			cp := s.newChargePoint(ch.ID)
			for _, e := range cfg.EVSEsOf(ch.ID) {
				cp.addEVSE(e.UID, e.OCPPConnectorID, at)
			}
			s.cps[cp.id] = cp
		}
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
		changedAt:    make(map[string]time.Time),
		transactions: make(map[string]*Transaction),
		readings:     make(map[string]MeterReading),
	}
}

func (cp *chargePointState) addEVSE(uid string, connectorID int, at time.Time) {
	if _, ok := cp.effective[uid]; ok {
		return
	}
	cp.order = append(cp.order, uid)
	cp.connectorIDs[uid] = connectorID
	cp.effective[uid] = StatusUnknown
	cp.changedAt[uid] = at
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
		cp.changedAt[uid] = at
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
	if s.cfg != nil {
		if e, ok := s.cfg.EVSEByConnector(cpID, connectorID); ok {
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
	cp.bootedSinceConnect = false
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
	cp.lastDisconnect = Disconnect{At: at, Reason: reason}
	clear(cp.readings)
	cp.stationReading = nil
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
	cp.bootedSinceConnect = true
	s.mu.Unlock()

	s.bus.publish(Event{
		Kind: EventBootNotified, At: at, ChargePointID: cpID, Detail: info.String(),
	})
}

// BootedSinceConnect reports whether the charge point has sent a
// BootNotification since its current connection came up. A station that
// boots reports every connector's status by itself; one that does not may
// have to be asked.
func (s *Service) BootedSinceConnect(cpID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cp, ok := s.cps[cpID]
	return ok && cp.online && cp.bootedSinceConnect
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
		cp.addEVSE(uid, connectorID, at)
		cp.reported[uid] = status
		cp.errorCode[uid] = errorCode
	}
	events = append(events, cp.recompute(at)...)
	s.mu.Unlock()

	s.publishAll(events)
}

// RecordMeterValues merges r into the EVSE's reading, field by field (see
// MeterReading). Only the newest value of each is kept, in memory: nothing in
// cpms bills, and meter history is out of scope. r.Summary is what the log
// and the TUI show.
//
// txID is the transaction the station says the values belong to, or "" for
// none. A reading for a transaction that is not running on the EVSE is
// published but not kept: it is a late or replayed message of a session that
// has stopped, and keeping it would show that session's draw again with
// nothing left to clear it. A reading without a transaction is kept: that is
// clock-aligned data, such as Power.Offered on an idle or Reserved connector.
//
// Connector 0 is the station as a whole, as in SetConnectorStatus: its
// reading is the station's, never an EVSE's, and its event has no EVSEUID.
func (s *Service) RecordMeterValues(cpID string, connectorID int, txID string, r MeterReading) {
	at := s.now()
	if r.At.IsZero() {
		r.At = at
	}

	s.mu.Lock()
	cp := s.ensure(cpID)
	cp.lastSeen = at
	if connectorID == 0 {
		var prev MeterReading
		if cp.stationReading != nil {
			prev = *cp.stationReading
		}
		next := prev.merge(r)
		cp.stationReading = &next
		s.mu.Unlock()

		s.bus.publish(Event{Kind: EventMeterValues, At: at, ChargePointID: cpID, Detail: r.Summary})
		return
	}
	uid := s.evseUID(cpID, connectorID)
	cp.addEVSE(uid, connectorID, at)
	stale := false
	if txID != "" {
		tx, ok := cp.transactions[txID]
		stale = !ok || !tx.Active() || tx.EVSEUID != uid
	}
	if !stale {
		cp.readings[uid] = cp.readings[uid].merge(r)
	}
	s.mu.Unlock()

	if stale {
		s.log.Info("not keeping a meter reading for a transaction that is not running on the EVSE",
			"charge_point", cpID, "evse", uid, "transaction_id", txID)
	}

	s.bus.publish(Event{
		Kind: EventMeterValues, At: at, ChargePointID: cpID, EVSEUID: uid, Detail: r.Summary,
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

// Disconnect is when and why a charge point went offline.
type Disconnect struct {
	At     time.Time
	Reason string
}

// EVSESnapshot is one EVSE as seen by a reader.
type EVSESnapshot struct {
	UID         string
	ConnectorID int
	Status      EVSEStatus
	ErrorCode   string
	// LastUpdated is when Status last changed, the moment its
	// EventEVSEStatusChanged was emitted; before any change, when the Service
	// started tracking the EVSE. A repeated StatusNotification that changes
	// nothing leaves it alone, so OCPI's last_updated means what it says.
	LastUpdated time.Time
	// Reading is the newest meter reading, or nil when there is none: no
	// MeterValues yet, or the session or the connection ended since.
	Reading *MeterReading
}

// ChargePointSnapshot is one charge point as seen by a reader.
type ChargePointSnapshot struct {
	ID       string
	Online   bool
	Boot     BootInfo
	LastSeen time.Time
	// LastDisconnect is the most recent disconnect, or zero if there has been
	// none since this process started.
	LastDisconnect Disconnect
	StationStatus  EVSEStatus
	// StationReading is the newest meter reading for the station as a whole
	// (OCPP 1.6 connector 0), or nil when there is none.
	StationReading *MeterReading
	EVSEs          []EVSESnapshot
	// Transactions holds the active ones and those stopped since this process
	// started, in the order they began. Only active ones survive a restart.
	Transactions []Transaction
	// Reservations holds pending, active and cancelling reservations, by id.
	Reservations []Reservation
	// ChargingProfiles are the profiles the station accepted, as sent, by
	// id: the station's own and every EVSE's.
	ChargingProfiles []ChargingProfile
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
			ID:             cp.id,
			Online:         cp.online,
			Boot:           cp.boot,
			LastSeen:       cp.lastSeen,
			LastDisconnect: cp.lastDisconnect,
			StationStatus:  cp.station,
			EVSEs:          make([]EVSESnapshot, 0, len(cp.order)),
			Transactions:   make([]Transaction, 0, len(cp.transactions)),
		}
		if r := cp.stationReading; r != nil {
			snap.StationReading = r.clone()
		}
		for _, uid := range cp.order {
			e := EVSESnapshot{
				UID:         uid,
				ConnectorID: cp.connectorIDs[uid],
				Status:      cp.effective[uid],
				ErrorCode:   cp.errorCode[uid],
				LastUpdated: cp.changedAt[uid],
			}
			if r, ok := cp.readings[uid]; ok {
				e.Reading = r.clone()
			}
			snap.EVSEs = append(snap.EVSEs, e)
		}
		for _, tx := range cp.transactions {
			snap.Transactions = append(snap.Transactions, *tx)
		}
		sort.Slice(snap.Transactions, func(i, j int) bool {
			return snap.Transactions[i].seq < snap.Transactions[j].seq
		})
		snap.Reservations = s.reservationsLocked(cp.id)
		snap.ChargingProfiles = s.profilesLocked(cp.id)
		out.ChargePoints = append(out.ChargePoints, snap)
	}
	sort.Slice(out.ChargePoints, func(i, j int) bool {
		return out.ChargePoints[i].ID < out.ChargePoints[j].ID
	})
	return out
}
