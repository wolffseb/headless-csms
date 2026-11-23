package control_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// Two chargers, one EVSE each, as on the coneva bench.
const (
	cpA = "PRUESTEL_000"
	cpB = "PRUESTEL_001"
)

// named is a fake station that answers to its own charger id.
type named struct {
	*fakeStation
	id string
}

func (n named) ID() string { return n.id }

type fleet map[string]*fakeStation

func (f fleet) Command(id string) (ocpp.ChargePoint, error) {
	st, ok := f[id]
	if !ok {
		return nil, ocpp.ErrNotConnected
	}
	return named{st, id}, nil
}

type multiRig struct {
	clk   *clocktest.Clock
	core  *core.Service
	ctl   *control.Service
	cfg   *config.Config
	fleet fleet
}

func newMultiRig(t *testing.T) *multiRig {
	t.Helper()

	cfg := &config.Config{
		Chargers: []config.Charger{{ID: cpA}, {ID: cpB}},
		Auth:     config.Auth{DefaultIDTag: "RFID_PRUESTEL_000", IDTags: []string{"RFID_PRUESTEL_001"}},
		Location: config.Location{EVSEs: []config.EVSE{
			{UID: "PRUESTEL_000-1", Charger: cpA, OCPPConnectorID: 1},
			{UID: "PRUESTEL_001-1", Charger: cpB, OCPPConnectorID: 1},
			{UID: "PRUESTEL_001-2", Charger: cpB, OCPPConnectorID: 2},
		}},
	}
	clk := clocktest.New(t0)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := core.New(cfg, core.WithClock(clk.Now), core.WithLogger(discard),
		core.WithAfterFunc(func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) }))
	t.Cleanup(svc.Close)

	f := fleet{cpA: newFakeStation(), cpB: newFakeStation()}
	ctl := control.New(control.Options{
		Core: svc, Commands: f, Config: cfg, Now: clk.Now, Log: discard,
		AfterFunc: func(d time.Duration, fn func()) core.Stopper { return clk.AfterFunc(d, fn) },
	})
	t.Cleanup(ctl.Close)
	return &multiRig{clk: clk, core: svc, ctl: ctl, cfg: cfg, fleet: f}
}

func startedOn(f *fakeStation) []ocpp.RemoteStartRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.starts)
}

// A command that names an EVSE reaches that EVSE's charger and no other.
func TestCommandsGoToTheChargerOfTheEVSE(t *testing.T) {
	t.Parallel()
	r := newMultiRig(t)
	ctx := context.Background()

	if err := r.ctl.Start(ctx, "PRUESTEL_001-2", "RFID_PRUESTEL_001"); err != nil {
		t.Fatal(err)
	}
	if err := r.ctl.Start(ctx, "PRUESTEL_000-1", ""); err != nil {
		t.Fatal(err)
	}
	a, b := startedOn(r.fleet[cpA]), startedOn(r.fleet[cpB])
	if len(a) != 1 || a[0].EVSEUID != "PRUESTEL_000-1" || a[0].IDTag != "RFID_PRUESTEL_000" {
		t.Errorf("charger A got %+v, want one start of PRUESTEL_000-1 with the default tag", a)
	}
	if len(b) != 1 || b[0].EVSEUID != "PRUESTEL_001-2" || b[0].IDTag != "RFID_PRUESTEL_001" {
		t.Errorf("charger B got %+v, want one start of PRUESTEL_001-2 with the second tag", b)
	}

	if _, err := r.ctl.Reserve(ctx, control.ReserveParams{EVSEUID: "PRUESTEL_001-1"}); err != nil {
		t.Fatal(err)
	}
	if got := len(r.fleet[cpA].reserve); got != 0 {
		t.Errorf("charger A got %d ReserveNow for an EVSE of charger B", got)
	}
	if got := len(r.fleet[cpB].reserve); got != 1 {
		t.Errorf("charger B got %d ReserveNow, want 1", got)
	}
	if res, ok := r.core.ActiveReservation("PRUESTEL_001-1"); !ok || res.ChargePointID != cpB {
		t.Errorf("reservation = %+v, want one recorded for %s", res, cpB)
	}
}

// A command for a station is refused until it is told which one, and the
// refusal lists the ids; nothing reaches either station.
func TestStationCommandsNeedACharger(t *testing.T) {
	t.Parallel()
	r := newMultiRig(t)
	ctx := context.Background()
	kw := 7.0

	cases := map[string]func() error{
		"probe":  func() error { _, err := r.ctl.Probe(ctx, ""); return err },
		"config": func() error { _, err := r.ctl.ChangeConfiguration(ctx, "", "HeartbeatInterval", "60"); return err },
		"limit": func() error {
			_, err := r.ctl.SetLimit(ctx, control.LimitParams{KW: &kw, Purpose: ocpp.PurposeStationMax})
			return err
		},
		"clear":    func() error { _, err := r.ctl.ClearProfiles(ctx, control.ClearParams{}); return err },
		"schedule": func() error { _, err := r.ctl.Schedule(ctx, "", "", time.Hour, nil); return err },
	}
	for name, call := range cases {
		err := call()
		if !errors.Is(err, control.ErrChargerRequired) {
			t.Errorf("%s: err = %v, want ErrChargerRequired", name, err)
			continue
		}
		if !strings.Contains(err.Error(), cpA) || !strings.Contains(err.Error(), cpB) {
			t.Errorf("%s: %q should list both charger ids", name, err)
		}
	}
	for id, st := range r.fleet {
		if len(st.calls) != 0 {
			t.Errorf("charger %s was sent %v by refused commands", id, st.calls)
		}
	}
}

func TestStationCommandsWithACharger(t *testing.T) {
	t.Parallel()
	r := newMultiRig(t)
	ctx := context.Background()

	if _, err := r.ctl.Probe(ctx, cpB); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ctl.ChangeConfiguration(ctx, cpA, "HeartbeatInterval", "60"); err != nil {
		t.Fatal(err)
	}
	kw := 7.0
	if _, err := r.ctl.SetLimit(ctx, control.LimitParams{KW: &kw, Purpose: ocpp.PurposeStationMax, Charger: cpB}); err != nil {
		t.Fatal(err)
	}
	if got := r.fleet[cpA].calls; !slices.Equal(got, []string{"config HeartbeatInterval=60"}) {
		t.Errorf("charger A calls = %v, want only the config change", got)
	}
	if got := len(r.fleet[cpB].profiles); got != 1 || slices.Contains(r.fleet[cpB].calls, "config HeartbeatInterval=60") {
		t.Errorf("charger B profiles = %d, calls = %v, want the station limit and no config change", got, r.fleet[cpB].calls)
	}
	snap := r.core.Snapshot()
	if cp, _ := snap.ChargePoint(cpB); len(cp.ChargingProfiles) != 1 {
		t.Errorf("records for B = %+v, want the one limit", cp.ChargingProfiles)
	}
	if cp, _ := snap.ChargePoint(cpA); len(cp.ChargingProfiles) != 0 {
		t.Errorf("records for A = %+v, want none", cp.ChargingProfiles)
	}

	if _, err := r.ctl.Probe(ctx, "NOBODY"); !errors.Is(err, control.ErrUnknownCharger) {
		t.Errorf("unknown charger: err = %v, want ErrUnknownCharger", err)
	}
	// An EVSE and a charger that do not belong together are a mistake.
	if _, err := r.ctl.SetLimit(ctx, control.LimitParams{EVSEUID: "PRUESTEL_000-1", KW: &kw, Charger: cpB}); !errors.Is(err, control.ErrChargerMismatch) {
		t.Errorf("mismatch: err = %v, want ErrChargerMismatch", err)
	}
}

// Reconnecting charger B asks charger B's EVSEs for their status, and no
// EVSE of charger A.
func TestRefreshAsksOnlyTheReconnectedCharger(t *testing.T) {
	t.Parallel()
	r := newMultiRig(t)

	r.core.Connected(cpB)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ctl.RefreshStatusOnConnect(context.Background(), named{r.fleet[cpB], cpB})
	}()
	waitUntil(t, "the boot grace to start", func() bool { return r.clk.Pending() == 1 })
	r.clk.Advance(control.DefaultBootGrace)
	waitClosed(t, "RefreshStatusOnConnect to return", done)

	if got, want := r.fleet[cpB].triggers, []string{"PRUESTEL_001-1", "PRUESTEL_001-2"}; !slices.Equal(got, want) {
		t.Errorf("charger B was asked for %v, want %v", got, want)
	}
	if got := r.fleet[cpA].triggers; len(got) != 0 {
		t.Errorf("charger A was asked for %v after charger B reconnected", got)
	}
}
