package simulator_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// offlineSim is a simulator that never connects: enough to drive its
// command handlers directly.
func offlineSim(t *testing.T, mutate ...func(*simulator.Options)) *simulator.Simulator {
	t.Helper()

	opts := simulator.Options{
		URL: "ws://127.0.0.1:1", ID: testCP, Connectors: 2, IDTag: testTag, Log: discard(),
	}
	for _, m := range mutate {
		m(&opts)
	}
	sim, err := simulator.New(opts)
	if err != nil {
		t.Fatalf("simulator.New: %v", err)
	}
	t.Cleanup(func() {
		sim.Close()
		sim.Wait()
	})
	return sim
}

// handle sends one command to the simulator and decodes its answer into T,
// as the CSMS would see it on the wire.
func handle[T any](t *testing.T, sim *simulator.Simulator, action string, payload any) T {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	conf, rpcErr := sim.HandleCall(context.Background(), "", action, raw)
	if rpcErr != nil {
		t.Fatalf("%s: %v", action, rpcErr)
	}
	b, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("marshal conf: %v", err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decoding %s: %v", action, err)
	}
	return out
}

func profile(id, stack int, purpose string, limits ...float64) v16.ChargingProfile {
	p := v16.ChargingProfile{
		ChargingProfileID:      id,
		StackLevel:             stack,
		ChargingProfilePurpose: purpose,
		ChargingProfileKind:    v16.KindRelative,
		ChargingSchedule:       v16.ChargingSchedule{ChargingRateUnit: v16.UnitW},
	}
	for i, l := range limits {
		p.ChargingSchedule.ChargingSchedulePeriod = append(p.ChargingSchedule.ChargingSchedulePeriod,
			v16.ChargingSchedulePeriod{StartPeriod: i * 3600, Limit: l})
	}
	return p
}

func setProfile(t *testing.T, sim *simulator.Simulator, connectorID int, p v16.ChargingProfile) string {
	t.Helper()
	return handle[v16.SetChargingProfileConf](t, sim, v16.ActionSetChargingProfile,
		v16.SetChargingProfileReq{ConnectorID: connectorID, CSChargingProfiles: p}).Status
}

func composite(t *testing.T, sim *simulator.Simulator, connectorID, seconds int, unit string) v16.GetCompositeScheduleConf {
	t.Helper()
	return handle[v16.GetCompositeScheduleConf](t, sim, v16.ActionGetCompositeSchedule,
		v16.GetCompositeScheduleReq{ConnectorID: connectorID, Duration: seconds, ChargingRateUnit: unit})
}

// limits lists the composite schedule's periods as "start:limit".
func limits(conf v16.GetCompositeScheduleConf) []string {
	if conf.ChargingSchedule == nil {
		return nil
	}
	var out []string
	for _, p := range conf.ChargingSchedule.ChargingSchedulePeriod {
		b, _ := json.Marshal(p.Limit)
		out = append(out, strings.Join([]string{jsonInt(p.StartPeriod), string(b)}, ":"))
	}
	return out
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func equal(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestCompositeScheduleCombinesPurposes(t *testing.T) {
	t.Parallel()

	sim := offlineSim(t)

	// No profile: the station's own maximum.
	if got := limits(composite(t, sim, 1, 60, "")); !equal(got, []string{"0:50000"}) {
		t.Errorf("no profiles: %v, want 0:50000", got)
	}

	// TxDefault with a step down after an hour, and a higher stack level
	// that wins over it.
	for _, tt := range []struct {
		connector int
		p         v16.ChargingProfile
	}{
		{1, profile(1, 0, v16.PurposeTxDefault, 30000, 11000)},
		{1, profile(2, 1, v16.PurposeTxDefault, 20000, 11000)},
	} {
		if got := setProfile(t, sim, tt.connector, tt.p); got != v16.ProfileAccepted {
			t.Fatalf("profile %d: %s", tt.p.ChargingProfileID, got)
		}
	}
	if got := limits(composite(t, sim, 1, 7200, v16.UnitW)); !equal(got, []string{"0:20000", "3600:11000"}) {
		t.Errorf("TxDefault: %v, want 0:20000, 3600:11000", got)
	}
	// Within the duration only.
	if got := limits(composite(t, sim, 1, 3600, v16.UnitW)); !equal(got, []string{"0:20000"}) {
		t.Errorf("one hour: %v, want 0:20000", got)
	}
	// Connector 2 has none of connector 1's profiles.
	if got := limits(composite(t, sim, 2, 60, v16.UnitW)); !equal(got, []string{"0:50000"}) {
		t.Errorf("connector 2: %v, want 0:50000", got)
	}

	// A station maximum below the TxDefault caps every connector.
	if got := setProfile(t, sim, 0, profile(3, 0, v16.PurposeChargePointMax, 15000)); got != v16.ProfileAccepted {
		t.Fatalf("station max: %s", got)
	}
	if got := limits(composite(t, sim, 1, 7200, v16.UnitW)); !equal(got, []string{"0:15000", "3600:11000"}) {
		t.Errorf("with station max: %v, want 0:15000, 3600:11000", got)
	}
	if got := limits(composite(t, sim, 2, 60, v16.UnitW)); !equal(got, []string{"0:15000"}) {
		t.Errorf("connector 2 with station max: %v, want 0:15000", got)
	}

	// Units are not converted.
	if got := composite(t, sim, 1, 60, v16.UnitA); got.Status != v16.CompositeRejected {
		t.Errorf("asking in A for W profiles: %s, want Rejected", got.Status)
	}
	if got := composite(t, sim, 3, 60, v16.UnitW); got.Status != v16.CompositeRejected {
		t.Errorf("unknown connector: %s, want Rejected", got.Status)
	}
}

func TestSetChargingProfileReplacesAndRefuses(t *testing.T) {
	t.Parallel()

	sim := offlineSim(t)
	set := func(connector int, p v16.ChargingProfile, want string) {
		t.Helper()
		if got := setProfile(t, sim, connector, p); got != want {
			t.Errorf("profile %d (%s, stack %d) on %d: %s, want %s",
				p.ChargingProfileID, p.ChargingProfilePurpose, p.StackLevel, connector, got, want)
		}
	}

	set(1, profile(1, 0, v16.PurposeTxDefault, 20000), v16.ProfileAccepted)
	// Same id: replaced.
	set(1, profile(1, 0, v16.PurposeTxDefault, 18000), v16.ProfileAccepted)
	// Same purpose and stack level, new id: replaced too.
	set(1, profile(2, 0, v16.PurposeTxDefault, 16000), v16.ProfileAccepted)
	if got := limits(composite(t, sim, 1, 60, "")); !equal(got, []string{"0:16000"}) {
		t.Errorf("after replacing: %v, want 0:16000", got)
	}
	// Profile 1 is gone, so clearing it finds nothing.
	id := 1
	if got := handle[v16.ClearChargingProfileConf](t, sim, v16.ActionClearChargingProfile,
		v16.ClearChargingProfileReq{ID: &id}); got.Status != v16.ClearUnknown {
		t.Errorf("clearing the replaced profile: %s, want Unknown", got.Status)
	}

	// The limits the configuration keys advertise.
	set(1, profile(3, 9, v16.PurposeTxDefault, 1000), v16.ProfileRejected)
	set(1, profile(3, 8, v16.PurposeTxDefault, 1000), v16.ProfileAccepted)
	many := make([]float64, 25)
	set(1, profile(4, 1, v16.PurposeTxDefault, many...), v16.ProfileRejected)
	set(1, profile(4, 1, v16.PurposeTxDefault, many[:24]...), v16.ProfileAccepted)
	// Profiles 2, 3 and 4 are installed; seven more make ten.
	for i := 5; i <= 11; i++ {
		set(2, profile(i, i-5, v16.PurposeTxDefault, 1000), v16.ProfileAccepted)
	}
	set(2, profile(12, 7, v16.PurposeTxDefault, 1000), v16.ProfileRejected)
	// Replacing one at the limit is fine.
	set(2, profile(11, 6, v16.PurposeTxDefault, 2000), v16.ProfileAccepted)

	// Purposes on the wrong connector.
	set(1, profile(20, 0, v16.PurposeChargePointMax, 1000), v16.ProfileRejected)
	set(3, profile(21, 0, v16.PurposeTxDefault, 1000), v16.ProfileRejected)
	// No transaction runs, so a TxProfile has nothing to apply to.
	set(1, profile(22, 0, v16.PurposeTx, 1000), v16.ProfileRejected)
}

func TestRejectProfileScenario(t *testing.T) {
	t.Parallel()

	sim := offlineSim(t, func(o *simulator.Options) { o.Scenario = simulator.ScenarioRejectProfile })
	if got := setProfile(t, sim, 0, profile(1, 0, v16.PurposeChargePointMax, 20000)); got != v16.ProfileRejected {
		t.Errorf("SetChargingProfile under reject-profile: %s, want Rejected", got)
	}
}

// TestProfilesSurviveAReset follows errata 3.37: profiles set by
// SetChargingProfile persist across reboots, so a Reset is no way to drop a
// limit.
func TestProfilesSurviveAReset(t *testing.T) {
	t.Parallel()

	sim := offlineSim(t)
	if got := setProfile(t, sim, 1, profile(1, 0, v16.PurposeTxDefault, 20000)); got != v16.ProfileAccepted {
		t.Fatalf("set: %s", got)
	}
	handle[v16.ResetConf](t, sim, v16.ActionReset, v16.ResetReq{Type: v16.ResetSoft})
	if got := limits(composite(t, sim, 1, 60, "")); !equal(got, []string{"0:20000"}) {
		t.Errorf("after Reset: %v, want the TxDefault's 0:20000", got)
	}
}

func TestTxProfileEndsWithItsTransaction(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	ctx := context.Background()
	if err := r.sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	txID := r.sim.TransactionID(1)

	other := txID + 1
	p := profile(1, 0, v16.PurposeTx, 10000)
	p.TransactionID = &other
	if got := setProfile(t, r.sim, 1, p); got != v16.ProfileRejected {
		t.Errorf("TxProfile for another transaction: %s, want Rejected", got)
	}
	p.TransactionID = &txID
	if got := setProfile(t, r.sim, 1, p); got != v16.ProfileAccepted {
		t.Fatalf("TxProfile: %s", got)
	}
	if got := limits(composite(t, r.sim, 1, 60, "")); !equal(got, []string{"0:10000"}) {
		t.Errorf("during the transaction: %v, want 0:10000", got)
	}

	if err := r.sim.StopTransaction(ctx, 1); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := limits(composite(t, r.sim, 1, 60, "")); !equal(got, []string{"0:50000"}) {
		t.Errorf("after the transaction: %v, want 0:50000", got)
	}
	id := 1
	if got := handle[v16.ClearChargingProfileConf](t, r.sim, v16.ActionClearChargingProfile,
		v16.ClearChargingProfileReq{ID: &id}); got.Status != v16.ClearUnknown {
		t.Errorf("the TxProfile is still installed: clearing it answered %s", got.Status)
	}
}

func TestProfilesSurviveAReconnect(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	clk := clocktest.New(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	r := newRig(t, reconnecting(clk, &backoffLog{wait: time.Second}), func(o *simulator.Options) {
		p = ocpptest.NewProxy(t, strings.TrimPrefix(o.URL, "ws://"))
		o.URL = p.URL()
	})
	waitFor(t, "both EVSEs to report available", func() bool { return evsesAre(r, core.StatusAvailable) })

	if got := setProfile(t, r.sim, 1, profile(1, 0, v16.PurposeTxDefault, 20000)); got != v16.ProfileAccepted {
		t.Fatalf("set: %s", got)
	}

	p.Cut()
	waitFor(t, "core to see the station go", func() bool { return !r.core.IsOnline(testCP) })
	waitFor(t, "the redial to be scheduled", func() bool { return clk.Pending() == 1 })
	p.Restore()
	clk.Advance(time.Second)
	waitFor(t, "the station to come back", func() bool { return r.core.IsOnline(testCP) })

	// Asked over the new connection, as the CSMS would.
	var conf v16.GetCompositeScheduleConf
	r.call(t, v16.ActionGetCompositeSchedule, v16.GetCompositeScheduleReq{ConnectorID: 1, Duration: 60}, &conf)
	if got := limits(conf); !equal(got, []string{"0:20000"}) {
		t.Errorf("after the reconnect: %v, want 0:20000", got)
	}
}
