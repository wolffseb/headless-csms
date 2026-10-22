package locations_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	"github.com/wolffseb/cli-cpms/internal/ocpp/ocppj"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// TestLiveStatusFromTheSimulator is the whole path: the simulator reports a
// status over OCPP, core records it, and the Locations endpoint serves it.
func TestLiveStatusFromTheSimulator(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	svc := h.core
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	server, err := csms.New(csms.Options{
		Bind:     "127.0.0.1:0",
		Core:     svc,
		Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: v16.NewHandler(h.cfg, svc, log)},
		NewChargePoint: func(conn *ocppj.Conn) (ocpp.ChargePoint, error) {
			return v16.NewChargePoint(conn, h.cfg), nil
		},
		Accept:      func(id string) bool { return id == h.cfg.Charger.ID },
		CallTimeout: 5 * time.Second,
		IdleTimeout: h.cfg.Charger.HeartbeatTimeout.Duration(),
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
		ID:         h.cfg.Charger.ID,
		Connectors: len(h.cfg.Location.EVSEs),
		IDTag:      h.cfg.Auth.DefaultIDTag,
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

	loc := h.cfg.Location
	target := loc.EVSEs[0]
	evsePath := "/locations/" + loc.ID + "/" + target.UID
	waitEVSE := func(want core.EVSEStatus) jsonEVSE {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var e jsonEVSE
			h.ok(evsePath, &e)
			if e.Status == string(want) {
				return e
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s stayed %s, want %s", target.UID, e.Status, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	locationUpdated := func() string {
		t.Helper()
		var l jsonLocation
		h.ok("/locations/"+loc.ID, &l)
		return l.LastUpdated
	}

	before := waitEVSE(core.StatusAvailable)
	locBefore := locationUpdated()

	// Core's clock moves a minute; the station reports charging.
	h.clock.Advance(time.Minute)
	if err := sim.SetConnectorStatus(ctx, target.OCPPConnectorID, "Charging"); err != nil {
		t.Fatal(err)
	}
	after := waitEVSE(core.StatusCharging)

	// RFC 3339 UTC strings of one format compare in time order.
	if after.LastUpdated <= before.LastUpdated {
		t.Errorf("EVSE last_updated %s did not move past %s", after.LastUpdated, before.LastUpdated)
	}
	if got := locationUpdated(); got <= locBefore || got != after.LastUpdated {
		t.Errorf("Location last_updated = %s, want past %s and equal to the EVSE's %s", got, locBefore, after.LastUpdated)
	}

	// The station drops off: every EVSE reads UNKNOWN.
	sim.Close()
	sim.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var l jsonLocation
		h.ok("/locations/"+loc.ID, &l)
		unknown := 0
		for _, e := range l.EVSEs {
			if e.Status == string(core.StatusUnknown) {
				unknown++
			}
		}
		if unknown == len(l.EVSEs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after disconnect: %s", fmt.Sprint(l.EVSEs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
