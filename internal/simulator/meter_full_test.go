package simulator_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// TestFullBatteryReportsSuspendedEV runs a session past the point where the
// car's battery is full: power drops to 0, the register stops rising, and the
// connector reports SuspendedEV exactly once, while the session itself goes
// on until stopped. Not parallel, for goleak.
func TestFullBatteryReportsSuspendedEV(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	const interval = 10 * time.Minute // 50 kW into 75 kWh from 20% takes 72 min
	clk := clocktest.New(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	f := newFakeCSMS(t, 0, 99)
	sim, err := simulator.New(simulator.Options{
		URL: f.url(), ID: testCP, IDTag: testTag, Log: discard(),
		MeterInterval: interval,
		Now:           clk.Now,
		AfterFunc:     func(d time.Duration, fn func()) simulator.Stopper { return clk.AfterFunc(d, fn) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		sim.Close()
		sim.Wait()
	}()
	if err := sim.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Ten samples: the battery is full from the eighth on.
	const samples = 10
	for n := 1; n <= samples; n++ {
		waitFor(t, "the next meter timer", func() bool { return clk.Pending() == 1 })
		clk.Advance(interval)
		waitFor(t, "the sample to arrive", func() bool { return f.count(v16.ActionMeterValues) == n })
	}
	// The status report goes out before the next timer is armed.
	waitFor(t, "the next meter timer", func() bool { return clk.Pending() == 1 })

	type reading struct{ energy, power, soc int }
	var readings []reading
	suspended := 0
	calls, _ := f.snapshot()
	for _, c := range calls {
		switch c.action {
		case v16.ActionMeterValues:
			var req v16.MeterValuesReq
			if err := json.Unmarshal(c.payload, &req); err != nil {
				t.Fatalf("decoding MeterValues: %v", err)
			}
			var r reading
			for _, sv := range req.MeterValue[0].SampledValue {
				n, _ := strconv.Atoi(sv.Value)
				switch sv.Measurand {
				case "Energy.Active.Import.Register":
					r.energy = n
				case "Power.Active.Import":
					r.power = n
				case "SoC":
					r.soc = n
				}
			}
			readings = append(readings, r)
		case v16.ActionStatusNotification:
			var req v16.StatusNotificationReq
			if err := json.Unmarshal(c.payload, &req); err != nil {
				t.Fatalf("decoding StatusNotification: %v", err)
			}
			if req.Status == v16.StatusSuspendedEV {
				suspended++
				if len(readings) != 8 {
					t.Errorf("SuspendedEV came after %d samples, want right after the 8th, the first full one", len(readings))
				}
			}
		}
	}

	if len(readings) != samples {
		t.Fatalf("%d samples, want %d", len(readings), samples)
	}
	if readings[6].power != 50_000 || readings[6].soc >= 100 {
		t.Errorf("sample 7 = %+v, still charging", readings[6])
	}
	full := readings[7]
	if full.soc != 100 || full.energy != 60_000 {
		t.Errorf("sample 8 = %+v, want a full battery at 60000 Wh", full)
	}
	for i, r := range readings[8:] {
		if r.power != 0 || r.energy != full.energy || r.soc != 100 {
			t.Errorf("sample %d = %+v, want power 0 and the register unchanged at %d", i+9, r, full.energy)
		}
	}
	if suspended != 1 {
		t.Errorf("SuspendedEV sent %d times, want once", suspended)
	}
	if got := sim.ConnectorStatus(1); got != v16.StatusSuspendedEV {
		t.Errorf("connector status %q, want SuspendedEV", got)
	}
	if sim.TransactionID(1) == 0 {
		t.Fatal("the session ended by itself; it should run until stopped")
	}

	if err := sim.StopTransaction(ctx, 1); err != nil {
		t.Fatalf("stop: %v", err)
	}
	calls, _ = f.snapshot()
	stopped := false
	for _, c := range calls {
		if c.action != v16.ActionStopTransaction {
			continue
		}
		stopped = true
		var req v16.StopTransactionReq
		if err := json.Unmarshal(c.payload, &req); err != nil {
			t.Fatalf("decoding StopTransaction: %v", err)
		}
		if req.MeterStop != full.energy {
			t.Errorf("meterStop = %d, want the last streamed %d", req.MeterStop, full.energy)
		}
	}
	if !stopped {
		t.Error("no StopTransaction was sent")
	}
}
