package push_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	"github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// station is an in-process CSMS with a simulator dialled in, over the live
// rig's core.
type station struct {
	r   *rig
	sim *simulator.Simulator
	ctx context.Context
}

func newStation(t *testing.T, r *rig) *station {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := csms.New(csms.Options{
		Bind:     "127.0.0.1:0",
		Core:     r.core.Service,
		Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: v16.NewHandler(r.cfg, r.core.Service, log)},
		NewChargePoint: func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
			return v16.NewChargePoint(conn, r.cfg), nil
		},
		Accept:      func(id string) bool { return id == r.cfg.Charger.ID },
		CallTimeout: 5 * time.Second,
		IdleTimeout: r.cfg.Charger.HeartbeatTimeout.Duration(),
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	sim, err := simulator.New(simulator.Options{
		URL:        "ws://" + server.Addr(),
		ID:         r.cfg.Charger.ID,
		Connectors: len(r.cfg.Location.EVSEs),
		IDTag:      r.cfg.Auth.DefaultIDTag,
		Log:        log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); sim.Close(); sim.Wait() })
	if err := sim.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	return &station{r: r, sim: sim, ctx: ctx}
}

func (s *station) setStatus(evse int, status string) {
	s.r.t.Helper()
	if err := s.sim.SetConnectorStatus(s.ctx, s.r.cfg.Location.EVSEs[evse].OCPPConnectorID, status); err != nil {
		s.r.t.Fatal(err)
	}
}

// quiet fails the test if another PATCH shows up within a window longer than
// the debounce.
func (r *rig) quiet(have int) {
	r.t.Helper()
	time.Sleep(r.debounce + 300*time.Millisecond)
	if n := len(r.emsp.got()); n != have {
		r.t.Fatalf("%d PATCHes, want it to stay at %d", n, have)
	}
}

// The whole path on the real clock: the station reports Charging over OCPP,
// and exactly one PATCH reaches the partner within a second, with the path
// from config, TOKEN_B, and the status.
func TestStationChangeIsPushedOnce(t *testing.T) {
	t.Parallel()
	r := newLiveRig(t)
	r.register()
	s := newStation(t, r)

	// Boot: both connectors report Available.
	got := r.emsp.waitPatches(r.evses())
	r.quiet(len(got))

	s.setStatus(0, "Charging")
	deadline := time.Now().Add(time.Second)
	eventually(t, "the PATCH for Charging", func() bool { return len(r.emsp.got()) > r.evses() })
	if time.Now().After(deadline) {
		t.Errorf("the PATCH took more than a second")
	}
	got = r.emsp.got()
	p := got[r.evses()]
	if p.path != r.evsePath(0) || p.auth != r.authHeader() || p.status() != "CHARGING" {
		t.Errorf("PATCH %s %q %s", p.path, p.auth, p.raw)
	}
	if _, ok := p.body["last_updated"].(string); !ok || len(p.body) != 2 {
		t.Errorf("body %s: want exactly status and last_updated", p.raw)
	}
	r.quiet(r.evses() + 1)
}

// The station going away is UNKNOWN for every EVSE, once each.
func TestStationDisconnectPushesUnknownForEveryEVSE(t *testing.T) {
	t.Parallel()
	r := newLiveRig(t)
	r.register()
	s := newStation(t, r)
	r.emsp.waitPatches(r.evses())
	r.quiet(r.evses())

	s.sim.Close()
	s.sim.Wait()
	got := r.emsp.waitPatches(2 * r.evses())
	r.quiet(2 * r.evses())

	seen := map[string]bool{}
	for _, p := range got[r.evses():] {
		if p.status() != string(core.StatusUnknown) {
			t.Errorf("%s: %s, want UNKNOWN", p.path, p.status())
		}
		seen[p.path] = true
	}
	for i := range r.cfg.Location.EVSEs {
		if !seen[r.evsePath(i)] {
			t.Errorf("no UNKNOWN PATCH for %s", r.cfg.Location.EVSEs[i].UID)
		}
	}
}
