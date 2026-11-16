package cli

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// TestMeterCellShowsEachValuesAge: core merges MeterValues per field, so a
// value can be older than the newest message. The cell then says how old
// each one is instead of passing a carried-over value off as fresh.
func TestMeterCellShowsEachValuesAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 16, 12, 1, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	f := func(v float64) *float64 { return &v }

	for _, tt := range []struct {
		name string
		e    socket.EVSEStatus
		want string
	}{{
		name: "none",
		want: "-",
	}, {
		name: "one message",
		e: socket.EVSEStatus{
			PowerW: f(50000), PowerAt: ago(3 * time.Second), CurrentA: f(125), CurrentAt: ago(3 * time.Second),
			PowerOfferedW: f(150000), PowerOfferedAt: ago(3 * time.Second), ReadingAt: ago(3 * time.Second),
		},
		want: "50.0 kW 125.0 A, offered 150.0 kW, 3s ago",
	}, {
		name: "offered from an older message",
		e: socket.EVSEStatus{
			PowerW: f(50000), PowerAt: ago(3 * time.Second), CurrentA: f(125), CurrentAt: ago(3 * time.Second),
			PowerOfferedW: f(22000), PowerOfferedAt: ago(time.Minute),
			CurrentOfferedA: f(32), CurrentOfferedAt: ago(time.Minute), ReadingAt: ago(3 * time.Second),
		},
		want: "50.0 kW 125.0 A 3s ago, offered 22.0 kW 32.0 A 1m0s ago",
	}, {
		name: "values within a group differ",
		e: socket.EVSEStatus{
			PowerW: f(50000), PowerAt: ago(3 * time.Second), CurrentA: f(125), CurrentAt: ago(time.Minute),
			ReadingAt: ago(3 * time.Second),
		},
		want: "50.0 kW 3s ago 125.0 A 1m0s ago",
	}, {
		name: "offered only, as clock-aligned data on a Reserved connector",
		e: socket.EVSEStatus{
			PowerOfferedW: f(22000), PowerOfferedAt: ago(10 * time.Second), ReadingAt: ago(10 * time.Second),
		},
		want: "offered 22.0 kW, 10s ago",
	}, {
		name: "energy only",
		e:    socket.EVSEStatus{ReadingAt: ago(5 * time.Second)},
		want: "no power reported, 5s ago",
	}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := meterCell(tt.e, now); got != tt.want {
				t.Errorf("meterCell = %q, want %q", got, tt.want)
			}
		})
	}
}
