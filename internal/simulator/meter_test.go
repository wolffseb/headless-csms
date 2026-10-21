package simulator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/core"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// energyOf pulls the energy register out of a MeterValues event's summary,
// which the 1.6 handler renders as "Energy.Active.Import.Register=123Wh ...".
func energyOf(t *testing.T, e core.Event) int {
	t.Helper()

	for _, part := range strings.Fields(e.Detail) {
		if v, ok := strings.CutPrefix(part, "Energy.Active.Import.Register="); ok {
			n, err := strconv.Atoi(strings.TrimSuffix(v, "Wh"))
			if err != nil {
				t.Fatalf("energy in %q: %v", e.Detail, err)
			}
			return n
		}
	}
	t.Fatalf("no energy register in %q", e.Detail)
	return 0
}

// nextMeterValues waits for the next MeterValues event, skipping the rest.
func nextMeterValues(t *testing.T, events <-chan core.Event) core.Event {
	t.Helper()

	timeout := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == core.EventMeterValues {
				return e
			}
		case <-timeout:
			t.Fatal("timed out waiting for MeterValues")
		}
	}
}

// noMeterValues fails if a MeterValues event arrives within d.
func noMeterValues(t *testing.T, events <-chan core.Event, d time.Duration) {
	t.Helper()

	timeout := time.After(d)
	for {
		select {
		case e := <-events:
			if e.Kind == core.EventMeterValues {
				t.Fatalf("unexpected MeterValues: %s", e.Detail)
			}
		case <-timeout:
			return
		}
	}
}

// TestMeterValuesStreamDuringATransaction drives the stream with the fake
// clock: one sample per interval, energy following power × time, nothing
// before the session or after it, and meterStop equal to the last sample.
func TestMeterValuesStreamDuringATransaction(t *testing.T) {
	t.Parallel()

	const interval = 10 * time.Second
	clk := clocktest.New(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	r := newRig(t, func(o *simulator.Options) {
		o.MeterInterval = interval
		o.Now = clk.Now
		o.AfterFunc = func(d time.Duration, fn func()) simulator.Stopper { return clk.AfterFunc(d, fn) }
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	// No session, no stream.
	clk.Advance(3 * interval)
	if n := clk.Pending(); n != 0 {
		t.Fatalf("%d meter timers armed without a transaction", n)
	}
	noMeterValues(t, events, 100*time.Millisecond)

	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 50 kW for 10 s is 138.9 Wh a sample.
	last := 0
	for n := 1; n <= 3; n++ {
		waitFor(t, "the next meter timer", func() bool { return clk.Pending() == 1 })
		clk.Advance(interval)

		e := nextMeterValues(t, events)
		if e.EVSEUID != "EVSE-1" {
			t.Errorf("sample %d came from %s, want EVSE-1", n, e.EVSEUID)
		}
		for _, want := range []string{"Power.Active.Import=50000W", "Current.Import=125A", "SoC="} {
			if !strings.Contains(e.Detail, want) {
				t.Errorf("sample %d = %q, missing %s", n, e.Detail, want)
			}
		}
		energy := energyOf(t, e)
		if energy <= last {
			t.Errorf("sample %d: energy %d Wh did not increase from %d Wh", n, energy, last)
		}
		if want := n * 50_000 * 10 / 3600; energy < want-1 || energy > want+1 {
			t.Errorf("sample %d: energy %d Wh, want about %d Wh for 50 kW over %s", n, energy, want, time.Duration(n)*interval)
		}
		last = energy
	}

	if err := r.sim.StopTransaction(context.Background(), 1); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, "the csms to close the transaction", func() bool {
		snap, _ := r.core.Snapshot().ChargePoint(testCP)
		return len(snap.Transactions) == 1 && !snap.Transactions[0].Active()
	})
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	if got := snap.Transactions[0].MeterStop; got != last {
		t.Errorf("meterStop = %d Wh, want the last streamed %d Wh", got, last)
	}

	// And the stream is gone with the session.
	waitFor(t, "the meter timer to be stopped", func() bool { return clk.Pending() == 0 })
	clk.Advance(3 * interval)
	noMeterValues(t, events, 100*time.Millisecond)
}

// TestNoMeterValuesByDefault keeps today's behaviour without --meter-interval:
// a session sends no MeterValues and still moves the meter by a flat kWh.
func TestNoMeterValuesByDefault(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	noMeterValues(t, events, 300*time.Millisecond)
	if err := r.sim.StopTransaction(context.Background(), 1); err != nil {
		t.Fatalf("stop: %v", err)
	}

	waitFor(t, "the csms to close the transaction", func() bool {
		snap, _ := r.core.Snapshot().ChargePoint(testCP)
		return len(snap.Transactions) == 1 && !snap.Transactions[0].Active()
	})
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	if tx := snap.Transactions[0]; tx.MeterStop-tx.MeterStart != 1000 {
		t.Errorf("session moved the meter %d → %d, want the flat 1000 Wh", tx.MeterStart, tx.MeterStop)
	}
}

// TestMeterValuesKeepFlowingWhenSlow combines the stream with the slow
// scenario: commands from the CSMS stall, the station's own messages do not.
func TestMeterValuesKeepFlowingWhenSlow(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 3 * time.Second
		o.MeterInterval = 100 * time.Millisecond
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}

	// A command the station sits on for longer than the 2s call timeout.
	stalled := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := r.conn(t).Call(ctx, v16.ActionUnlockConnector, v16.UnlockConnectorReq{ConnectorID: 1})
		stalled <- err
	}()

	last := 0
	for n := 1; n <= 5; n++ {
		energy := energyOf(t, nextMeterValues(t, events))
		if energy <= last {
			t.Errorf("sample %d: energy %d Wh did not increase from %d Wh", n, energy, last)
		}
		last = energy
	}
	select {
	case err := <-stalled:
		t.Fatalf("the slow command finished (%v) before the samples arrived; it should still be stalled", err)
	default:
	}
	if err := <-stalled; err == nil {
		t.Error("the stalled command should have timed out")
	}
}

func TestSilentStationStreamsNothing(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSilent
		o.MeterInterval = 50 * time.Millisecond
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	noMeterValues(t, events, 400*time.Millisecond)
}

// TestChangeConfigurationMeterValueSampleInterval turns streaming on in the
// middle of a session, reads the setting back, and turns it off again.
func TestChangeConfigurationMeterValueSampleInterval(t *testing.T) {
	t.Parallel()

	clk := clocktest.New(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	r := newRig(t, func(o *simulator.Options) {
		o.Now = clk.Now
		o.AfterFunc = func(d time.Duration, fn func()) simulator.Stopper { return clk.AfterFunc(d, fn) }
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	if err := r.sim.StartTransaction(context.Background(), 1); err != nil {
		t.Fatalf("start: %v", err)
	}

	change := func(value string) string {
		var conf v16.ChangeConfigurationConf
		r.call(t, v16.ActionChangeConfiguration,
			v16.ChangeConfigurationReq{Key: v16.KeyMeterValueSampleInterval, Value: value}, &conf)
		return conf.Status
	}

	if got := change("10"); got != v16.ConfigAccepted {
		t.Fatalf("set 10 = %q, want Accepted", got)
	}
	var get v16.GetConfigurationConf
	r.call(t, v16.ActionGetConfiguration,
		v16.GetConfigurationReq{Key: []string{v16.KeyMeterValueSampleInterval}}, &get)
	if got := configValue(get, v16.KeyMeterValueSampleInterval); got != "10" {
		t.Errorf("GetConfiguration reports %q, want 10", got)
	}

	// The session already running starts streaming.
	waitFor(t, "the meter timer", func() bool { return clk.Pending() == 1 })
	clk.Advance(10 * time.Second)
	if energy := energyOf(t, nextMeterValues(t, events)); energy != 50_000*10/3600 {
		t.Errorf("energy %d Wh, want %d", energy, 50_000*10/3600)
	}

	if got := change("0"); got != v16.ConfigAccepted {
		t.Fatalf("set 0 = %q, want Accepted", got)
	}
	// The next tick sees 0 and ends the stream without sending.
	waitFor(t, "the meter timer", func() bool { return clk.Pending() == 1 })
	clk.Advance(10 * time.Second)
	waitFor(t, "the stream to end", func() bool { return clk.Pending() == 0 })
	noMeterValues(t, events, 100*time.Millisecond)

	if got := change("-5"); got != v16.ConfigRejected {
		t.Errorf("set -5 = %q, want Rejected", got)
	}
}

func TestChangeConfiguration(t *testing.T) {
	t.Parallel()

	r := newRig(t, func(o *simulator.Options) {
		o.UnreportedKeys = []string{v16.KeyLocalPreAuthorize}
	})
	waitFor(t, "the station to come online", func() bool { return r.core.IsOnline(testCP) })

	tests := []struct {
		key, value, want string
		readBack         string
	}{
		{v16.KeyHeartbeatInterval, "300", v16.ConfigAccepted, "300"},
		{v16.KeyHeartbeatInterval, "0", v16.ConfigRejected, "300"},
		{v16.KeyHeartbeatInterval, "soon", v16.ConfigRejected, "300"},
		{v16.KeyConnectionTimeOut, "90", v16.ConfigAccepted, "90"},
		{v16.KeyAuthorizeRemoteTxRequests, "true", v16.ConfigAccepted, "true"},
		{v16.KeyAuthorizeRemoteTxRequests, "maybe", v16.ConfigRejected, "true"},
		{v16.KeyNumberOfConnectors, "4", v16.ConfigRejected, "2"},
		{v16.KeySupportedFeatureProfiles, "Core", v16.ConfigRejected, ""},
		// A key the station does not report is one it does not know.
		{v16.KeyLocalPreAuthorize, "true", v16.ConfigNotSupported, ""},
		{"NoSuchKey", "1", v16.ConfigNotSupported, ""},
	}
	for _, tt := range tests {
		var conf v16.ChangeConfigurationConf
		r.call(t, v16.ActionChangeConfiguration, v16.ChangeConfigurationReq{Key: tt.key, Value: tt.value}, &conf)
		if conf.Status != tt.want {
			t.Errorf("%s=%s: status %q, want %q", tt.key, tt.value, conf.Status, tt.want)
		}
		if tt.readBack == "" {
			continue
		}
		var get v16.GetConfigurationConf
		r.call(t, v16.ActionGetConfiguration, v16.GetConfigurationReq{Key: []string{tt.key}}, &get)
		if got := configValue(get, tt.key); got != tt.readBack {
			t.Errorf("after %s=%s: reads back %q, want %q", tt.key, tt.value, got, tt.readBack)
		}
	}
}

// fakeCSMS is a bare OCPP-J server that answers every CALL after a delay and
// records what it was sent. It exists to see the wire as the station drives
// it, which a real CSMS hides: whether two CALLs were ever outstanding at
// once, in what order they came, and the transactionId on each sample.
type fakeCSMS struct {
	t      *testing.T
	server *httptest.Server
	delay  time.Duration
	txID   int

	mu          sync.Mutex
	outstanding int
	overlaps    int
	calls       []fakeCall
	conn        *websocket.Conn
}

type fakeCall struct {
	action  string
	payload json.RawMessage
}

func newFakeCSMS(t *testing.T, delay time.Duration, txID int) *fakeCSMS {
	t.Helper()

	f := &fakeCSMS{t: t, delay: delay, txID: txID}
	upgrader := websocket.Upgrader{Subprotocols: []string{"ocpp1.6"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = ws
		f.mu.Unlock()
		f.serve(ws)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeCSMS) url() string { return "ws" + strings.TrimPrefix(f.server.URL, "http") }

func (f *fakeCSMS) serve(ws *websocket.Conn) {
	defer func() { _ = ws.Close() }()

	var (
		writeMu sync.Mutex
		replies sync.WaitGroup
	)
	defer replies.Wait()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var frame []json.RawMessage
		if err := json.Unmarshal(data, &frame); err != nil || len(frame) != 4 {
			f.t.Errorf("unexpected frame %s", data)
			continue
		}
		var id, action string
		_ = json.Unmarshal(frame[1], &id)
		_ = json.Unmarshal(frame[2], &action)

		f.mu.Lock()
		if f.outstanding > 0 {
			f.overlaps++
		}
		f.outstanding++
		f.calls = append(f.calls, fakeCall{action: action, payload: frame[3]})
		f.mu.Unlock()

		var result any = struct{}{}
		switch action {
		case v16.ActionBootNotification:
			result = v16.BootNotificationConf{Status: "Accepted", Interval: 60, CurrentTime: time.Now().UTC().Format(time.RFC3339)}
		case v16.ActionStartTransaction:
			result = v16.StartTransactionConf{TransactionID: f.txID, IDTagInfo: v16.IDTagInfo{Status: v16.AuthAccepted}}
		}

		replies.Add(1)
		go func() {
			defer replies.Done()
			time.Sleep(f.delay)
			out, _ := json.Marshal([]any{3, id, result})

			// The CALL stops being outstanding when the answer is on its way,
			// which is the moment the station may send its next one.
			f.mu.Lock()
			f.outstanding--
			f.mu.Unlock()
			writeMu.Lock()
			_ = ws.WriteMessage(websocket.TextMessage, out)
			writeMu.Unlock()
		}()
	}
}

func (f *fakeCSMS) snapshot() (calls []fakeCall, overlaps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...), f.overlaps
}

func (f *fakeCSMS) count(action string) int {
	calls, _ := f.snapshot()
	n := 0
	for _, c := range calls {
		if c.action == action {
			n++
		}
	}
	return n
}

// TestMeterValuesShareTheCallSlot is not parallel: goleak compares against
// the goroutines running when it starts, which parallel tests would disturb.
//
// It streams fast, against a CSMS that answers slowly, while status reports
// are sent from another goroutine: every CALL must still wait for the one
// before it, every sample must carry the CSMS's transactionId, none may follow
// the StopTransaction, and meterStop must equal the last sample.
func TestMeterValuesShareTheCallSlot(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	const txID = 4711
	f := newFakeCSMS(t, 20*time.Millisecond, txID)

	sim, err := simulator.New(simulator.Options{
		URL: f.url(), ID: testCP, Connectors: 2, IDTag: testTag, Log: discard(),
		MeterInterval: 5 * time.Millisecond,
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
	if got := sim.TransactionID(1); got != txID {
		t.Fatalf("simulator transaction = %d, want the CSMS's %d", got, txID)
	}

	// Status reports on the other connector, racing the stream.
	var reporters sync.WaitGroup
	reporters.Add(1)
	go func() {
		defer reporters.Done()
		for range 5 {
			_ = sim.SetConnectorStatus(ctx, 2, v16.StatusAvailable)
		}
	}()

	waitFor(t, "a few samples", func() bool { return f.count(v16.ActionMeterValues) >= 5 })
	reporters.Wait()
	if err := sim.StopTransaction(ctx, 1); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Long enough for a leftover tick to show up if the stream survived.
	time.Sleep(100 * time.Millisecond)

	calls, overlaps := f.snapshot()
	if overlaps > 0 {
		t.Errorf("%d CALLs were sent while another was still unanswered", overlaps)
	}

	lastEnergy, stopped := -1, false
	for _, c := range calls {
		switch c.action {
		case v16.ActionMeterValues:
			if stopped {
				t.Error("MeterValues sent after StopTransaction")
			}
			var req v16.MeterValuesReq
			if err := json.Unmarshal(c.payload, &req); err != nil {
				t.Fatalf("decoding MeterValues: %v", err)
			}
			if req.ConnectorID != 1 || req.TransactionID == nil || *req.TransactionID != txID {
				t.Errorf("sample for connector %d, transaction %v; want 1, %d", req.ConnectorID, req.TransactionID, txID)
			}
			for _, sv := range req.MeterValue[0].SampledValue {
				if sv.Measurand == "Energy.Active.Import.Register" {
					lastEnergy, _ = strconv.Atoi(sv.Value)
				}
			}
		case v16.ActionStopTransaction:
			stopped = true
			var req v16.StopTransactionReq
			if err := json.Unmarshal(c.payload, &req); err != nil {
				t.Fatalf("decoding StopTransaction: %v", err)
			}
			if req.TransactionID != txID {
				t.Errorf("stopped transaction %d, want %d", req.TransactionID, txID)
			}
			if req.MeterStop != lastEnergy {
				t.Errorf("meterStop = %d, want the last streamed %d", req.MeterStop, lastEnergy)
			}
		}
	}
	if !stopped {
		t.Error("no StopTransaction was sent")
	}
}

// TestMeterStreamEndsOnDisconnect drops the connection mid-session: the stream
// must stop and leave no goroutine behind. Not parallel, for goleak.
func TestMeterStreamEndsOnDisconnect(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := newFakeCSMS(t, 0, 7)
	sim, err := simulator.New(simulator.Options{
		URL: f.url(), ID: testCP, IDTag: testTag, Log: discard(),
		MeterInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sim.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the stream to start", func() bool { return f.count(v16.ActionMeterValues) >= 2 })

	f.mu.Lock()
	_ = f.conn.Close()
	f.mu.Unlock()

	select {
	case <-sim.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the simulator did not notice the disconnect")
	}
	waited := make(chan struct{})
	go func() { sim.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("a simulator goroutine outlived the connection")
	}
}
