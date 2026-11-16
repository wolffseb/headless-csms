package cli_test

import (
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/simulator"
)

// TestRunAsksAResumedStationForItsStatus is #53 as it happened on an Alpitronic HYC50:
// cpms restarts while the station keeps running, and the station redials
// without a BootNotification and without reporting any status. cpms has
// nothing to go on, so the EVSE reads UNKNOWN until cpms asks, after the
// boot grace, rather than until the station next changes state.
//
// Port 19601 (19600 is TestProbeWithoutSmartCharging's).
func TestRunAsksAResumedStationForItsStatus(t *testing.T) {
	t.Parallel()

	s := newStation(t, 19601)
	s.connect(func(o *simulator.Options) {
		o.Reconnect = true
		o.ResumeWithoutBoot = true
		o.ReconnectBackoff = func(int) time.Duration { return 50 * time.Millisecond }
	})

	s.stopRun()
	s.startRun()

	var seenOnline bool
	deadline := time.Now().Add(control.DefaultBootGrace + 10*time.Second)
	for {
		st := s.status()
		if st.ChargePoint.Online && len(st.EVSEs) == 1 {
			// Until cpms asks, the restarted cpms knows nothing.
			if !seenOnline && st.EVSEs[0].Status != "UNKNOWN" {
				t.Fatalf("EVSE-1 = %s as soon as the station was back; it was not meant to report by itself",
					st.EVSEs[0].Status)
			}
			seenOnline = true
			if st.EVSEs[0].Status == "AVAILABLE" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("EVSE-1 never left UNKNOWN after the station resumed; last status: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
