package v16

import (
	"fmt"
	"testing"
	"time"
)

func TestMeterReadingExtraction(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	sampled := time.Date(2026, 11, 16, 11, 59, 30, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	sv := func(measurand, value, unit string) SampledValue {
		return SampledValue{Measurand: measurand, Value: value, Unit: unit}
	}
	phase := func(s SampledValue, p string) SampledValue { s.Phase = p; return s }
	ctx := func(s SampledValue, c string) SampledValue { s.Context = c; return s }

	tests := []struct {
		name    string
		samples []SampledValue
		power   *float64
		current *float64
		offerP  *float64
		offerI  *float64
		energy  *int
		skipped int
	}{
		{
			name:    "total in W",
			samples: []SampledValue{sv(measurandPower, "50000", "W"), sv(measurandEnergy, "1234", "Wh")},
			power:   f(50000), energy: i(1234),
		},
		{
			name:    "kW and kWh are converted",
			samples: []SampledValue{sv(measurandPower, "11.5", "kW"), sv(measurandEnergy, "12.3456", "kWh")},
			power:   f(11500), energy: i(12346),
		},
		{
			name:    "no unit is the base unit",
			samples: []SampledValue{sv(measurandPower, "7400", ""), sv("", "99", "")},
			power:   f(7400), energy: i(99),
		},
		{
			name: "phases only: power summed, current the highest phase",
			samples: []SampledValue{
				phase(sv(measurandPower, "3600", "W"), "L1-N"),
				phase(sv(measurandPower, "3700", "W"), "L2-N"),
				phase(sv(measurandPower, "3500", "W"), "L3-N"),
				phase(sv(measurandCurrent, "15.6", "A"), "L1"),
				phase(sv(measurandCurrent, "16.1", "A"), "L2"),
				phase(sv(measurandCurrent, "15.2", "A"), "L3"),
				phase(sv(measurandCurrent, "40", "A"), "N"),
			},
			power: f(10800), current: f(16.1),
		},
		{
			name: "a total wins over phases",
			samples: []SampledValue{
				phase(sv(measurandPower, "3600", "W"), "L1"),
				sv(measurandPower, "11000", "W"),
				phase(sv(measurandPower, "3700", "W"), "L2"),
			},
			power: f(11000),
		},
		{
			name: "a line reported twice counts once",
			samples: []SampledValue{
				phase(sv(measurandPower, "3600", "W"), "L1"),
				phase(sv(measurandPower, "3600", "W"), "L1-N"),
				phase(sv(measurandPower, "3700", "W"), "L2"),
			},
			power: f(7300),
		},
		{
			name:    "current only",
			samples: []SampledValue{sv(measurandCurrent, "125", "A")},
			current: f(125),
		},
		{
			name: "offered values beside the import",
			samples: []SampledValue{
				sv(measurandPower, "50000", "W"), sv(measurandPowerOffered, "150", "kW"),
				sv(measurandCurrent, "125", "A"), sv(measurandCurrentOffered, "200", "A"),
			},
			power: f(50000), current: f(125), offerP: f(150000), offerI: f(200),
		},
		{
			name: "Begin and End are dropped beside a periodic value",
			samples: []SampledValue{
				ctx(sv(measurandPower, "0", "W"), contextTxBegin),
				ctx(sv(measurandPower, "22000", "W"), contextPeriodic),
				ctx(sv(measurandCurrent, "1", "A"), contextTxEnd),
			},
			power: f(22000),
		},
		{
			name: "no context is periodic",
			samples: []SampledValue{
				ctx(sv(measurandPower, "0", "W"), contextTxEnd),
				sv(measurandPower, "22000", "W"),
			},
			power: f(22000),
		},
		{
			name: "Begin and End only are kept",
			samples: []SampledValue{
				ctx(sv(measurandPower, "1500", "W"), contextTxBegin),
				ctx(sv(measurandEnergy, "100", "Wh"), contextTxBegin),
			},
			power: f(1500), energy: i(100),
		},
		{
			name: "an unreadable value is skipped, the rest kept",
			samples: []SampledValue{
				sv(measurandPower, "fifty", "W"),
				sv(measurandCurrent, "125", "A"),
				sv(measurandCurrentOffered, "200", "mA"),
				sv(measurandEnergy, "NaN", "Wh"),
			},
			current: f(125), skipped: 3,
		},
		{
			name:    "measurands cpms does not read are ignored",
			samples: []SampledValue{sv("SoC", "45", "Percent"), sv("Voltage", "400", "V")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, skipped := meterReading([]MeterValue{
				{Timestamp: "2026-11-16T11:00:00Z", SampledValue: []SampledValue{sv(measurandPower, "1", "W")}},
				{Timestamp: sampled.Format(time.RFC3339), SampledValue: tt.samples},
			}, now)

			if !r.At.Equal(sampled) {
				t.Errorf("At = %v, want the newest value's timestamp %v", r.At, sampled)
			}
			check(t, "PowerW", r.PowerW, tt.power)
			check(t, "CurrentA", r.CurrentA, tt.current)
			check(t, "PowerOfferedW", r.PowerOfferedW, tt.offerP)
			check(t, "CurrentOfferedA", r.CurrentOfferedA, tt.offerI)
			check(t, "EnergyWh", r.EnergyWh, tt.energy)
			if len(skipped) != tt.skipped {
				t.Errorf("skipped %q, want %d", skipped, tt.skipped)
			}
			if r.Summary != summariseMeterValues([]MeterValue{{SampledValue: tt.samples}}) {
				t.Errorf("Summary = %q, want the event's summary of the newest value", r.Summary)
			}
		})
	}
}

func TestMeterReadingWithoutValues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	r, skipped := meterReading(nil, now)
	if r.PowerW != nil || r.CurrentA != nil || r.EnergyWh != nil || len(skipped) != 0 {
		t.Errorf("reading = %+v, skipped %q; want nothing", r, skipped)
	}
	if !r.At.Equal(now) || r.Summary != "no samples" {
		t.Errorf("At = %v, Summary = %q", r.At, r.Summary)
	}
}

func check[T comparable](t *testing.T, name string, got, want *T) {
	t.Helper()
	if fmt.Sprint(deref(got)) != fmt.Sprint(deref(want)) {
		t.Errorf("%s = %v, want %v", name, deref(got), deref(want))
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
