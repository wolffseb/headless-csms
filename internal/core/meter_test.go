package core_test

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
)

func reading(t *testing.T, s *core.Service, uid string) *core.MeterReading {
	t.Helper()

	cp, ok := s.Snapshot().ChargePoint(testCP)
	if !ok {
		t.Fatalf("no %s in the snapshot", testCP)
	}
	for _, e := range cp.EVSEs {
		if e.UID == uid {
			return e.Reading
		}
	}
	t.Fatalf("no EVSE %s in the snapshot", uid)
	return nil
}

func watts(v float64) *float64 { return &v }

func TestMeterReadingIsTheNewestUntilTheSessionStops(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	s.Connected(testCP)
	s.StartTransaction(testCP, core.TransactionStart{ID: "1", ConnectorID: 1, IDTag: "TAG", At: time.Now()})

	if r := reading(t, s, "EVSE-1"); r != nil {
		t.Fatalf("reading before any MeterValues = %+v, want none", r)
	}

	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.RecordMeterValues(testCP, 1, "", core.MeterReading{At: at, PowerW: watts(11000), Summary: "first"})
	s.RecordMeterValues(testCP, 1, "", core.MeterReading{At: at.Add(10 * time.Second), PowerW: watts(22000), Summary: "second"})
	s.RecordMeterValues(testCP, 2, "", core.MeterReading{At: at, PowerW: watts(7000)})

	r := reading(t, s, "EVSE-1")
	if r == nil || r.PowerW == nil || *r.PowerW != 22000 || !r.At.Equal(at.Add(10*time.Second)) {
		t.Fatalf("EVSE-1 reading = %+v, want the newest, 22 kW", r)
	}
	// A snapshot must not alias core's state.
	*r.PowerW = 1
	if again := reading(t, s, "EVSE-1"); *again.PowerW != 22000 {
		t.Fatalf("changing a snapshot changed core: %v W", *again.PowerW)
	}

	if _, ok := s.StopTransaction(testCP, "1", 0, "Local", time.Now()); !ok {
		t.Fatal("StopTransaction did not find the session")
	}
	if r := reading(t, s, "EVSE-1"); r != nil {
		t.Errorf("EVSE-1 reading after its session stopped = %+v, want none", r)
	}
	if r := reading(t, s, "EVSE-2"); r == nil || *r.PowerW != 7000 {
		t.Errorf("EVSE-2 reading = %+v, want it untouched by EVSE-1's stop", r)
	}
}

func TestMeterReadingsGoWithTheConnection(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	s.Connected(testCP)
	s.RecordMeterValues(testCP, 1, "", core.MeterReading{PowerW: watts(11000)})
	if r := reading(t, s, "EVSE-1"); r == nil || r.At.IsZero() {
		t.Fatalf("reading = %+v, want one stamped with the time it arrived", r)
	}

	s.Disconnected(testCP, "socket closed")
	if r := reading(t, s, "EVSE-1"); r != nil {
		t.Errorf("reading after the station disconnected = %+v, want none", r)
	}
}

// TestMeterValuesForAStoppedSessionAreDropped: a station may deliver a
// session's last MeterValues after its StopTransaction (an offline queue
// replayed out of order, or right at the boundary). That must not bring the
// draw back on an EVSE with no session.
func TestMeterValuesForAStoppedSessionAreDropped(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	s.Connected(testCP)
	s.StartTransaction(testCP, core.TransactionStart{ID: "1", ConnectorID: 1, IDTag: "TAG", At: time.Now()})
	s.StartTransaction(testCP, core.TransactionStart{ID: "2", ConnectorID: 2, IDTag: "TAG", At: time.Now()})

	s.RecordMeterValues(testCP, 1, "1", core.MeterReading{PowerW: watts(50000)})
	if r := reading(t, s, "EVSE-1"); r == nil || *r.PowerW != 50000 {
		t.Fatalf("reading during the session = %+v, want 50 kW", r)
	}
	if _, ok := s.StopTransaction(testCP, "1", 0, "Local", time.Now()); !ok {
		t.Fatal("StopTransaction did not find the session")
	}

	s.RecordMeterValues(testCP, 1, "1", core.MeterReading{PowerW: watts(50000)})
	if r := reading(t, s, "EVSE-1"); r != nil {
		t.Errorf("reading after a late MeterValues of the stopped session = %+v, want none", r)
	}
	// Unknown ids, and a session running on another EVSE, are not this
	// EVSE's session either.
	s.RecordMeterValues(testCP, 1, "99", core.MeterReading{PowerW: watts(1)})
	s.RecordMeterValues(testCP, 1, "2", core.MeterReading{PowerW: watts(2)})
	if r := reading(t, s, "EVSE-1"); r != nil {
		t.Errorf("reading for a transaction not active on EVSE-1 = %+v, want none", r)
	}

	// Without a transaction id the reading is the connector's own, such as
	// clock-aligned Power.Offered on an idle or Reserved connector: kept.
	s.RecordMeterValues(testCP, 1, "", core.MeterReading{PowerOfferedW: watts(150000)})
	if r := reading(t, s, "EVSE-1"); r == nil || r.PowerOfferedW == nil || *r.PowerOfferedW != 150000 {
		t.Errorf("reading without a transaction id = %+v, want 150 kW offered", r)
	}
}

// TestMeterReadingsMergePerField: stations interleave MeterValues with
// different measurand sets (clock-aligned energy between periodic samples, a
// Transaction.Begin with energy only). A value the newest message lacks keeps
// its previous one, with that value's own time, so a carried-over value is
// not shown as fresh.
func TestMeterReadingsMergePerField(t *testing.T) {
	t.Parallel()

	s := core.New(testConfig())
	s.Connected(testCP)
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Minute)

	s.RecordMeterValues(testCP, 1, "", core.MeterReading{
		At: at, PowerW: watts(50000), PowerOfferedW: watts(22000), Summary: "sampled",
	})
	energy := 1000
	s.RecordMeterValues(testCP, 1, "", core.MeterReading{At: later, EnergyWh: &energy, Summary: "aligned"})

	r := reading(t, s, "EVSE-1")
	if r == nil || r.PowerOfferedW == nil || *r.PowerOfferedW != 22000 || r.PowerW == nil || *r.PowerW != 50000 {
		t.Fatalf("reading = %+v, want 50 kW and 22 kW offered kept from the earlier message", r)
	}
	if r.EnergyWh == nil || *r.EnergyWh != 1000 {
		t.Errorf("energy = %v, want 1000 Wh from the newest message", r.EnergyWh)
	}
	if !r.PowerAt.Equal(at) || !r.PowerOfferedAt.Equal(at) || !r.EnergyAt.Equal(later) {
		t.Errorf("value times: power %v, offered %v, energy %v; want %v, %v, %v",
			r.PowerAt, r.PowerOfferedAt, r.EnergyAt, at, at, later)
	}
	if !r.CurrentAt.IsZero() || !r.CurrentOfferedAt.IsZero() {
		t.Errorf("times for values never reported: %v, %v; want zero", r.CurrentAt, r.CurrentOfferedAt)
	}
	if !r.At.Equal(later) || r.Summary != "aligned" {
		t.Errorf("reading at %v %q, want the newest message's", r.At, r.Summary)
	}

	// A new value replaces its own field only.
	s.RecordMeterValues(testCP, 1, "", core.MeterReading{At: later, PowerOfferedW: watts(11000)})
	r = reading(t, s, "EVSE-1")
	if *r.PowerOfferedW != 11000 || !r.PowerOfferedAt.Equal(later) || *r.PowerW != 50000 || !r.PowerAt.Equal(at) {
		t.Errorf("reading = %+v, want 11 kW offered now and 50 kW from before", r)
	}
}
