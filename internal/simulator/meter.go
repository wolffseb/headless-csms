package simulator

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// The car on the other end of a simulated session: a DC charge at constant
// power into a battery that starts at initialSoC and stops drawing once full.
// Constant power keeps the numbers easy to check by hand: at 50 kW, ten
// seconds add 138 Wh.
const (
	chargePower     = 50_000 // W
	chargeVoltage   = 400    // V
	batteryCapacity = 75_000 // Wh
	initialSoC      = 20     // %

	// toFullMWh is how much energy takes the battery from initialSoC to 100%.
	toFullMWh = int64(batteryCapacity) * 1000 * (100 - initialSoC) / 100
	// legacyStopMWh is what a session adds when it streamed nothing: the flat
	// kWh the simulator has always reported, kept so that runs without
	// --meter-interval look the same as before.
	legacyStopMWh = 1_000_000
)

// meter is one connector's energy register and the session drawing on it.
type meter struct {
	// mWh is the register in milliwatt-hours, so that samples shorter than a
	// watt-hour still add up. The wire carries whole Wh.
	mWh int64
	// sessionStart is mWh when the running transaction began; the SoC is
	// derived from what has flowed since.
	sessionStart int64
	// sampledAt is when mWh was last advanced.
	sampledAt time.Time
	// streamed is set once the running transaction has sent MeterValues.
	streamed bool
}

// register is the energy reading in Wh, as MeterValues, meterStart and
// meterStop report it.
func (m *meter) register() int { return int(m.mWh / 1000) }

// begin starts a session at now.
func (m *meter) begin(now time.Time) {
	m.sessionStart = m.mWh
	m.sampledAt = now
	m.streamed = false
}

func (m *meter) delivered() int64 { return m.mWh - m.sessionStart }

// power is what the car draws right now: everything until the battery is
// full, then nothing, so a long session flattens out instead of charging past
// 100%.
func (m *meter) power() int {
	if m.delivered() >= toFullMWh {
		return 0
	}
	return chargePower
}

// advance adds the energy that flowed between the last sample and now.
func (m *meter) advance(now time.Time) {
	dt := now.Sub(m.sampledAt)
	if dt <= 0 {
		return
	}
	m.sampledAt = now

	// W × ms / 3600 is mWh.
	m.mWh += int64(m.power()) * dt.Milliseconds() / 3600
	if over := m.delivered() - toFullMWh; over > 0 {
		m.mWh -= over
	}
}

// soc is the car's state of charge in percent.
func (m *meter) soc() int {
	soc := initialSoC + int(m.delivered()*100/(int64(batteryCapacity)*1000))
	return min(soc, 100)
}

// sampledValues is one reading of everything the simulator meters.
func (m *meter) sampledValues() []v16.SampledValue {
	power := m.power()
	sample := func(value int, measurand, unit, location string) v16.SampledValue {
		return v16.SampledValue{
			Value:     strconv.Itoa(value),
			Context:   "Sample.Periodic",
			Measurand: measurand,
			Unit:      unit,
			Location:  location,
		}
	}
	return []v16.SampledValue{
		sample(m.register(), "Energy.Active.Import.Register", "Wh", "Outlet"),
		sample(power, "Power.Active.Import", "W", "Outlet"),
		sample(power/chargeVoltage, "Current.Import", "A", "Outlet"),
		sample(m.soc(), "SoC", "Percent", "EV"),
	}
}

// startMeterLocked begins streaming MeterValues for the transaction running
// on c, unless a stream already runs, streaming is off, or the station is
// silent. The caller holds s.mu.
func (s *Simulator) startMeterLocked(connectorID int, c *connector) {
	if c.transactionID == 0 || c.stopMeter != nil || s.meterInterval <= 0 {
		return
	}
	// Silent means silent: the stream would keep the watchdog fed.
	if s.opts.Scenario == ScenarioSilent {
		return
	}

	stop := make(chan struct{})
	c.stopMeter = stop
	txID := c.transactionID

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.meterLoop(connectorID, txID, stop)
	}()
}

// stopMeterLocked ends the stream on c, if one runs. The caller holds s.mu.
func stopMeterLocked(c *connector) {
	if c.stopMeter != nil {
		close(c.stopMeter)
		c.stopMeter = nil
	}
}

// meterLoop sends a sample every interval until the transaction stops, the
// interval is set to 0, or the connection goes.
func (s *Simulator) meterLoop(connectorID, txID int, stop <-chan struct{}) {
	for {
		s.mu.Lock()
		interval, on := s.meterOnLocked(connectorID, stop)
		changed := s.meterChanged
		s.mu.Unlock()
		if !on {
			return
		}

		tick := make(chan struct{}, 1)
		timer := s.opts.AfterFunc(interval, func() { tick <- struct{}{} })
		select {
		case <-tick:
		case <-changed:
			// A new interval counts from now; 0 ends the stream at the top
			// of the loop.
			timer.Stop()
			continue
		case <-stop:
			timer.Stop()
			return
		case <-s.done:
			timer.Stop()
			return
		}

		if err := s.sendMeterValues(connectorID, txID, stop); err != nil {
			if errors.Is(err, errMeterOff) || errors.Is(err, ocpp.ErrNotConnected) {
				return
			}
			// A refused or unanswered sample is the CSMS's problem; a real
			// station keeps metering regardless.
			s.log.Debug("meter values failed", "connector", connectorID, "error", err)
		}
	}
}

// errMeterOff ends a stream whose interval was set to 0.
var errMeterOff = errors.New("meter values switched off")

// meterOnLocked reports the current interval, and whether the stream should
// go on at all. The caller holds s.mu.
//
// When ChangeConfiguration has set the interval to 0, it marks the stream
// ended. Doing so under the same lock that set the interval lets a later
// change start a fresh stream rather than find this one apparently running.
func (s *Simulator) meterOnLocked(connectorID int, stop <-chan struct{}) (time.Duration, bool) {
	if s.meterInterval > 0 {
		return s.meterInterval, true
	}
	if c := s.connectors[connectorID]; c.stopMeter == stop {
		c.stopMeter = nil
	}
	return 0, false
}

// sendMeterValues advances the meter and reports it.
//
// The sample is taken while holding the CALL slot, so that it cannot be
// overtaken by the StopTransaction of the same session: either it goes out
// before the stop, or the stop has already happened and it is dropped.
func (s *Simulator) sendMeterValues(connectorID, txID int, stop <-chan struct{}) error {
	s.callMu.Lock()
	defer s.callMu.Unlock()

	s.mu.Lock()
	select {
	case <-stop:
		s.mu.Unlock()
		return nil
	default:
	}
	// The interval may have been set to 0 while this tick was pending.
	if _, on := s.meterOnLocked(connectorID, stop); !on {
		s.mu.Unlock()
		return errMeterOff
	}
	c := s.connectors[connectorID]
	now := s.opts.Now()
	c.meter.advance(now)
	c.meter.streamed = true
	values := c.meter.sampledValues()
	s.mu.Unlock()

	_, err := s.conn.Call(context.Background(), v16.ActionMeterValues, v16.MeterValuesReq{
		ConnectorID:   connectorID,
		TransactionID: &txID,
		MeterValue: []v16.MeterValue{{
			Timestamp:    now.UTC().Format(time.RFC3339),
			SampledValue: values,
		}},
	})
	return err
}
