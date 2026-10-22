package socket

import (
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi/credentials"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/state"
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
	if d := cp.LastDisconnect; !d.At.IsZero() {
		out.ChargePoint.LastDisconnect = &DisconnectStatus{At: d.At.UTC(), Reason: d.Reason}
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

// BuildOCPIStatus renders the OCPI registration for status, without its
// token. A nil registration is "not registered".
func BuildOCPIStatus(reg *state.OCPIRegistration) OCPIStatus {
	if reg == nil {
		return OCPIStatus{}
	}
	out := OCPIStatus{Registered: true, Party: credentials.Party(reg), Since: utcOrNil(reg.RegisteredAt)}
	if url, ok := credentials.PushEndpoint(reg); ok {
		out.PushEndpoint = url
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

// BuildProbe renders a probe report, with the identity the station gave at
// boot, as the result a person or a script reads. Like BuildStatus it is a
// pure function, so its JSON shape can be pinned without a station.
func BuildProbe(report ocpp.ProbeReport, snap core.Snapshot, chargePointID string) ProbeResult {
	out := ProbeResult{
		ChargePoint:  ProbeChargePoint{ID: chargePointID, OCPPVersion: string(report.Version)},
		Features:     make([]ProbeFeature, 0, len(report.Features)),
		Capabilities: make([]ProbeCapability, 0, len(ocpp.AllCapabilities())),
		Settings:     make([]ProbeSetting, 0, len(report.Settings)),
		Raw:          make([]ProbeRawKey, 0, len(report.Raw)),
		Connectors:   report.Connectors,
	}
	if cp, ok := snap.ChargePoint(chargePointID); ok {
		out.ChargePoint.Vendor = cp.Boot.Vendor
		out.ChargePoint.Model = cp.Boot.Model
		out.ChargePoint.SerialNumber = cp.Boot.SerialNumber
		out.ChargePoint.Firmware = cp.Boot.FirmwareVersion
	}

	for _, f := range report.Features {
		grants := make([]string, 0, len(f.Grants))
		for _, g := range f.Grants {
			grants = append(grants, string(g))
		}
		out.Features = append(out.Features, ProbeFeature{Name: f.Name, Support: string(f.Support), Grants: grants, Assumed: f.Assumed})
	}
	for _, c := range ocpp.AllCapabilities() {
		out.Capabilities = append(out.Capabilities, ProbeCapability{Name: string(c), Support: string(report.Support(c))})
	}
	for _, s := range report.Settings {
		out.Settings = append(out.Settings, ProbeSetting{Name: s.Name, Value: s.Value, Reported: s.Reported, Meaning: s.Meaning})
	}
	if d := report.HeartbeatInterval; d != nil {
		secs := int(d.Seconds())
		out.HeartbeatIntervalSeconds = &secs
	}
	for _, k := range report.Raw {
		out.Raw = append(out.Raw, ProbeRawKey{Key: k.Key, Value: k.Value, Readonly: k.Readonly})
	}
	return out
}
