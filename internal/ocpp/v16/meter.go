package v16

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
)

// Measurands and contexts read from MeterValues (OCPP 1.6 §7.31, §7.45).
const (
	measurandEnergy         = "Energy.Active.Import.Register"
	measurandPower          = "Power.Active.Import"
	measurandCurrent        = "Current.Import"
	measurandPowerOffered   = "Power.Offered"
	measurandCurrentOffered = "Current.Offered"

	contextPeriodic = "Sample.Periodic"
	contextTxBegin  = "Transaction.Begin"
	contextTxEnd    = "Transaction.End"
)

// meterReading reads the newest MeterValue of a MeterValues.req into a
// version-agnostic reading. Values it cannot read are left out and described
// in skipped, for the caller to log: a station sending one odd value must not
// lose the rest, nor get a CALLERROR it would retry.
//
// The rules, per measurand:
//   - A value without a phase is the total and wins.
//   - Without a total, power is the sum of the phases and current is the
//     highest phase: that is the phase a current limit has to respect.
//   - Transaction.Begin and Transaction.End values are dropped when the same
//     set has a Sample.Periodic one (the default context): a station that
//     sends both at a boundary means the periodic one as the live draw.
func meterReading(values []MeterValue, now time.Time) (r core.MeterReading, skipped []string) {
	r.Summary = summariseMeterValues(values)
	if len(values) == 0 {
		r.At = now
		return r, nil
	}
	last := values[len(values)-1]
	r.At = parseTime(last.Timestamp, now)

	samples := last.SampledValue
	if hasPeriodic(samples) {
		kept := make([]SampledValue, 0, len(samples))
		for _, sv := range samples {
			if sv.Context != contextTxBegin && sv.Context != contextTxEnd {
				kept = append(kept, sv)
			}
		}
		samples = kept
	}

	read := func(measurand string, units map[string]float64, combine func(a, b float64) float64) *float64 {
		var total *float64
		var fromPhases *float64
		seen := map[string]bool{}
		for _, sv := range samples {
			if measurandOf(sv) != measurand {
				continue
			}
			v, err := sampleValue(sv, units)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s %q %s: %v", measurand, sv.Value, sv.Unit, err))
				continue
			}
			if sv.Phase == "" {
				if total == nil {
					total = &v
				}
				continue
			}
			line, ok := lineOf(sv.Phase)
			if !ok || seen[line] {
				// N, and line-to-line voltages, are not a line's share; a
				// line reported twice (L1 and L1-N) is counted once.
				continue
			}
			seen[line] = true
			if fromPhases == nil {
				fromPhases = &v
			} else {
				*fromPhases = combine(*fromPhases, v)
			}
		}
		if total != nil {
			return total
		}
		return fromPhases
	}

	sum := func(a, b float64) float64 { return a + b }
	watts := map[string]float64{"": 1, "W": 1, "kW": 1000}
	amps := map[string]float64{"": 1, "A": 1}

	r.PowerW = read(measurandPower, watts, sum)
	r.CurrentA = read(measurandCurrent, amps, math.Max)
	r.PowerOfferedW = read(measurandPowerOffered, watts, sum)
	r.CurrentOfferedA = read(measurandCurrentOffered, amps, math.Max)
	if wh := read(measurandEnergy, map[string]float64{"": 1, "Wh": 1, "kWh": 1000}, sum); wh != nil {
		v := int(math.Round(*wh))
		r.EnergyWh = &v
	}
	return r, skipped
}

// hasPeriodic reports whether a set has a Sample.Periodic value, which is
// also what a value without a context is.
func hasPeriodic(samples []SampledValue) bool {
	for _, sv := range samples {
		if sv.Context == "" || sv.Context == contextPeriodic {
			return true
		}
	}
	return false
}

// measurandOf applies the 1.6 default for an absent measurand.
func measurandOf(sv SampledValue) string {
	if sv.Measurand == "" {
		return measurandEnergy
	}
	return sv.Measurand
}

// sampleValue parses a value and scales it by its unit. units maps every unit
// accepted for the measurand to its factor; "" is the unit left out.
func sampleValue(sv SampledValue, units map[string]float64) (float64, error) {
	factor, ok := units[sv.Unit]
	if !ok {
		return 0, fmt.Errorf("unexpected unit")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(sv.Value), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("not a number")
	}
	return v * factor, nil
}

// lineOf maps a phase to the line it measures: L1 and L1-N are both line 1.
func lineOf(phase string) (string, bool) {
	switch phase {
	case "L1", "L1-N":
		return "L1", true
	case "L2", "L2-N":
		return "L2", true
	case "L3", "L3-N":
		return "L3", true
	default:
		return "", false
	}
}
