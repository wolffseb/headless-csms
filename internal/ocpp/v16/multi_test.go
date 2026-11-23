package v16_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
	"github.com/wolffseb/cli-cpms/internal/ocpp/csms"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

// multiConfig is the coneva bench: two stations on one listener, one tag
// each, both accepted everywhere, and a different heartbeat interval per
// station so a mixed-up lookup shows.
func multiConfig() *config.Config {
	return &config.Config{
		Chargers: []config.Charger{
			{ID: "PRUESTEL_000", HeartbeatInterval: config.NewDuration(60 * time.Second)},
			{ID: "PRUESTEL_001", HeartbeatInterval: config.NewDuration(45 * time.Second)},
		},
		Auth: config.Auth{DefaultIDTag: "RFID_PRUESTEL_000", IDTags: []string{"RFID_PRUESTEL_001"}},
		Location: config.Location{
			EVSEs: []config.EVSE{
				{UID: "PRUESTEL_000-1", Charger: "PRUESTEL_000", OCPPConnectorID: 1},
				{UID: "PRUESTEL_001-1", Charger: "PRUESTEL_001", OCPPConnectorID: 1},
			},
		},
	}
}

type multiStack struct {
	core   *core.Service
	server *csms.Server
}

func newMultiStack(t *testing.T) *multiStack {
	t.Helper()

	cfg := multiConfig()
	svc := core.New(cfg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := v16.NewHandler(cfg, svc, log, v16.WithClock(func() time.Time { return testNow }))
	server, err := csms.New(csms.Options{
		Bind:     "127.0.0.1:0",
		Core:     svc,
		Handlers: map[ocpp.Version]ocpp.Handler{ocpp.Version16: handler},
		Accept:   func(id string) bool { _, ok := cfg.ChargerByID(id); return ok },
		Log:      log,
	})
	if err != nil {
		t.Fatalf("csms.New: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return &multiStack{core: svc, server: server}
}

func (m *multiStack) dial(t *testing.T, id string) *ocpptest.Client {
	t.Helper()
	return ocpptest.MustDial(t, "ws://"+m.server.Addr()+"/ocpp/"+id)
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

func TestMultiChargerAcceptsEveryConfiguredIDAndRejectsOthers(t *testing.T) {
	t.Parallel()
	m := newMultiStack(t)

	for _, id := range []string{"PRUESTEL_000", "PRUESTEL_001"} {
		m.dial(t, id)
	}

	_, hs, err := ocpptest.Dial(t, "ws://"+m.server.Addr()+"/ocpp/PRUESTEL_002", "ocpp1.6")
	if err == nil {
		t.Fatal("unconfigured charger id was accepted")
	}
	if hs.StatusCode == 101 {
		t.Errorf("status = %d, want a rejection", hs.StatusCode)
	}
}

func TestMultiChargerBootIntervalComesFromTheBootingCharger(t *testing.T) {
	t.Parallel()
	m := newMultiStack(t)

	for id, want := range map[string]int{"PRUESTEL_000": 60, "PRUESTEL_001": 45} {
		raw := m.dial(t, id).MustCall(v16.ActionBootNotification, v16.BootNotificationReq{
			ChargePointVendor: "coneva", ChargePointModel: "bench",
		})
		if got := decode[v16.BootNotificationConf](t, raw).Interval; got != want {
			t.Errorf("%s: interval = %d, want %d", id, got, want)
		}
	}
}

func TestMultiChargerTagsAreAcceptedOnEveryCharger(t *testing.T) {
	t.Parallel()
	m := newMultiStack(t)

	tags := []struct {
		tag  string
		want string
	}{
		{"RFID_PRUESTEL_000", v16.AuthAccepted},
		{"RFID_PRUESTEL_001", v16.AuthAccepted},
		{"rfid_pruestel_001", v16.AuthAccepted}, // idTags are case-insensitive
		{"RFID_PRUESTEL_002", v16.AuthInvalid},
	}
	for _, id := range []string{"PRUESTEL_000", "PRUESTEL_001"} {
		c := m.dial(t, id)
		for _, tc := range tags {
			auth := decode[v16.AuthorizeConf](t, c.MustCall(v16.ActionAuthorize, v16.AuthorizeReq{IDTag: tc.tag}))
			if auth.IDTagInfo.Status != tc.want {
				t.Errorf("%s Authorize %s = %s, want %s", id, tc.tag, auth.IDTagInfo.Status, tc.want)
			}
			start := decode[v16.StartTransactionConf](t, c.MustCall(v16.ActionStartTransaction, v16.StartTransactionReq{
				ConnectorID: 1, IDTag: tc.tag, MeterStart: 1, Timestamp: testNow.Format(time.RFC3339),
			}))
			if start.IDTagInfo.Status != tc.want {
				t.Errorf("%s StartTransaction %s = %s, want %s", id, tc.tag, start.IDTagInfo.Status, tc.want)
			}
		}
	}
}

func TestMultiChargerStatusLandsOnTheSecondChargersEVSE(t *testing.T) {
	t.Parallel()
	m := newMultiStack(t)

	c := m.dial(t, "PRUESTEL_001")
	c.MustCall(v16.ActionStatusNotification, v16.StatusNotificationReq{
		ConnectorID: 1, ErrorCode: "NoError", Status: v16.StatusCharging,
	})
	c.MustCall(v16.ActionMeterValues, v16.MeterValuesReq{
		ConnectorID: 1,
		MeterValue: []v16.MeterValue{{
			Timestamp:    testNow.Format(time.RFC3339),
			SampledValue: []v16.SampledValue{{Value: "7400", Measurand: "Power.Active.Import", Unit: "W"}},
		}},
	})

	snap := m.core.Snapshot()
	cp, ok := snap.ChargePoint("PRUESTEL_001")
	if !ok || len(cp.EVSEs) != 1 {
		t.Fatalf("PRUESTEL_001 snapshot = %+v, %v; want its one configured EVSE", cp, ok)
	}
	if e := cp.EVSEs[0]; e.UID != "PRUESTEL_001-1" || e.Status != core.StatusCharging {
		t.Errorf("EVSE = %s %s, want PRUESTEL_001-1 CHARGING", e.UID, e.Status)
	}
	if st, _ := m.core.EVSEStatus("PRUESTEL_000-1"); st == core.StatusCharging {
		t.Error("charger 1's status leaked onto charger 0's EVSE")
	}
	if _, ok := m.core.EVSEStatus("PRUESTEL_001-1"); !ok {
		t.Error("PRUESTEL_001-1 unknown")
	}
	if _, ok := m.core.EVSEStatus("PRUESTEL_001-1-1"); ok {
		t.Error("a synthetic EVSE uid was invented")
	}
}
