package tui

import (
	"fmt"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
)

// eventText is an event as the log pane words it. core's own line is for
// headless logs; the pane differs where it must agree with the rest of the
// screen: times are local, like every other time on it.
func (m *Model) eventText(e core.Event) string {
	switch e.Kind {
	case core.EventReservationCreated:
		until := e.Detail
		if t, err := time.Parse(time.RFC3339, e.Detail); err == nil {
			until = m.clock(t)
		}
		return fmt.Sprintf("%s reserved for tag %s until %s (reservation %d)",
			e.EVSEUID, e.IDTag, until, e.ReservationID)
	}
	return e.String()
}
