// Package simulator is a charge point that dials a CSMS and behaves like a
// station: it boots, heartbeats, reports connector status, and answers the
// commands a CSMS sends it.
//
// It exists so the rest of cpms can be developed and tested without the real
// Alpitronic on the desk. Tests drive it in-process; `cpms simulate charger`
// runs it from a second terminal.
package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// Scenario makes the simulator misbehave in a specific, repeatable way, so
// that the unhappy paths have something to test against.
type Scenario string

// The available scenarios.
const (
	// ScenarioNormal is a cooperative station.
	ScenarioNormal Scenario = "normal"
	// ScenarioRejectReserve refuses every ReserveNow outright, which is what a
	// station without the Reservation feature profile effectively does.
	ScenarioRejectReserve Scenario = "reject-reserve"
	// ScenarioOccupied answers ReserveNow with Occupied, as a station with a
	// car already plugged in would.
	ScenarioOccupied Scenario = "occupied"
	// ScenarioSlow delays every answer past a normal call timeout.
	ScenarioSlow Scenario = "slow"
	// ScenarioUnlockFails refuses to release the cable lock.
	ScenarioUnlockFails Scenario = "unlock-fails"
	// ScenarioSilent boots and then stops sending anything, while its
	// WebSocket stack keeps answering the CSMS's pings.
	//
	// This is a station whose OCPP layer has hung but whose TCP connection is
	// fine, and it is the case heartbeat_timeout exists to catch. Without it
	// the watchdog cannot be exercised by hand at all, because a cooperative
	// station keeps it fed.
	ScenarioSilent Scenario = "silent"
	// ScenarioOfflineTx scripts a station charging through an outage. It
	// implies Reconnect, and streams meter values every second unless
	// MeterInterval says otherwise.
	//
	// Once booted it starts a transaction on connector 1 and waits for the
	// connection to drop, which is the CSMS's or a proxy's doing. Offline,
	// it lets offlineSamples meter values queue up, stops the transaction,
	// and starts and stops a second one. Only then does it redial, so that
	// the queue is the same however fast the CSMS comes back; after the
	// boot it delivers the queue, and carries on as a normal station.
	ScenarioOfflineTx Scenario = "offline-tx"
)

// Scenarios lists every scenario, for the CLI's help text and validation.
func Scenarios() []Scenario {
	return []Scenario{
		ScenarioNormal, ScenarioRejectReserve, ScenarioOccupied,
		ScenarioSlow, ScenarioUnlockFails, ScenarioSilent, ScenarioOfflineTx,
	}
}

// Valid reports whether s is a known scenario.
func (s Scenario) Valid() bool {
	for _, known := range Scenarios() {
		if s == known {
			return true
		}
	}
	return false
}

const (
	// defaultSlowDelay is how long ScenarioSlow sits on a response. It must
	// comfortably exceed a normal CSMS call timeout.
	defaultSlowDelay = 45 * time.Second
	// commandSettle is the pause before a command's side effect is reported.
	// A real station answers RemoteStartTransaction first and only then sends
	// StartTransaction; doing it in one breath would reverse that order on the
	// wire and misrepresent how a charger behaves.
	commandSettle = 50 * time.Millisecond
	// offlineTxMeterInterval is ScenarioOfflineTx's meter interval when none
	// is given, and offlineSamples how many samples it queues while offline.
	offlineTxMeterInterval = time.Second
	offlineSamples         = 3
	// fallbackHeartbeat is used when the CSMS boot response gives no interval.
	fallbackHeartbeat = 60 * time.Second

	// The redial backoff with Reconnect: 1 s, 2 s, 4 s … up to 30 s, each
	// shortened by up to reconnectJitter at random, so that simulators that
	// lost the same CSMS do not redial it in lockstep.
	reconnectInitial = time.Second
	reconnectMax     = 30 * time.Second
	reconnectJitter  = 0.2
)

// Options configure a Simulator.
type Options struct {
	// URL is the CSMS WebSocket endpoint. A URL without a path gets
	// /ocpp/<ID> appended, which is what cpms serves.
	URL string
	// ID is the charge point identity.
	ID string
	// Version is the OCPP version to offer. Only 1.6 is implemented.
	Version ocpp.Version
	// Connectors is how many connectors to report, numbered from 1.
	Connectors int

	Vendor       string
	Model        string
	Firmware     string
	SerialNumber string

	// IDTag is the token the station quotes when it starts a session by
	// itself, standing in for someone presenting an RFID card.
	IDTag string

	Scenario Scenario
	// SlowDelay overrides how long ScenarioSlow stalls.
	SlowDelay time.Duration

	// UnreportedKeys are configuration keys left out of every
	// GetConfiguration answer, as a station that does not know them would.
	UnreportedKeys []string

	// FeatureProfiles, when set, is the SupportedFeatureProfiles value the
	// simulator reports instead of the one its scenario implies.
	FeatureProfiles string

	// MeterInterval is how often a connector with a running transaction
	// sends MeterValues. Zero sends none, which is what a station does with
	// MeterValueSampleInterval at 0. The CSMS can change it at run time with
	// ChangeConfiguration MeterValueSampleInterval.
	//
	// It is a duration rather than whole seconds so that tests can stream in
	// milliseconds; GetConfiguration reports it rounded up to a second. The
	// register counts whole Wh, so below about 75 ms of wall time a sample
	// can repeat the previous reading.
	//
	// The car is a 75 kWh battery that starts at 20% and charges at a
	// constant 50 kW, so the energy register rises strictly until the battery
	// is full, after about 72 minutes. From then on power and current are 0,
	// the SoC stays at 100 and the register stays put; the connector reports
	// SuspendedEV once, and the session runs until it is stopped, with
	// meterStop equal to the last streamed register.
	MeterInterval time.Duration

	Log *slog.Logger
	Now func() time.Time
	// AfterFunc schedules the meter samples. Together with Now it lets a test
	// move time forward instead of waiting for it; see internal/clocktest.
	AfterFunc func(time.Duration, func()) Stopper
	// HeartbeatAfterFunc schedules the heartbeats. It is separate from
	// AfterFunc so that a test counting meter timers does not see the
	// heartbeat's; it defaults to real time.
	HeartbeatAfterFunc func(time.Duration, func()) Stopper

	// Reconnect makes the simulator redial when its connection drops, instead
	// of stopping: it waits ReconnectBackoff, dials again, boots again and
	// reports the status of the station and every connector again, as a real
	// station does. A BootNotification the CSMS rejects after a reconnect
	// still stops it, and Run returns the rejection.
	//
	// The first Connect is not retried: a CSMS that cannot be reached at all
	// at start-up is more likely a wrong URL than an outage.
	//
	// Offline, the station keeps charging: StartTransaction and
	// StopTransaction work locally, and every transaction-related message is
	// queued, in memory, and delivered in order after the next Accepted boot,
	// ahead of everything else (see deliverTxLocked and classify).
	Reconnect bool
	// ReconnectBackoff, when set, is the wait before redial attempt n,
	// counting from 1 after every drop, in place of the default 1 s, 2 s,
	// 4 s … capped at 30 s, with jitter. It also spaces out the retries of a
	// transaction message the CSMS failed to process, n counting those.
	// Tests set it for short, predictable waits.
	ReconnectBackoff func(attempt int) time.Duration
	// ReconnectAfterFunc schedules the waits between redials and retries. Like
	// HeartbeatAfterFunc it is a clock of its own, so that a test can step
	// it alone; it defaults to real time.
	ReconnectAfterFunc func(time.Duration, func()) Stopper
}

// Stopper is a timer that can be cancelled, as time.AfterFunc returns.
type Stopper interface {
	Stop() bool
}

func realAfterFunc(d time.Duration, fn func()) Stopper { return time.AfterFunc(d, fn) }

func (o *Options) applyDefaults() {
	if o.Version == "" {
		o.Version = ocpp.Version16
	}
	if o.Connectors <= 0 {
		o.Connectors = 1
	}
	if o.Vendor == "" {
		o.Vendor = "Alpitronic"
	}
	if o.Model == "" {
		o.Model = "HYC300"
	}
	if o.Firmware == "" {
		o.Firmware = "simulator"
	}
	if o.Scenario == "" {
		o.Scenario = ScenarioNormal
	}
	if o.Scenario == ScenarioOfflineTx {
		o.Reconnect = true
		if o.MeterInterval == 0 {
			o.MeterInterval = offlineTxMeterInterval
		}
	}
	if o.SlowDelay <= 0 {
		o.SlowDelay = defaultSlowDelay
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AfterFunc == nil {
		o.AfterFunc = realAfterFunc
	}
	if o.HeartbeatAfterFunc == nil {
		o.HeartbeatAfterFunc = realAfterFunc
	}
	if o.ReconnectBackoff == nil {
		o.ReconnectBackoff = func(attempt int) time.Duration { return backoff(attempt, rand.Float64) }
	}
	if o.ReconnectAfterFunc == nil {
		o.ReconnectAfterFunc = realAfterFunc
	}
}

// backoff is the default wait before redial attempt n: reconnectInitial,
// doubling per attempt up to reconnectMax, then shortened by a random share
// of up to reconnectJitter. Shortening rather than spreading both ways keeps
// the cap a cap. jitter returns a number in [0, 1).
func backoff(attempt int, jitter func() float64) time.Duration {
	d := reconnectMax
	if attempt < 6 { // 1 s << 5 is already past the cap
		d = min(reconnectInitial<<max(attempt-1, 0), reconnectMax)
	}
	return time.Duration(float64(d) * (1 - reconnectJitter*jitter()))
}

func (o Options) validate() error {
	if o.URL == "" {
		return errors.New("simulator: URL is required")
	}
	if o.ID == "" {
		return errors.New("simulator: ID is required")
	}
	if o.Version != ocpp.Version16 {
		return fmt.Errorf("simulator: OCPP %s is not implemented", o.Version)
	}
	if !o.Scenario.Valid() {
		return fmt.Errorf("simulator: unknown scenario %q", o.Scenario)
	}
	if o.MeterInterval < 0 {
		return fmt.Errorf("simulator: meter interval %s is negative", o.MeterInterval)
	}
	return nil
}

// connector is one outlet's state.
type connector struct {
	status string
	// transactionID is the id the CSMS assigned, or 0 when idle. While the
	// session's StartTransaction waits for an answer it is a negative
	// placeholder (see nextLocalID), which TransactionID does not report.
	transactionID int
	// reservationID is the CSMS's reservation id, or 0 when not reserved.
	reservationID int
	meter         meter
	// stopMeter is closed to end the MeterValues stream of the running
	// transaction. It is nil while no stream runs.
	stopMeter chan struct{}
}

// Simulator is a charge point.
type Simulator struct {
	opts Options
	log  *slog.Logger

	mu sync.Mutex
	// link is the current connection, booted or not, and nil while there is
	// none. online is the same connection once it has booted; the station's
	// own messages go out on it (see call), so that nothing reaches the CSMS
	// ahead of a new connection's BootNotification.
	link   *ocppj.Conn
	online *ocppj.Conn
	// closed is set by Close, under mu, so that a redial finishing at that
	// moment cannot install a connection Close has missed.
	closed bool
	// fatal is why the simulator stopped by itself, which Run returns.
	fatal error

	// station is connector 0: the station as a whole.
	station    string
	connectors map[int]*connector
	heartbeat  time.Duration
	// meterInterval starts as Options.MeterInterval; ChangeConfiguration
	// moves it.
	meterInterval time.Duration
	// settings holds the writable configuration keys that change nothing
	// in the simulator's behaviour, so that a CSMS reads back what it set.
	settings map[string]string
	// heartbeatChanged and meterChanged are closed, and replaced, whenever
	// ChangeConfiguration moves the matching interval, so that a loop parked
	// on a timer armed with the old value re-arms with the new one at once.
	heartbeatChanged chan struct{}
	meterChanged     chan struct{}

	// queue holds the transaction-related messages not yet answered, oldest
	// first; see deliverTxLocked. It is only ever used with Reconnect, and
	// lives in memory only: a restarted simulator starts empty. It changes
	// only under callMu as well as mu. queueChanged is signalled on every
	// change.
	queue        []*txMessage
	queueChanged chan struct{}
	// nextLocalID is the placeholder id for the next session, counting down
	// from -1 so that it can never be mistaken for one the CSMS assigned.
	nextLocalID int

	// callMu keeps at most one CALL of ours outstanding, as OCPP-J requires
	// of each direction. Heartbeats, status reports, transactions and meter
	// samples come from different goroutines; holding this across the round
	// trip queues them instead of letting them overlap on the wire.
	callMu sync.Mutex

	// wg tracks the goroutines that report command side effects, so Close
	// does not race them.
	wg sync.WaitGroup

	// done is closed by Close; closing cancels closeCtx, which aborts a dial
	// in progress.
	done      chan struct{}
	closeCtx  context.Context
	cancelAll context.CancelFunc
}

// New builds a Simulator. It does not connect until Connect is called.
func New(opts Options) (*Simulator, error) {
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return nil, err
	}

	s := &Simulator{
		opts:       opts,
		log:        opts.Log.With("charge_point", opts.ID),
		station:    v16.StatusAvailable,
		connectors: make(map[int]*connector, opts.Connectors),
		heartbeat:  fallbackHeartbeat,
		done:       make(chan struct{}),

		meterInterval: opts.MeterInterval,
		settings:      defaultSettings(),

		heartbeatChanged: make(chan struct{}),
		meterChanged:     make(chan struct{}),
		queueChanged:     make(chan struct{}),
		nextLocalID:      -1,
	}
	for i := 1; i <= opts.Connectors; i++ {
		s.connectors[i] = &connector{status: v16.StatusAvailable}
	}
	s.closeCtx, s.cancelAll = context.WithCancel(context.Background())
	return s, nil
}

// endpoint is the URL to dial, with the conventional path filled in when the
// caller gave only a host.
func (s *Simulator) endpoint() (string, error) {
	u, err := url.Parse(s.opts.URL)
	if err != nil {
		return "", fmt.Errorf("simulator: invalid URL %q: %w", s.opts.URL, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("simulator: URL %q must use ws or wss", s.opts.URL)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ocpp/" + s.opts.ID
	}
	return u.String(), nil
}

// errRejected marks a BootNotification the CSMS answered with anything but
// Accepted. It stops the simulator even with Reconnect: a CSMS that does not
// want the station will not change its mind on a redial.
var errRejected = errors.New("boot rejected")

// errClosed is returned by a redial that lost the race with Close.
var errClosed = errors.New("simulator: closed")

// Connect dials the CSMS, sends BootNotification, and reports the initial
// status of the station and every connector.
//
// It returns once the station is booted and observable. Run blocks instead.
// With Reconnect, it also starts redialing whenever the connection drops.
func (s *Simulator) Connect(ctx context.Context) error {
	conn, err := s.establish(ctx)
	if err != nil {
		s.Close()
		return err
	}

	// The scenario holds the first redial until its offline part is done.
	scripted := make(chan struct{})
	if s.opts.Scenario == ScenarioOfflineTx {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer close(scripted)
			s.offlineTx(ctx, conn)
		}()
	} else {
		close(scripted)
	}

	if s.opts.Reconnect {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.supervise(ctx, conn, scripted)
		}()
	}
	return nil
}

// establish opens one connection and brings the station up on it: dial,
// BootNotification, the status of the station and every connector, then the
// heartbeat. The connection is closed again if anything before the heartbeat
// fails.
func (s *Simulator) establish(ctx context.Context) (*ocppj.Conn, error) {
	endpoint, err := s.endpoint()
	if err != nil {
		return nil, err
	}

	// A dial in progress when Close is called must not hold Close's caller
	// up for the handshake timeout.
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.closeCtx, cancel)()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{s.opts.Version.Subprotocol()},
	}
	ws, resp, err := dialer.DialContext(dialCtx, endpoint, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("simulator: dialling %s: %w (HTTP %d)", endpoint, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("simulator: dialling %s: %w", endpoint, err)
	}

	conn := ocppj.New(ws, ocppj.Options{
		ID:      s.opts.ID,
		Version: s.opts.Version,
		// A station keeps talking to a CSMS that has gone quiet, so there is
		// no idle deadline here; the CSMS is the side that decides a peer is
		// gone. A long timeout stands in for "never".
		IdleTimeout: 24 * time.Hour,
		CallTimeout: 30 * time.Second,
		Log:         s.log,
	})

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close()
		return nil, errClosed
	}
	s.link = conn
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		reason := conn.Run(ctx, s)
		s.log.Info("disconnected from csms", "reason", reason)
		s.dropped(conn)
	}()

	s.log.Info("connected to csms", "url", endpoint, "version", string(s.opts.Version))

	if err := s.bringUp(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.heartbeatLoop(ctx, conn)
	}()
	return conn, nil
}

// bringUp boots over a fresh connection, delivers the queue, which marks it
// online, and reports every status on it.
func (s *Simulator) bringUp(ctx context.Context, conn *ocppj.Conn) error {
	if err := s.boot(ctx, conn); err != nil {
		return err
	}

	// Whatever was queued while offline goes first, ahead of even the status
	// round; delivering the queue is what marks the connection online.
	s.callMu.Lock()
	s.mu.Lock()
	queued := len(s.queue)
	s.mu.Unlock()
	err := s.drainLocked(conn)
	s.callMu.Unlock()
	if err != nil {
		return fmt.Errorf("simulator: delivering queued transaction messages: %w", err)
	}
	if queued > 0 {
		s.log.Info("delivered transaction messages queued while offline", "messages", queued)
	}

	return s.reportInitialStatus(ctx, conn)
}

// dropped forgets a connection that has ended. Without Reconnect, that ends
// the simulator.
func (s *Simulator) dropped(conn *ocppj.Conn) {
	s.mu.Lock()
	if s.link == conn {
		s.link, s.online = nil, nil
	}
	s.mu.Unlock()

	if !s.opts.Reconnect {
		s.Close()
	}
}

// supervise redials each time the current connection drops, until the
// simulator closes, the context ends, or a redial is rejected. It does not
// redial before ready is closed.
func (s *Simulator) supervise(ctx context.Context, conn *ocppj.Conn, ready <-chan struct{}) {
	for {
		select {
		case <-conn.Done():
		case <-s.done:
			return
		case <-ctx.Done():
			return
		}
		select {
		case <-ready:
		case <-s.done:
			return
		case <-ctx.Done():
			return
		}
		if conn = s.redial(ctx); conn == nil {
			return
		}
	}
}

// redial dials until a connection is up and booted. It returns nil if the
// simulator closes, the context ends, or the CSMS rejects the boot, which
// stops the simulator.
func (s *Simulator) redial(ctx context.Context) *ocppj.Conn {
	for attempt := 1; ; attempt++ {
		wait := s.opts.ReconnectBackoff(attempt)
		s.log.Info("reconnecting", "attempt", attempt, "in", wait.String())
		if !s.sleep(ctx, s.opts.ReconnectAfterFunc, wait, nil) {
			return nil
		}

		conn, err := s.establish(ctx)
		switch {
		case err == nil:
			s.log.Info("reconnected", "attempt", attempt)
			return conn
		case errors.Is(err, errRejected):
			s.fail(err)
			return nil
		case errors.Is(err, errClosed), ctx.Err() != nil, s.isClosed():
			return nil
		}
		s.log.Warn("reconnect failed", "attempt", attempt, "error", err)
	}
}

// sleep waits d on the given clock. It reports false, early, if the
// simulator closes, the context ends, or abort is closed first; a nil abort
// never is.
func (s *Simulator) sleep(ctx context.Context, after func(time.Duration, func()) Stopper, d time.Duration, abort <-chan struct{}) bool {
	tick := make(chan struct{}, 1)
	timer := after(d, func() { tick <- struct{}{} })
	defer timer.Stop()

	select {
	case <-tick:
		return true
	case <-s.done:
		return false
	case <-ctx.Done():
		return false
	case <-abort:
		return false
	}
}

func (s *Simulator) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// fail stops the simulator because of err, which Run then returns.
func (s *Simulator) fail(err error) {
	s.log.Error("giving up", "error", err)
	s.mu.Lock()
	if s.fatal == nil {
		s.fatal = err
	}
	s.mu.Unlock()
	s.Close()
}

// Run connects and blocks until the context is cancelled or the simulator
// stops: the connection drops without Reconnect, or a reconnect is rejected,
// which Run then returns as its error.
func (s *Simulator) Run(ctx context.Context) error {
	if err := s.Connect(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-s.done:
	}
	s.Close()
	s.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// Close disconnects and stops redialing. It is safe to call more than once.
func (s *Simulator) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	conn := s.link
	s.mu.Unlock()

	s.cancelAll()
	if conn != nil {
		conn.Close()
	}
}

// Wait blocks until every goroutine the simulator started has finished.
func (s *Simulator) Wait() { s.wg.Wait() }

// Done is closed when the simulator has stopped: Close was called, the
// connection dropped without Reconnect, or a reconnect was rejected. With
// Reconnect, a dropped connection alone does not close it.
func (s *Simulator) Done() <-chan struct{} { return s.done }

// call sends one CALL on the booted connection and waits for its answer,
// queued behind any other CALL still outstanding. Without a booted
// connection it fails with ocpp.ErrNotConnected.
func (s *Simulator) call(ctx context.Context, action string, payload any) (json.RawMessage, error) {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	return s.callLocked(ctx, action, payload)
}

// callLocked is call for a caller that already holds callMu.
func (s *Simulator) callLocked(ctx context.Context, action string, payload any) (json.RawMessage, error) {
	conn := s.onlineConn()
	if conn == nil {
		return nil, ocpp.ErrNotConnected
	}
	return conn.Call(ctx, action, payload)
}

// callOn is call on a given connection, booted or not: the boot and status
// round of a new connection, and the heartbeat bound to it.
func (s *Simulator) callOn(ctx context.Context, conn *ocppj.Conn, action string, payload any) (json.RawMessage, error) {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	return conn.Call(ctx, action, payload)
}

func (s *Simulator) onlineConn() *ocppj.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.online
}

// boot sends BootNotification on conn.
func (s *Simulator) boot(ctx context.Context, conn *ocppj.Conn) error {
	raw, err := s.callOn(ctx, conn, v16.ActionBootNotification, v16.BootNotificationReq{
		ChargePointVendor:       s.opts.Vendor,
		ChargePointModel:        s.opts.Model,
		ChargePointSerialNumber: s.opts.SerialNumber,
		FirmwareVersion:         s.opts.Firmware,
	})
	if err != nil {
		return fmt.Errorf("simulator: BootNotification: %w", err)
	}

	var conf v16.BootNotificationConf
	if err := decodeInto(raw, &conf); err != nil {
		return fmt.Errorf("simulator: BootNotification response: %w", err)
	}
	if conf.Status != "Accepted" {
		return fmt.Errorf("simulator: csms answered BootNotification with %q: %w", conf.Status, errRejected)
	}

	if conf.Interval > 0 {
		s.mu.Lock()
		s.heartbeat = time.Duration(conf.Interval) * time.Second
		s.mu.Unlock()
	}
	s.log.Info("booted", "heartbeat_interval", s.heartbeatInterval().String())
	return nil
}

func (s *Simulator) heartbeatInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heartbeat
}

// heartbeatState returns the interval together with the channel that is
// closed when it next changes, read under one lock so no change is missed.
func (s *Simulator) heartbeatState() (time.Duration, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heartbeat, s.heartbeatChanged
}

// signalLocked wakes everyone waiting on *ch and arms a fresh channel. The
// caller holds s.mu.
func signalLocked(ch *chan struct{}) {
	close(*ch)
	*ch = make(chan struct{})
}

// reportInitialStatus tells the CSMS about connector 0 and every connector, as
// a station does after booting.
func (s *Simulator) reportInitialStatus(ctx context.Context, conn *ocppj.Conn) error {
	for i := 0; i <= s.opts.Connectors; i++ {
		status := s.stationStatus()
		if i > 0 {
			status = s.ConnectorStatus(i)
		}
		if _, err := s.callOn(ctx, conn, v16.ActionStatusNotification, s.statusReq(i, status)); err != nil {
			return fmt.Errorf("simulator: StatusNotification(%d): %w", i, err)
		}
	}
	return nil
}

// heartbeatLoop heartbeats on conn until it drops.
func (s *Simulator) heartbeatLoop(ctx context.Context, conn *ocppj.Conn) {
	if s.opts.Scenario == ScenarioSilent {
		// Deliberately never heartbeat. The connection stays open and pings
		// still get answered, so only the CSMS's application-level watchdog
		// can notice.
		s.log.Info("going silent: no further messages will be sent")
		return
	}

	for {
		interval, changed := s.heartbeatState()
		tick := make(chan struct{}, 1)
		timer := s.opts.HeartbeatAfterFunc(interval, func() { tick <- struct{}{} })

		select {
		case <-changed:
			// A new interval counts from now.
			timer.Stop()
			continue
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.done:
			timer.Stop()
			return
		case <-conn.Done():
			timer.Stop()
			return
		case <-tick:
		}

		if _, err := s.callOn(ctx, conn, v16.ActionHeartbeat, struct{}{}); err != nil {
			s.log.Debug("heartbeat failed", "error", err)
			return
		}
	}
}

// sendStatus reports one connector's status. Connector 0 is the station.
//
// With Reconnect, a status that changes while there is no booted connection
// is only kept: the status round after the next boot reports it. That is
// what a station does instead of queueing status messages.
func (s *Simulator) sendStatus(ctx context.Context, connectorID int, status string) error {
	_, err := s.call(ctx, v16.ActionStatusNotification, s.statusReq(connectorID, status))
	if s.opts.Reconnect && errors.Is(err, ocpp.ErrNotConnected) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("simulator: StatusNotification(%d): %w", connectorID, err)
	}
	return nil
}

func (s *Simulator) statusReq(connectorID int, status string) v16.StatusNotificationReq {
	return v16.StatusNotificationReq{
		ConnectorID: connectorID,
		ErrorCode:   "NoError",
		Status:      status,
		Timestamp:   s.opts.Now().UTC().Format(time.RFC3339),
	}
}

// SetConnectorStatus changes a connector's status and reports it, which is how
// a test or an operator stands in for someone plugging in a car.
func (s *Simulator) SetConnectorStatus(ctx context.Context, connectorID int, status string) error {
	if connectorID == 0 {
		s.mu.Lock()
		s.station = status
		s.mu.Unlock()
		return s.sendStatus(ctx, 0, status)
	}

	s.mu.Lock()
	c, ok := s.connectors[connectorID]
	if ok {
		c.status = status
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("simulator: no connector %d", connectorID)
	}
	return s.sendStatus(ctx, connectorID, status)
}

// ConnectorStatus reports a connector's current OCPP status.
func (s *Simulator) ConnectorStatus(connectorID int) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.connectors[connectorID]; ok {
		return c.status
	}
	return ""
}

func (s *Simulator) stationStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.station
}

// TransactionID reports the running transaction on a connector, as the CSMS
// numbered it, or 0 if there is none or the CSMS has not answered its
// StartTransaction yet (it is queued, or in flight). The placeholder id a
// session runs under until then is the simulator's own and is never
// reported.
func (s *Simulator) TransactionID(connectorID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.connectors[connectorID]; ok && c.transactionID > 0 {
		return c.transactionID
	}
	return 0
}

// ReservationID reports the reservation held on a connector, or 0.
func (s *Simulator) ReservationID(connectorID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.connectors[connectorID]; ok {
		return c.reservationID
	}
	return 0
}
