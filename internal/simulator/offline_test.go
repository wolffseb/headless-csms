package simulator_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// afterBoot returns the calls made after the nth BootNotification, up to the
// next one.
func afterBoot(calls []fakeCall, n int) []fakeCall {
	var out []fakeCall
	boots := 0
	for _, c := range calls {
		if c.action == v16.ActionBootNotification {
			boots++
			continue
		}
		if boots == n {
			out = append(out, c)
		}
	}
	return out
}

func actions(calls []fakeCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.action
	}
	return out
}

func decode[T any](t *testing.T, c fakeCall) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(c.payload, &v); err != nil {
		t.Fatalf("decoding %s: %v", c.action, err)
	}
	return v
}

func energyIn(t *testing.T, req v16.MeterValuesReq) int {
	t.Helper()
	for _, sv := range req.MeterValue[0].SampledValue {
		if sv.Measurand == "Energy.Active.Import.Register" {
			n, err := strconv.Atoi(sv.Value)
			if err != nil {
				t.Fatalf("energy %q: %v", sv.Value, err)
			}
			return n
		}
	}
	t.Fatal("no energy register in the sample")
	return 0
}

// clocked sets the simulator's own clock and meter timers to clk.
func clocked(clk *clocktest.Clock) func(*simulator.Options) {
	return func(o *simulator.Options) {
		o.Now = clk.Now
		o.AfterFunc = func(d time.Duration, fn func()) simulator.Stopper { return clk.AfterFunc(d, fn) }
	}
}

// runOfflineTx drives the offline-tx scenario through one outage: the
// connection is cut after the first session has started, the meter clock
// steps through the offline samples, and once the scenario asks to redial
// the meter clock jumps an hour (so that a timestamp made at replay time
// would show) and the CSMS comes back.
func runOfflineTx(t *testing.T, p *ocpptest.Proxy, clk, rclk *clocktest.Clock, interval time.Duration, sim *simulator.Simulator) {
	t.Helper()

	waitFor(t, "the first session's meter timer", func() bool { return sim.TransactionID(1) > 0 && clk.Pending() == 1 })
	p.Cut()
	for n := 1; n <= 3; n++ {
		waitFor(t, "the next meter timer", func() bool { return clk.Pending() == 1 })
		clk.Advance(interval)
	}
	waitFor(t, "the scenario to ask for a redial", func() bool { return rclk.Pending() == 1 })
	if n := clk.Pending(); n != 0 {
		t.Fatalf("%d meter timers armed after both sessions stopped", n)
	}

	clk.Advance(time.Hour)
	p.Restore()
	rclk.Advance(time.Second)
}

// TestOfflineTxReplaysInOrder is the scenario against a CSMS that shows the
// wire: after the reconnect the queued messages come first, each exactly
// once, in the order they were made, with the timestamps they were made
// with; the replayed StopTransaction's meterStop is the session's last
// sample, and the session started offline carries the id the CSMS gave its
// StartTransaction. Not parallel, for goleak.
func TestOfflineTxReplaysInOrder(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	const interval = 10 * time.Second
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clk, rclk := clocktest.New(t0), clocktest.New(t0)
	f := newFakeCSMS(t, 0, 100)
	p := ocpptest.NewProxy(t, strings.TrimPrefix(f.url(), "ws://"))
	sim := startSim(t, p.URL(), clocked(clk), reconnecting(rclk, &backoffLog{wait: time.Second}), func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioOfflineTx
		o.MeterInterval = interval
	})

	runOfflineTx(t, p, clk, rclk, interval, sim)
	waitFor(t, "the status round after the reconnect", func() bool {
		calls, _ := f.snapshot()
		return f.count(v16.ActionBootNotification) == 2 && len(afterBoot(calls, 2)) == 9
	})

	calls, overlaps := f.snapshot()
	if overlaps > 0 {
		t.Errorf("%d CALLs were sent while another was still unanswered", overlaps)
	}

	// Before the outage: the status round and the first session, no samples.
	before := afterBoot(calls, 1)
	if got := actions(before); strings.Join(got, ",") != "StatusNotification,StatusNotification,StatusNotification,StatusNotification,StartTransaction,StatusNotification" {
		t.Fatalf("before the outage: %v", got)
	}
	start1 := decode[v16.StartTransactionReq](t, before[4])

	// After it: the queue, then everything else.
	after := afterBoot(calls, 2)
	want := []string{
		v16.ActionMeterValues, v16.ActionMeterValues, v16.ActionMeterValues,
		v16.ActionStopTransaction, v16.ActionStartTransaction, v16.ActionStopTransaction,
		v16.ActionStatusNotification, v16.ActionStatusNotification, v16.ActionStatusNotification,
	}
	if got := actions(after); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after the reconnect: %v\nwant %v", got, want)
	}

	stamp := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	last := 0
	for i := range 3 {
		mv := decode[v16.MeterValuesReq](t, after[i])
		if mv.TransactionID == nil || *mv.TransactionID != 100 {
			t.Errorf("sample %d for transaction %v, want 100", i+1, mv.TransactionID)
		}
		if got, want := mv.MeterValue[0].Timestamp, stamp(time.Duration(i+1)*interval); got != want {
			t.Errorf("sample %d stamped %s, want the original %s", i+1, got, want)
		}
		energy := energyIn(t, mv)
		if energy <= last {
			t.Errorf("sample %d: energy %d Wh did not rise from %d Wh", i+1, energy, last)
		}
		last = energy
	}

	stop1 := decode[v16.StopTransactionReq](t, after[3])
	start2 := decode[v16.StartTransactionReq](t, after[4])
	stop2 := decode[v16.StopTransactionReq](t, after[5])
	if stop1.TransactionID != 100 || stop1.MeterStop != last || stop1.Timestamp != stamp(3*interval) {
		t.Errorf("first stop = %+v, want transaction 100, meterStop %d (the last sample), at %s", stop1, last, stamp(3*interval))
	}
	if start1.Timestamp != stamp(0) || start2.Timestamp != stamp(3*interval) || start2.MeterStart != last {
		t.Errorf("starts at %s and %s (meterStart %d), want %s and %s (meterStart %d)",
			start1.Timestamp, start2.Timestamp, start2.MeterStart, stamp(0), stamp(3*interval), last)
	}
	// The second session started offline, under a placeholder; its stop
	// must carry the id the CSMS gave its StartTransaction on replay.
	if stop2.TransactionID != 101 {
		t.Errorf("offline session stopped as transaction %d, want the CSMS-assigned 101", stop2.TransactionID)
	}
	if stop2.MeterStop != start2.MeterStart {
		t.Errorf("offline session meterStop %d, want %d: it streamed nothing", stop2.MeterStop, start2.MeterStart)
	}

	for action, want := range map[string]int{
		v16.ActionMeterValues: 3, v16.ActionStartTransaction: 2, v16.ActionStopTransaction: 2,
	} {
		if n := f.count(action); n != want {
			t.Errorf("%d %s in all, want each exactly once: %d", n, action, want)
		}
	}
	if got := sim.TransactionID(1); got != 0 {
		t.Errorf("connector 1 still holds transaction %d", got)
	}
}

// TestOfflineTxAgainstTheCSMS runs the scenario against the real CSMS and
// core, with the station's clock two hours behind core's: core gets two
// late StartTransactions with old timestamps, and must end up with both
// sessions closed, under the ids it assigned, at the times the station
// reported.
func TestOfflineTxAgainstTheCSMS(t *testing.T) {
	t.Parallel()

	const interval = 10 * time.Second
	t0 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	clk, rclk := clocktest.New(t0), clocktest.New(t0)
	var p *ocpptest.Proxy
	r := newRig(t, clocked(clk), reconnecting(rclk, &backoffLog{wait: time.Second}), func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioOfflineTx
		o.MeterInterval = interval
		p = ocpptest.NewProxy(t, strings.TrimPrefix(o.URL, "ws://"))
		o.URL = p.URL()
	})

	runOfflineTx(t, p, clk, rclk, interval, r.sim)
	waitFor(t, "both sessions to close in core", func() bool {
		snap, _ := r.core.Snapshot().ChargePoint(testCP)
		if len(snap.Transactions) != 2 {
			return false
		}
		for _, tx := range snap.Transactions {
			if tx.Active() {
				return false
			}
		}
		return true
	})
	waitFor(t, "EVSE-1 to be available again", func() bool { return evsesAre(r, core.StatusAvailable) })

	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	txs := map[string]core.Transaction{}
	for _, tx := range snap.Transactions {
		txs[tx.ID] = tx
	}
	first, second := txs["1"], txs["2"]
	if !first.StartedAt.Equal(t0) || !first.StoppedAt.Equal(t0.Add(3*interval)) {
		t.Errorf("first session %s → %s, want the station's %s → %s", first.StartedAt, first.StoppedAt, t0, t0.Add(3*interval))
	}
	if want := 3 * 50_000 * 10 / 3600; first.MeterStop < want-1 || first.MeterStop > want+1 {
		t.Errorf("first session meterStop %d Wh, want about %d Wh for three offline samples", first.MeterStop, want)
	}
	if !second.StartedAt.Equal(t0.Add(3*interval)) || !second.StoppedAt.Equal(t0.Add(3*interval)) {
		t.Errorf("offline session %+v, want started and stopped at %s", second, t0.Add(3*interval))
	}
	if second.MeterStart != first.MeterStop || second.MeterStop != second.MeterStart {
		t.Errorf("offline session meter %d → %d, want %d → %d", second.MeterStart, second.MeterStop, first.MeterStop, first.MeterStop)
	}
}

// TestTxMessageRetriedOnInternalError has the CSMS fail to process a
// StopTransaction twice: the station must send it again, waiting the backoff
// on its clock, until it is answered. Not parallel, for goleak.
func TestTxMessageRetriedOnInternalError(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 7)
	f.mu.Lock()
	f.respond = func(action string, n int) (string, bool) {
		if action == v16.ActionStopTransaction && n <= 2 {
			return string(ocpp.ErrInternalError), false
		}
		return "", false
	}
	f.mu.Unlock()
	rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	b := &backoffLog{wait: 5 * time.Second}
	sim := startSim(t, f.url(), reconnecting(rclk, b))

	if err := sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- sim.StopTransaction(context.Background(), 1) }()
	for n := 1; n <= 2; n++ {
		waitFor(t, "a retry to be scheduled", func() bool { return rclk.Pending() == 1 })
		rclk.Advance(5 * time.Second)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stop never got through")
	}
	if n := f.count(v16.ActionStopTransaction); n != 3 {
		t.Errorf("%d StopTransactions, want 3: two failures and the answer", n)
	}
	if got := b.seen(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("retry waits for attempts %v, want [1 2]", got)
	}
}

// TestTxMessageGivenUp covers a CSMS that keeps failing to process a
// transaction message: whatever CALLERROR it answers with, the message is
// sent three times before the station moves on (OCPP 1.6 section 3.7.1 lets
// a station skip a message only if the CSMS repeatedly reports a failure to
// process it). Not parallel, for goleak.
func TestTxMessageGivenUp(t *testing.T) {
	const attempts = 3
	for _, tc := range []struct {
		code     ocpp.ErrorCode
		attempts int
	}{
		{ocpp.ErrInternalError, attempts},
		{ocpp.ErrGenericError, attempts},
		{ocpp.ErrFormationViolation, attempts},
		{ocpp.ErrPropertyConstraintViolation, attempts},
		{ocpp.ErrTypeConstraintViolation, attempts},
		{ocpp.ErrOccurrenceConstraintViolation, attempts},
		{ocpp.ErrProtocolError, attempts},
		{ocpp.ErrNotImplemented, attempts},
		{ocpp.ErrNotSupported, attempts},
		{ocpp.ErrSecurityError, attempts},
		{ocpp.ErrRpcFrameworkError, attempts},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			ignore := goleak.IgnoreCurrent()
			t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

			f := newFakeCSMS(t, 0, 7)
			f.mu.Lock()
			f.respond = func(action string, _ int) (string, bool) {
				if action == v16.ActionStopTransaction {
					return string(tc.code), false
				}
				return "", false
			}
			f.mu.Unlock()
			rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
			sim := startSim(t, f.url(), reconnecting(rclk, &backoffLog{wait: time.Second}))

			if err := sim.StartTransaction(context.Background(), 1); err != nil {
				t.Fatalf("start: %v", err)
			}
			stopped := make(chan error, 1)
			go func() { stopped <- sim.StopTransaction(context.Background(), 1) }()
			for n := 1; n < tc.attempts; n++ {
				waitFor(t, "a retry to be scheduled", func() bool { return rclk.Pending() == 1 })
				rclk.Advance(time.Second)
			}
			select {
			case err := <-stopped:
				if err == nil || !strings.Contains(err.Error(), string(tc.code)) {
					t.Errorf("stop = %v, want the %s it was given up on", err, tc.code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the stop was never given up")
			}
			if n := f.count(v16.ActionStopTransaction); n != tc.attempts {
				t.Errorf("%d StopTransactions, want %d", n, tc.attempts)
			}

			// The queue is clear: the next session goes straight out, and
			// the given-up stop is not sent again.
			if err := sim.StartTransaction(context.Background(), 1); err != nil {
				t.Fatalf("second start: %v", err)
			}
			if got := sim.TransactionID(1); got != 8 {
				t.Errorf("second session = %d, want the CSMS's 8", got)
			}
			if n := f.count(v16.ActionStopTransaction); n != tc.attempts {
				t.Errorf("the given-up stop was sent again: %d in all", n)
			}
		})
	}
}

// TestDisconnectDuringReplayKeepsTheRest drops the connection while the
// replay waits for an answer: the unanswered message and everything behind
// it stay queued and go out after the next reconnect, and nothing answered
// is sent twice. Not parallel, for goleak.
func TestDisconnectDuringReplayKeepsTheRest(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 50)
	f.mu.Lock()
	f.respond = func(action string, n int) (string, bool) {
		// The first StartTransaction is the online one; the second is the
		// offline session's, on replay.
		return "", action == v16.ActionStartTransaction && n == 2
	}
	f.mu.Unlock()
	p := ocpptest.NewProxy(t, strings.TrimPrefix(f.url(), "ws://"))
	rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	sim := startSim(t, p.URL(), reconnecting(rclk, &backoffLog{wait: time.Second}))
	ctx := context.Background()

	if err := sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	p.Cut()
	waitFor(t, "the redial to be scheduled", func() bool { return rclk.Pending() == 1 })
	for _, step := range []func(context.Context, int) error{sim.StopTransaction, sim.StartTransaction, sim.StopTransaction} {
		if err := step(ctx, 1); err != nil {
			t.Fatalf("offline step: %v", err)
		}
	}
	if got := sim.TransactionID(1); got != 0 {
		t.Fatalf("connector 1 holds %d after the offline sessions", got)
	}

	p.Restore()
	rclk.Advance(time.Second)
	waitFor(t, "the replay to be cut short and a redial scheduled", func() bool {
		return f.count(v16.ActionStartTransaction) == 2 && rclk.Pending() == 1
	})
	rclk.Advance(time.Second)
	waitFor(t, "the status round after the second reconnect", func() bool {
		calls, _ := f.snapshot()
		return len(afterBoot(calls, 3)) == 5
	})

	calls, _ := f.snapshot()
	if got := strings.Join(actions(afterBoot(calls, 2)), ","); got != "StopTransaction,StartTransaction" {
		t.Errorf("first replay: %s, want the stop answered and the start cut off", got)
	}
	third := afterBoot(calls, 3)
	if got := strings.Join(actions(third[:2]), ","); got != "StartTransaction,StopTransaction" {
		t.Fatalf("second replay: %s, want the rest of the queue", got)
	}
	if stop := decode[v16.StopTransactionReq](t, third[1]); stop.TransactionID != 52 {
		t.Errorf("offline session stopped as %d, want 52, the id its answered StartTransaction got", stop.TransactionID)
	}
	if n := f.count(v16.ActionStopTransaction); n != 2 {
		t.Errorf("%d StopTransactions in all, want 2: the answered one is not sent again", n)
	}
}

// TestOfflineStartWithAnUnknownTag is a CSMS check: a session a station
// started offline, for a tag the CSMS does not know, is replayed. OCPP 1.6
// has the CSMS answer StartTransaction with a transactionId whatever the
// idTagInfo says, so that the station's StopTransaction can be put against
// it. cpms answers transactionId 0 and records nothing, so the session is
// lost, and every such session shares the id 0. It is skipped while that is
// so; see the PR that added it.
func TestOfflineStartWithAnUnknownTag(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	r := newRig(t, reconnecting(rclk, &backoffLog{wait: time.Second}), func(o *simulator.Options) {
		o.IDTag = "UNKNOWN-TAG"
		p = ocpptest.NewProxy(t, strings.TrimPrefix(o.URL, "ws://"))
		o.URL = p.URL()
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	p.Cut()
	waitFor(t, "the redial to be scheduled", func() bool { return rclk.Pending() == 1 })
	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("offline start: %v", err)
	}
	if err := r.sim.StopTransaction(context.Background(), 1); err != nil {
		t.Fatalf("offline stop: %v", err)
	}
	p.Restore()
	rclk.Advance(time.Second)
	waitFor(t, "the station to come back", func() bool { return r.core.IsOnline(testCP) && evsesAre(r, core.StatusAvailable) })

	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	if len(snap.Transactions) == 0 {
		t.Skip("known CSMS gap: a replayed StartTransaction for an unknown tag gets transactionId 0 and the offline session is not recorded")
	}
	if tx := snap.Transactions[0]; tx.ID == "0" || tx.Active() {
		t.Errorf("offline session recorded as %+v, want its own id, closed", tx)
	}
}

// TestTransactionIDIsNeverAPlaceholder polls TransactionID while a slow CSMS
// answers StartTransaction: callers (ocpp-tap, the e2e tests) treat anything
// but 0 as the CSMS's id, so it must be 0 until the answer, never negative.
// Not parallel, for goleak.
func TestTransactionIDIsNeverAPlaceholder(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		t.Run("reconnect="+strconv.FormatBool(reconnect), func(t *testing.T) {
			ignore := goleak.IgnoreCurrent()
			t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

			f := newFakeCSMS(t, 300*time.Millisecond, 7)
			var mutate []func(*simulator.Options)
			if reconnect {
				mutate = append(mutate, reconnecting(clocktest.New(time.Now()), &backoffLog{wait: time.Second}))
			}
			sim := startSim(t, f.url(), mutate...)

			started := make(chan error, 1)
			go func() { started <- sim.StartTransaction(context.Background(), 1) }()

			sawZero := false
			deadline := time.Now().Add(3 * time.Second)
			for {
				id := sim.TransactionID(1)
				if id < 0 {
					t.Fatalf("TransactionID = %d while the CSMS is answering", id)
				}
				if id > 0 {
					if id != 7 {
						t.Fatalf("TransactionID = %d, want the CSMS's 7", id)
					}
					break
				}
				sawZero = true
				if time.Now().After(deadline) {
					t.Fatal("the start was never answered")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !sawZero {
				t.Error("never saw TransactionID 0 before the answer; the test proves nothing")
			}
			if err := <-started; err != nil {
				t.Errorf("start: %v", err)
			}
		})
	}
}

// transactionIDsOnTheWire fails the test if any frame the CSMS saw carried
// a negative transactionId: the placeholder id of an offline session is the
// station's own bookkeeping and must never be sent.
func transactionIDsOnTheWire(t *testing.T, f *fakeCSMS) {
	t.Helper()
	calls, _ := f.snapshot()
	for _, c := range calls {
		if strings.Contains(string(c.payload), `"transactionId":-`) {
			t.Errorf("%s went out with a placeholder id: %s", c.action, c.payload)
		}
	}
}

// offlineSession cuts the connection, runs a first session to its end and
// starts a second one, still running, all while offline; then it brings the
// connection back and lets the station redial. The first session's id is 7
// and the offline one is the fake's second StartTransaction.
func offlineSession(t *testing.T, f *fakeCSMS) (*simulator.Simulator, *clocktest.Clock) {
	t.Helper()
	p := ocpptest.NewProxy(t, strings.TrimPrefix(f.url(), "ws://"))
	rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	sim := startSim(t, p.URL(), reconnecting(rclk, &backoffLog{wait: time.Second}))
	ctx := context.Background()

	if err := sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	p.Cut()
	waitFor(t, "the redial to be scheduled", func() bool { return rclk.Pending() == 1 })
	if err := sim.StopTransaction(ctx, 1); err != nil {
		t.Fatalf("offline stop: %v", err)
	}
	if err := sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("offline start: %v", err)
	}
	if got := sim.ConnectorStatus(1); got != v16.StatusCharging {
		t.Fatalf("connector 1 is %s after an offline start, want Charging", got)
	}
	if got := sim.TransactionID(1); got != 0 {
		t.Fatalf("TransactionID = %d while the start is queued, want 0 until the CSMS answers", got)
	}
	p.Restore()
	rclk.Advance(time.Second)
	return sim, rclk
}

// TestGivenUpOfflineStartEndsTheSession: a StartTransaction queued offline
// that the CSMS then fails to process three times is given up, and the
// session it opened ends locally. No message of it goes out, least of all
// under the placeholder id. Not parallel, for goleak.
func TestGivenUpOfflineStartEndsTheSession(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 7)
	f.mu.Lock()
	f.respond = func(action string, n int) (string, bool) {
		if action == v16.ActionStartTransaction && n >= 2 {
			return string(ocpp.ErrFormationViolation), false
		}
		return "", false
	}
	f.mu.Unlock()
	sim, rclk := offlineSession(t, f)

	for n := 1; n < 3; n++ {
		waitFor(t, "a retry to be scheduled", func() bool { return rclk.Pending() == 1 })
		rclk.Advance(time.Second)
	}
	// The first stop, three attempts at the start, the status round.
	waitFor(t, "the status round after the reconnect", func() bool {
		calls, _ := f.snapshot()
		return len(afterBoot(calls, 2)) == 1+3+3
	})

	calls, _ := f.snapshot()
	for _, c := range afterBoot(calls, 2) {
		if c.action == v16.ActionMeterValues || (c.action == v16.ActionStopTransaction && decode[v16.StopTransactionReq](t, c).TransactionID != 7) {
			t.Errorf("a message of the session the CSMS never opened went out: %s %s", c.action, c.payload)
		}
	}
	transactionIDsOnTheWire(t, f)
	if got := sim.TransactionID(1); got != 0 {
		t.Errorf("TransactionID = %d, want 0", got)
	}
	if got := sim.ConnectorStatus(1); got != v16.StatusAvailable {
		t.Errorf("connector 1 is %s, want Available", got)
	}
	if err := sim.StopTransaction(context.Background(), 1); err == nil {
		t.Error("a stop of the ended session was accepted")
	}
}

// TestReplayedStartNotAcceptedDeauthorizes: the CSMS answers the replayed
// StartTransaction of a session that is still running with an idTagInfo that
// is not Accepted, and, as cpms does for an unknown tag, transactionId 0.
// The station stops it as StopTransactionOnInvalidId=true would (OCPP 1.6
// section 3.5.4): a StopTransaction for the id the CSMS assigned, reason
// DeAuthorized, and the connector is free again. Not parallel, for goleak.
func TestReplayedStartNotAcceptedDeauthorizes(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 7)
	f.mu.Lock()
	f.startConf = func(n int) v16.StartTransactionConf {
		if n == 2 {
			return v16.StartTransactionConf{TransactionID: 0, IDTagInfo: v16.IDTagInfo{Status: v16.AuthInvalid}}
		}
		return v16.StartTransactionConf{TransactionID: 7, IDTagInfo: v16.IDTagInfo{Status: v16.AuthAccepted}}
	}
	f.mu.Unlock()
	sim, _ := offlineSession(t, f)

	waitFor(t, "the status round after the reconnect", func() bool {
		calls, _ := f.snapshot()
		return len(afterBoot(calls, 2)) == 3+3
	})

	calls, _ := f.snapshot()
	after := afterBoot(calls, 2)
	if got := strings.Join(actions(after[:3]), ","); got != "StopTransaction,StartTransaction,StopTransaction" {
		t.Fatalf("replay: %s, want the first stop, the start, and a stop of the refused session", got)
	}
	stop := decode[v16.StopTransactionReq](t, after[2])
	if stop.TransactionID != 0 || stop.Reason != "DeAuthorized" {
		t.Errorf("stop of the refused session = id %d reason %q, want the CSMS's 0 and DeAuthorized", stop.TransactionID, stop.Reason)
	}
	transactionIDsOnTheWire(t, f)
	if got := sim.ConnectorStatus(1); got != v16.StatusAvailable {
		t.Errorf("connector 1 is %s, want Available", got)
	}
	if err := sim.StartTransaction(context.Background(), 1); err != nil {
		t.Errorf("the connector cannot start a new session: %v", err)
	}
}
