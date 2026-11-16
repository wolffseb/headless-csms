package v16_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// powerLog passes every call on to the real handler, and keeps the
// Power.Active.Import samples of the MeterValues among them. Nothing in core
// records power yet, so this is how a test sees what the station streams.
type powerLog struct {
	ocpp.Handler

	mu      sync.Mutex
	samples map[int][]int // per connector, oldest first
}

func (l *powerLog) HandleCall(ctx context.Context, id, action string, payload json.RawMessage) (any, *ocpp.RPCError) {
	if action == v16.ActionMeterValues {
		var req v16.MeterValuesReq
		if err := json.Unmarshal(payload, &req); err == nil {
			for _, mv := range req.MeterValue {
				for _, sv := range mv.SampledValue {
					if sv.Measurand != "Power.Active.Import" {
						continue
					}
					if w, err := strconv.Atoi(sv.Value); err == nil {
						l.mu.Lock()
						if l.samples == nil {
							l.samples = map[int][]int{}
						}
						l.samples[req.ConnectorID] = append(l.samples[req.ConnectorID], w)
						l.mu.Unlock()
					}
				}
			}
		}
	}
	return l.Handler.HandleCall(ctx, id, action, payload)
}

// since returns the connector's samples from index from on.
func (l *powerLog) since(connectorID, from int) []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.samples[connectorID]
	if from >= len(s) {
		return nil
	}
	return append([]int(nil), s[from:]...)
}

func (l *powerLog) count(connectorID int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.samples[connectorID])
}

// settlesAt waits for a sample of exactly want W taken after mark, and then
// checks that nothing after it went above want.
func (r *simRig) settlesAt(t *testing.T, connectorID, mark, want int) int {
	t.Helper()

	at := -1
	pollUntil(t, "power to settle at "+strconv.Itoa(want)+" W", func() bool {
		for i, w := range r.power.since(connectorID, mark) {
			if w == want {
				at = mark + i
				return true
			}
		}
		return false
	})
	// A few more samples, all within the limit.
	pollUntil(t, "more samples", func() bool { return r.power.count(connectorID) >= at+4 })
	for _, w := range r.power.since(connectorID, at) {
		if w > want {
			t.Errorf("connector %d drew %d W after settling at %d W", connectorID, w, want)
		}
	}
	return r.power.count(connectorID)
}

// TestChargingProfileWhileReserved is the smart-charging question asked of
// the simulator: is a TxDefault limit taken while the EVSE is Reserved, and
// does the session that redeems the reservation honour it?
func TestChargingProfileWhileReserved(t *testing.T) {
	t.Parallel()

	r := newSimRig(t, 2*time.Second, func(o *simulator.Options) { o.MeterInterval = 20 * time.Millisecond })

	res, err := r.cp.ReserveNow(ctx3s(t), ocpp.ReserveRequest{
		ReservationID: 42, EVSEUID: "EVSE-1", IDTag: testTag, ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	expectStatus(t, "ReserveNow", res, err, ocpp.CommandAccepted)

	// TxDefault 20 kW: taken, and the reservation stands.
	txDefault := ocpp.ChargingProfile{
		ID: 1, Purpose: ocpp.PurposeTxDefault, Kind: ocpp.KindAbsolute, EVSEUID: "EVSE-1",
		Schedule: ocpp.ChargingSchedule{
			StartSchedule: ptr(time.Now().Add(-time.Minute)),
			Unit:          ocpp.UnitW,
			Periods:       []ocpp.SchedulePeriod{{Start: 0, Limit: 20000}},
		},
	}
	res, err = r.cp.SetChargingProfile(ctx3s(t), txDefault)
	expectStatus(t, "SetChargingProfile TxDefault", res, err, ocpp.CommandAccepted)
	if got := r.sim.ConnectorStatus(1); got != v16.StatusReserved {
		t.Errorf("connector 1 is %s after the profile, want still Reserved", got)
	}

	unit := ocpp.UnitW
	sched, err := r.cp.GetCompositeSchedule(ctx3s(t), "EVSE-1", time.Hour, &unit)
	if err != nil || sched.Status != ocpp.CommandAccepted || sched.Schedule == nil ||
		len(sched.Schedule.Periods) != 1 || sched.Schedule.Periods[0].Limit != 20000 {
		t.Fatalf("GetCompositeSchedule = %+v (%+v), %v; want one period of 20000 W", sched, sched.Schedule, err)
	}

	// A TxProfile has no transaction to apply to while the EVSE is reserved.
	txProfile := func(txID string, limit float64) ocpp.ChargingProfile {
		return ocpp.ChargingProfile{
			ID: 2, StackLevel: 1, Purpose: ocpp.PurposeTx, Kind: ocpp.KindRelative,
			TransactionID: txID, EVSEUID: "EVSE-1",
			Schedule: ocpp.ChargingSchedule{Unit: ocpp.UnitW, Periods: []ocpp.SchedulePeriod{{Start: 0, Limit: limit}}},
		}
	}
	res, err = r.cp.SetChargingProfile(ctx3s(t), txProfile("1", 10000))
	expectStatus(t, "SetChargingProfile Tx while reserved", res, err, ocpp.CommandRejected)

	// The reserved driver arrives; the session streams at most 20 kW.
	res, err = r.cp.RemoteStart(ctx3s(t), ocpp.RemoteStartRequest{EVSEUID: "EVSE-1", IDTag: testTag})
	expectStatus(t, "RemoteStart", res, err, ocpp.CommandAccepted)
	pollUntil(t, "the transaction to start", func() bool { return r.sim.TransactionID(1) != 0 })
	txID := strconv.Itoa(r.sim.TransactionID(1))
	mark := r.settlesAt(t, 1, 0, 20000)
	for _, w := range r.power.since(1, 0) {
		if w > 20000 {
			t.Errorf("the session drew %d W under a 20 kW TxDefault", w)
		}
	}

	// A TxProfile for the running session overrides the TxDefault.
	res, err = r.cp.SetChargingProfile(ctx3s(t), txProfile(txID, 10000))
	expectStatus(t, "SetChargingProfile Tx", res, err, ocpp.CommandAccepted)
	mark = r.settlesAt(t, 1, mark, 10000)

	// Clearing it by id falls back to the TxDefault; clearing that to the
	// car's own 50 kW.
	res, err = r.cp.ClearChargingProfile(ctx3s(t), ocpp.ClearProfileRequest{ID: ptr(2)})
	expectStatus(t, "ClearChargingProfile Tx", res, err, ocpp.CommandAccepted)
	mark = r.settlesAt(t, 1, mark, 20000)

	res, err = r.cp.ClearChargingProfile(ctx3s(t), ocpp.ClearProfileRequest{ID: ptr(1)})
	expectStatus(t, "ClearChargingProfile TxDefault", res, err, ocpp.CommandAccepted)
	r.settlesAt(t, 1, mark, 50000)

	res, err = r.cp.ClearChargingProfile(ctx3s(t), ocpp.ClearProfileRequest{ID: ptr(1)})
	expectStatus(t, "ClearChargingProfile again", res, err, ocpp.CommandUnknownProfile)
}

func TestChargingProfileUnderRejectProfile(t *testing.T) {
	t.Parallel()

	r := newSimRig(t, 2*time.Second, func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectProfile })
	res, err := r.cp.SetChargingProfile(ctx3s(t), txDefaultProfile(time.Now()))
	expectStatus(t, "SetChargingProfile", res, err, ocpp.CommandRejected)
}
