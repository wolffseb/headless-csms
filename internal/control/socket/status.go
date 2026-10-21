package socket

import (
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
)

// BuildStatus renders a core snapshot as the status a person or a script
// reads. It is a pure function so the JSON shape can be pinned by a test
// without a running station.
//
// Every timestamp is in UTC; the CLI converts to local time for display.
func BuildStatus(snap core.Snapshot, chargePointID string) Status {
	out := Status{
		ChargePoint: ChargePointStatus{ID: chargePointID},
		EVSEs:       []EVSEStatus{},
	}

	cp, found := snap.ChargePoint(chargePointID)
	if !found {
		return out
	}

	out.ChargePoint = ChargePointStatus{
		ID:            cp.ID,
		Online:        cp.Online,
		LastSeen:      utcOrNil(cp.LastSeen),
		Vendor:        cp.Boot.Vendor,
		Model:         cp.Boot.Model,
		Firmware:      cp.Boot.FirmwareVersion,
		StationStatus: string(cp.StationStatus),
	}

	reservations := make(map[string]core.Reservation, len(cp.Reservations))
	for _, r := range cp.Reservations {
		reservations[r.EVSEUID] = r
	}
	transactions := make(map[string]core.Transaction)
	for _, tx := range cp.Transactions {
		if tx.Active() {
			transactions[tx.EVSEUID] = tx
		}
	}

	for _, e := range cp.EVSEs {
		row := EVSEStatus{UID: e.UID, Status: string(e.Status), ErrorCode: e.ErrorCode}
		if r, ok := reservations[e.UID]; ok {
			row.Reservation = &ReservationStatus{
				ID: r.ID, IDTag: r.IDTag, ExpiresAt: r.ExpiresAt.UTC(), State: string(r.Status),
			}
		}
		if tx, ok := transactions[e.UID]; ok {
			row.Transaction = &TransactionStatus{ID: tx.ID, IDTag: tx.IDTag, StartedAt: tx.StartedAt.UTC()}
		}
		out.EVSEs = append(out.EVSEs, row)
	}
	return out
}

func utcOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
