package commands

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// The prepare functions below are the checks that need no station. Each turns
// a command into a plan, or into the refusal that answers it at once.
//
// With several chargers on the location, "the charge point" is the one that
// owns the EVSE a command is for, and an EVSE the command does not name is
// picked from the whole location.

// location refuses a location that is not ours: OCPI 2003.
func (r *Receiver) location(id string) *refusal {
	if strings.EqualFold(id, r.opts.Config.Location.ID) {
		return nil
	}
	return &refusal{
		response: ResponseRejected,
		code:     ocpi.StatusUnknownLocation,
		message:  fmt.Sprintf("unknown location %q", id),
	}
}

// findEVSE looks an EVSE up by uid, which is a CiString.
func (r *Receiver) findEVSE(uid string) (config.EVSE, *refusal) {
	for _, e := range r.opts.Config.Location.EVSEs {
		if strings.EqualFold(e.UID, uid) {
			return e, nil
		}
	}
	return config.EVSE{}, reject("unknown EVSE %q", uid)
}

// connectorOf refuses a connector the EVSE does not have.
func connectorOf(e config.EVSE, connectorID string) *refusal {
	for _, c := range e.Connectors {
		if strings.EqualFold(c.ID, connectorID) {
			return nil
		}
	}
	return reject("unknown connector %q on EVSE %s", connectorID, e.UID)
}

// online refuses a command for a charger that is not connected: it could not
// be sent, and a refusal now is worth more to the partner than a timeout
// later.
func (r *Receiver) online(e config.EVSE, snap core.Snapshot) *refusal {
	id := r.opts.Config.ChargerOf(e)
	if cp, ok := snap.ChargePoint(id); ok && cp.Online {
		return nil
	}
	return reject("charge point offline (%s)", id)
}

// checkTag refuses a token whose uid cannot be an OCPP 1.6 idTag, which is
// CiString20Type: printable ASCII, 20 characters at most. The station would
// refuse the command with a CALLERROR, long after we said ACCEPTED.
func checkTag(uid string) *refusal {
	if utf8.RuneCountInString(uid) > maxIDTagLength {
		return reject("token.uid %q is longer than %d characters, the limit of an OCPP 1.6 idTag", uid, maxIDTagLength)
	}
	for _, c := range uid {
		if c < 0x20 || c > 0x7e {
			return reject("token.uid %q has characters an OCPP 1.6 idTag cannot carry (printable ASCII only)", uid)
		}
	}
	return nil
}

// firstAvailable picks the first configured EVSE, across every charger, that
// reads AVAILABLE. core reads UNKNOWN for an offline charger's EVSEs, so
// AVAILABLE also means connected. A reservation wants one that is reservable
// and holds no reservation yet; a core record can run ahead of the status the
// station reports.
func (r *Receiver) firstAvailable(snap core.Snapshot, forReservation bool) (config.EVSE, bool) {
	reserved := map[string]bool{}
	status := map[string]core.EVSEStatus{}
	for _, cp := range snap.ChargePoints {
		for _, res := range cp.Reservations {
			reserved[res.EVSEUID] = true
		}
		for _, e := range cp.EVSEs {
			status[e.UID] = e.Status
		}
	}
	for _, e := range r.opts.Config.Location.EVSEs {
		if status[e.UID] != core.StatusAvailable {
			continue
		}
		if forReservation && (!e.IsReservable() || reserved[e.UID]) {
			continue
		}
		return e, true
	}
	return config.EVSE{}, false
}

// reservedOther reports whether the EVSE holds a reservation other than the one
// named, which may be empty.
func reservedOther(snap core.Snapshot, evseUID, ocpiID string) bool {
	for _, cp := range snap.ChargePoints {
		for _, res := range cp.Reservations {
			if res.EVSEUID == evseUID && !strings.EqualFold(res.OCPIReservationID, ocpiID) {
				return true
			}
		}
	}
	return false
}

func (r *Receiver) reserveNow(b *body) (*plan, *refusal) {
	if ref := r.location(b.LocationID); ref != nil {
		return nil, ref
	}
	if ref := checkTag(b.Token.UID); ref != nil {
		return nil, ref
	}
	expiry, _ := ocpi.ParseDateTime(b.ExpiryDate) // problem() read it
	if now := r.now(); expiry.Before(now.Add(MinReservationLead)) {
		return nil, reject("expiry_date %s must be at least %v from now", b.ExpiryDate, MinReservationLead)
	}

	snap := r.opts.Core.Snapshot()
	var (
		e   config.EVSE
		ref *refusal
	)
	held, replacing := r.opts.Core.ReservationByOCPIID(b.ReservationID)
	switch {
	case replacing:
		// A known reservation_id replaces that reservation, on the EVSE that
		// holds it: the station is not asked to move one.
		if e, ref = r.findEVSE(held.EVSEUID); ref != nil {
			return nil, ref
		}
		if b.EVSEUID != "" && !strings.EqualFold(b.EVSEUID, e.UID) {
			return nil, reject("reservation %q is held on EVSE %s; cancel it before reserving another EVSE", b.ReservationID, e.UID)
		}
	case b.EVSEUID != "":
		if e, ref = r.findEVSE(b.EVSEUID); ref != nil {
			return nil, ref
		}
	default:
		var ok bool
		if e, ok = r.firstAvailable(snap, true); !ok {
			return nil, reject("no EVSE at location %s is available to reserve", r.opts.Config.Location.ID)
		}
	}
	if !e.IsReservable() {
		return nil, &refusal{
			response: ResponseNotSupported, code: ocpi.StatusSuccess,
			message: fmt.Sprintf("EVSE %s is not reservable", e.UID),
		}
	}
	if ref := r.online(e, snap); ref != nil {
		return nil, ref
	}
	if reservedOther(snap, e.UID, b.ReservationID) {
		return nil, reject("EVSE %s is already reserved", e.UID)
	}

	charger := r.opts.Config.ChargerOf(e)
	return &plan{charger: charger, run: func(ctx context.Context) error {
		_, err := r.opts.Control.Reserve(ctx, control.ReserveParams{
			EVSEUID:           e.UID,
			IDTag:             b.Token.UID,
			ExpiresAt:         expiry,
			Source:            "ocpi",
			OCPIReservationID: b.ReservationID,
			ResponseURL:       b.ResponseURL,
		})
		return err
	}}, nil
}

func (r *Receiver) cancelReservation(b *body) (*plan, *refusal) {
	held, ok := r.opts.Core.ReservationByOCPIID(b.ReservationID)
	if !ok {
		// ACCEPTED, then UNKNOWN_RESERVATION as the result: the partner asked
		// the CPO to pass the cancel on, and the answer to "do you know it"
		// is the station-side outcome.
		return &plan{run: func(context.Context) error {
			return fmt.Errorf("reservation %q: %w", b.ReservationID, control.ErrNoReservation)
		}}, nil
	}
	e, ref := r.findEVSE(held.EVSEUID)
	if ref != nil {
		return nil, ref
	}
	if ref := r.online(e, r.opts.Core.Snapshot()); ref != nil {
		return nil, ref
	}
	// A reservation whose ReserveNow is still out is cancelled once the
	// station has answered it, so the cancel can take two calls.
	calls := 1
	if held.Status != core.ReservationActive {
		calls = 2
	}
	return &plan{charger: r.opts.Config.ChargerOf(e), calls: calls, run: func(ctx context.Context) error {
		return r.opts.Control.CancelForPartner(ctx, e.UID, held.ID)
	}}, nil
}

func (r *Receiver) startSession(b *body) (*plan, *refusal) {
	if ref := r.location(b.LocationID); ref != nil {
		return nil, ref
	}
	if ref := checkTag(b.Token.UID); ref != nil {
		return nil, ref
	}

	snap := r.opts.Core.Snapshot()
	var e config.EVSE
	switch {
	case b.EVSEUID != "":
		var ref *refusal
		if e, ref = r.findEVSE(b.EVSEUID); ref != nil {
			return nil, ref
		}
		if b.ConnectorID != "" {
			if ref := connectorOf(e, b.ConnectorID); ref != nil {
				return nil, ref
			}
		}
	case b.ConnectorID != "":
		return nil, reject("connector_id %q needs an evse_uid", b.ConnectorID)
	default:
		var ok bool
		if e, ok = r.firstAvailable(snap, false); !ok {
			return nil, reject("no EVSE at location %s is available to start a session on", r.opts.Config.Location.ID)
		}
	}
	if ref := r.online(e, snap); ref != nil {
		return nil, ref
	}

	return &plan{charger: r.opts.Config.ChargerOf(e), run: func(ctx context.Context) error {
		// The station will ask whether it may start for this tag, and the
		// spec has the CPO trust the eMSP to send valid tokens.
		r.opts.Core.AuthoriseTag(b.Token.UID, core.TagGrantWindow)
		return r.opts.Control.Start(ctx, e.UID, b.Token.UID)
	}}, nil
}

// stopSession reads session_id as our transaction id, the one `cpms status`
// shows: there is no Sessions module, so that is the only session id cpms has.
func (r *Receiver) stopSession(b *body) (*plan, *refusal) {
	snap := r.opts.Core.Snapshot()
	for _, cp := range snap.ChargePoints {
		for _, tx := range cp.Transactions {
			if !tx.Active() || !strings.EqualFold(tx.ID, b.SessionID) {
				continue
			}
			e, ok := r.opts.Config.EVSEByUID(tx.EVSEUID)
			if !ok {
				return nil, reject("session %q is on %s, which is not a configured EVSE", b.SessionID, tx.EVSEUID)
			}
			if !cp.Online {
				return nil, reject("charge point offline (%s)", cp.ID)
			}
			return &plan{charger: r.opts.Config.ChargerOf(e), run: func(ctx context.Context) error {
				return r.opts.Control.Stop(ctx, tx.EVSEUID, tx.ID)
			}}, nil
		}
	}
	return nil, &refusal{
		response: ResponseUnknownSession, code: ocpi.StatusSuccess,
		message: fmt.Sprintf("unknown session %q", b.SessionID),
	}
}

func (r *Receiver) unlockConnector(b *body) (*plan, *refusal) {
	if ref := r.location(b.LocationID); ref != nil {
		return nil, ref
	}
	e, ref := r.findEVSE(b.EVSEUID)
	if ref != nil {
		return nil, ref
	}
	if ref := connectorOf(e, b.ConnectorID); ref != nil {
		return nil, ref
	}
	if ref := r.online(e, r.opts.Core.Snapshot()); ref != nil {
		return nil, ref
	}
	return &plan{charger: r.opts.Config.ChargerOf(e), run: func(ctx context.Context) error {
		return r.opts.Control.Unlock(ctx, e.UID)
	}}, nil
}
