package control_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/clocktest"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// behindProxy puts the simulator behind a proxy the test can cut, and has it
// redial one second after a drop, on rclk.
func behindProxy(t *testing.T, p **ocpptest.Proxy, rclk *clocktest.Clock) func(*simulator.Options) {
	return func(o *simulator.Options) {
		*p = ocpptest.NewProxy(t, strings.TrimPrefix(o.URL, "ws://"))
		o.URL = (*p).URL()
		o.Reconnect = true
		o.ReconnectBackoff = func(int) time.Duration { return time.Second }
		o.ReconnectAfterFunc = func(d time.Duration, fn func()) simulator.Stopper { return rclk.AfterFunc(d, fn) }
	}
}

// offlineSession cuts the station off, runs a session on connector 1 while
// it is offline, and brings it back, waiting until core has the session
// closed.
func offlineSession(t *testing.T, r *simRig, p *ocpptest.Proxy, rclk *clocktest.Clock, beforeRestore func()) core.Transaction {
	t.Helper()
	ctx := context.Background()

	r.waitFor(t, "the redial to be scheduled", func() bool { return rclk.Pending() == 1 })
	if err := r.sim.StartTransaction(ctx, 1); err != nil {
		t.Fatalf("offline start: %v", err)
	}
	if err := r.sim.StopTransaction(ctx, 1); err != nil {
		t.Fatalf("offline stop: %v", err)
	}
	if beforeRestore != nil {
		beforeRestore()
	}
	p.Restore()
	rclk.Advance(time.Second)

	r.waitFor(t, "core to record the offline session", func() bool {
		snap, _ := r.core.Snapshot().ChargePoint(testCP)
		return len(snap.Transactions) == 1 && !snap.Transactions[0].Active()
	})
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	return snap.Transactions[0]
}

// TestReservationRedeemedOffline reserves, loses the station, and has the
// reservation redeemed while it is offline: core keeps the reservation
// active through the outage, and the replayed StartTransaction, which names
// it, consumes it.
func TestReservationRedeemedOffline(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	r := newSimRig(t, 5*time.Second, behindProxy(t, &p, rclk))

	res, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1", Source: "cli"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	r.waitFor(t, "the simulator to hold the reservation", func() bool { return r.sim.ReservationID(1) == res.ID })

	events, unsubscribe := r.core.Subscribe("test")
	defer unsubscribe()

	p.Cut()
	tx := offlineSession(t, r, p, rclk, func() {
		if _, held := r.core.ActiveReservation("EVSE-1"); !held {
			t.Error("core dropped the reservation while the station was offline")
		}
	})

	if tx.ReservationID != res.ID {
		t.Errorf("offline session redeemed reservation %d, want %d", tx.ReservationID, res.ID)
	}
	if _, held := r.core.ActiveReservation("EVSE-1"); held {
		t.Error("the reservation survived the session that redeemed it")
	}
	ended := ""
	for len(events) > 0 {
		if e := <-events; e.Kind == core.EventReservationEnded && e.ReservationID == res.ID {
			ended = e.Detail
		}
	}
	if ended != core.ReservationConsumed {
		t.Errorf("reservation ended as %q, want %q", ended, core.ReservationConsumed)
	}
}

// TestOrphanedReservationRedeemedOffline is the #33 case meeting an outage.
// ReserveNow reaches a slow station but its answer does not come back in
// time, so core marks the reservation cancelling and sends a cancel; the
// connection drops before the cancel gets through, so core ends it. The
// station still took the reservation, and redeems it offline. The replayed
// StartTransaction names a reservation core has already ended: core must
// record the session regardless and leave no reservation behind.
func TestOrphanedReservationRedeemedOffline(t *testing.T) {
	t.Parallel()

	var p *ocpptest.Proxy
	rclk := clocktest.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	r := newSimRig(t, 300*time.Millisecond, behindProxy(t, &p, rclk), func(o *simulator.Options) {
		o.Scenario = simulator.ScenarioSlow
		o.SlowDelay = 600 * time.Millisecond
	})

	reserved := make(chan error, 1)
	go func() {
		_, err := r.ctl.Reserve(context.Background(), control.ReserveParams{EVSEUID: "EVSE-1", Source: "cli"})
		reserved <- err
	}()
	// Let ReserveNow through, and nothing after it: the cancel must not
	// reach the station.
	r.waitFor(t, "ReserveNow to reach the station", func() bool { return p.Saw(`"ReserveNow"`) })
	p.Hold()

	select {
	case err := <-reserved:
		if !errors.Is(err, ocpp.ErrTimeout) {
			t.Fatalf("Reserve = %v, want ErrTimeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Reserve did not time out")
	}
	snap, _ := r.core.Snapshot().ChargePoint(testCP)
	if len(snap.Reservations) != 1 || snap.Reservations[0].Status != core.ReservationCancelling {
		t.Fatalf("reservations after the timeout = %+v, want one cancelling", snap.Reservations)
	}
	id := snap.Reservations[0].ID

	p.Cut()
	r.waitFor(t, "core to give up the cancel", func() bool {
		snap, _ := r.core.Snapshot().ChargePoint(testCP)
		return len(snap.Reservations) == 0
	})
	r.waitFor(t, "the slow station to take the reservation after all", func() bool { return r.sim.ReservationID(1) == id })

	tx := offlineSession(t, r, p, rclk, nil)
	if tx.ReservationID != id {
		t.Errorf("offline session records reservation %d, want %d, the one the station said it redeemed", tx.ReservationID, id)
	}
	snap, _ = r.core.Snapshot().ChargePoint(testCP)
	if len(snap.Reservations) != 0 {
		t.Errorf("reservations after the replay = %+v, want none", snap.Reservations)
	}
	r.waitFor(t, "EVSE-1 to be available again", func() bool { return r.status("EVSE-1") == core.StatusAvailable })
}
