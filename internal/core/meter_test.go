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
	s.RecordMeterValues(testCP, 1, core.MeterReading{At: at, PowerW: watts(11000), Summary: "first"})
	s.RecordMeterValues(testCP, 1, core.MeterReading{At: at.Add(10 * time.Second), PowerW: watts(22000), Summary: "second"})
	s.RecordMeterValues(testCP, 2, core.MeterReading{At: at, PowerW: watts(7000)})

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
	s.RecordMeterValues(testCP, 1, core.MeterReading{PowerW: watts(11000)})
	if r := reading(t, s, "EVSE-1"); r == nil || r.At.IsZero() {
		t.Fatalf("reading = %+v, want one stamped with the time it arrived", r)
	}

	s.Disconnected(testCP, "socket closed")
	if r := reading(t, s, "EVSE-1"); r != nil {
		t.Errorf("reading after the station disconnected = %+v, want none", r)
	}
}
