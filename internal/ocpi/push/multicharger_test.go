package push_test

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
)

const secondCharger = "ALP-HYC-002"

// twoChargers moves every EVSE but the first onto a second charger, so the
// first EVSE is alone on charger 1 and the rest share charger 2.
func twoChargers(cfg *config.Config) {
	second := cfg.Chargers[0]
	second.ID = secondCharger
	cfg.Chargers = append(cfg.Chargers, second)
	for i := 1; i < len(cfg.Location.EVSEs); i++ {
		cfg.Location.EVSEs[i].Charger = secondCharger
	}
}

// setStatusOn reports a status for EVSE i on whichever charger it is
// configured on.
func (r *rig) setStatusOn(i int, status core.EVSEStatus) {
	e := r.cfg.Location.EVSEs[i]
	r.core.SetConnectorStatus(e.Charger, e.OCPPConnectorID, status, "NoError")
}

// connectBoth brings both chargers online with every EVSE Available.
func (r *rig) connectBoth() {
	r.core.Connected(r.cfg.Chargers[0].ID)
	r.core.Connected(secondCharger)
	for i := range r.cfg.Location.EVSEs {
		r.setStatusOn(i, core.StatusAvailable)
	}
	r.core.flush(r.t)
}

// A status change on the second charger's EVSE is pushed as a PATCH for that
// EVSE, with the status read from that charger's own snapshot.
func TestSecondChargerStatusChangeIsPushed(t *testing.T) {
	t.Parallel()
	r := newRig(t, twoChargers)
	r.register()
	r.connectBoth()
	r.pushRound(r.evses())
	r.emsp.waitPatches(r.evses())

	r.clk.Advance(time.Minute)
	r.setStatusOn(1, core.StatusCharging)
	r.pushRound(1)

	got := r.emsp.waitPatches(r.evses() + 1)
	p := got[r.evses()]
	if want := r.evsePath(1); p.path != want {
		t.Errorf("path = %s, want %s", p.path, want)
	}
	if p.status() != "CHARGING" {
		t.Errorf("status = %q, want CHARGING", p.status())
	}
	if len(got) != r.evses()+1 {
		t.Errorf("%d PATCHes, want %d", len(got), r.evses()+1)
	}
}

// Changes on both chargers in quick succession both get pushed, each with the
// status of its own EVSE.
func TestBothChargersChangesArePushed(t *testing.T) {
	t.Parallel()
	r := newRig(t, twoChargers)
	r.register()
	r.connectBoth()
	r.pushRound(r.evses())
	r.emsp.waitPatches(r.evses())

	r.setStatusOn(0, core.StatusCharging)
	r.setStatusOn(1, core.StatusBlocked)
	r.pushRound(2)

	got := r.emsp.waitPatches(r.evses() + 2)[r.evses():]
	byPath := map[string]string{}
	for _, p := range got {
		byPath[p.path] = p.status()
	}
	if len(got) != 2 || byPath[r.evsePath(0)] != "CHARGING" || byPath[r.evsePath(1)] != "BLOCKED" {
		t.Errorf("PATCHes = %v, want %s CHARGING and %s BLOCKED", byPath, r.evsePath(0), r.evsePath(1))
	}
}

// One charger going offline pushes only its own EVSEs.
func TestOneChargerOfflinePushesOnlyItsEVSEs(t *testing.T) {
	t.Parallel()
	r := newRig(t, twoChargers)
	r.register()
	r.connectBoth()
	r.pushRound(r.evses())
	r.emsp.waitPatches(r.evses())

	r.core.Disconnected(secondCharger, "test")
	r.pushRound(r.evses() - 1)
	got := r.emsp.waitPatches(2*r.evses() - 1)
	for _, p := range got[r.evses():] {
		if p.path == r.evsePath(0) {
			t.Errorf("the EVSE of the charger that stayed online was pushed: %s", p.path)
		}
	}
	if len(got) != 2*r.evses()-1 {
		t.Errorf("%d PATCHes, want %d", len(got), 2*r.evses()-1)
	}
}
