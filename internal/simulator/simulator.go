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
	"errors"
	"fmt"
	"log/slog"
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
)

// Scenarios lists every scenario, for the CLI's help text and validation.
func Scenarios() []Scenario {
	return []Scenario{
		ScenarioNormal, ScenarioRejectReserve, ScenarioOccupied,
		ScenarioSlow, ScenarioUnlockFails,
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
	// fallbackHeartbeat is used when the CSMS boot response gives no interval.
	fallbackHeartbeat = 60 * time.Second
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

	Log *slog.Logger
	Now func() time.Time
}

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
	if o.SlowDelay <= 0 {
		o.SlowDelay = defaultSlowDelay
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
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
	return nil
}

// connector is one outlet's state.
type connector struct {
	status string
	// transactionID is the id the CSMS assigned, or 0 when idle.
	transactionID int
	// reservationID is the CSMS's reservation id, or 0 when not reserved.
	reservationID int
	meter         int
}

// Simulator is a charge point.
type Simulator struct {
	opts Options
	log  *slog.Logger

	conn *ocppj.Conn

	mu sync.Mutex
	// station is connector 0: the station as a whole.
	station    string
	connectors map[int]*connector
	heartbeat  time.Duration

	// wg tracks the goroutines that report command side effects, so Close
	// does not race them.
	wg sync.WaitGroup

	closeOnce sync.Once
	done      chan struct{}
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
	}
	for i := 1; i <= opts.Connectors; i++ {
		s.connectors[i] = &connector{status: v16.StatusAvailable}
	}
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

// Connect dials the CSMS, sends BootNotification, and reports the initial
// status of the station and every connector.
//
// It returns once the station is booted and observable. Run blocks instead.
func (s *Simulator) Connect(ctx context.Context) error {
	endpoint, err := s.endpoint()
	if err != nil {
		return err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{s.opts.Version.Subprotocol()},
	}
	ws, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return fmt.Errorf("simulator: dialling %s: %w (HTTP %d)", endpoint, err, resp.StatusCode)
		}
		return fmt.Errorf("simulator: dialling %s: %w", endpoint, err)
	}

	s.conn = ocppj.New(ws, ocppj.Options{
		ID:      s.opts.ID,
		Version: s.opts.Version,
		// A station keeps talking to a CSMS that has gone quiet, so there is
		// no idle deadline here; the CSMS is the side that decides a peer is
		// gone. A long timeout stands in for "never".
		IdleTimeout: 24 * time.Hour,
		CallTimeout: 30 * time.Second,
		Log:         s.log,
	})

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		reason := s.conn.Run(ctx, s)
		s.log.Info("disconnected from csms", "reason", reason)
		s.Close()
	}()

	s.log.Info("connected to csms", "url", endpoint, "version", string(s.opts.Version))

	if err := s.boot(ctx); err != nil {
		s.Close()
		return err
	}
	if err := s.reportInitialStatus(ctx); err != nil {
		s.Close()
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.heartbeatLoop(ctx)
	}()

	return nil
}

// Run connects and blocks until the context is cancelled or the connection
// drops.
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
	return nil
}

// Close disconnects. It is safe to call more than once.
func (s *Simulator) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.conn != nil {
			s.conn.Close()
		}
	})
}

// Wait blocks until every goroutine the simulator started has finished.
func (s *Simulator) Wait() { s.wg.Wait() }

// Done is closed when the simulator has disconnected.
func (s *Simulator) Done() <-chan struct{} { return s.done }

func (s *Simulator) boot(ctx context.Context) error {
	raw, err := s.conn.Call(ctx, v16.ActionBootNotification, v16.BootNotificationReq{
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
		return fmt.Errorf("simulator: csms answered BootNotification with %q", conf.Status)
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

// reportInitialStatus tells the CSMS about connector 0 and every connector, as
// a station does after booting.
func (s *Simulator) reportInitialStatus(ctx context.Context) error {
	if err := s.sendStatus(ctx, 0, s.stationStatus()); err != nil {
		return err
	}
	for i := 1; i <= s.opts.Connectors; i++ {
		if err := s.sendStatus(ctx, i, s.ConnectorStatus(i)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Simulator) heartbeatLoop(ctx context.Context) {
	for {
		timer := time.NewTimer(s.heartbeatInterval())

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}

		if _, err := s.conn.Call(ctx, v16.ActionHeartbeat, struct{}{}); err != nil {
			s.log.Debug("heartbeat failed", "error", err)
			return
		}
	}
}

// sendStatus reports one connector's status. Connector 0 is the station.
func (s *Simulator) sendStatus(ctx context.Context, connectorID int, status string) error {
	_, err := s.conn.Call(ctx, v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: connectorID,
		ErrorCode:   "NoError",
		Status:      status,
		Timestamp:   s.opts.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("simulator: StatusNotification(%d): %w", connectorID, err)
	}
	return nil
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

// TransactionID reports the running transaction on a connector, or 0.
func (s *Simulator) TransactionID(connectorID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c, ok := s.connectors[connectorID]; ok {
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
