package v16_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
	"github.com/wolffseb/cli-cpms/internal/statetest"
)

// Core holds transaction ids as strings so OCPP 2.0.1 fits. On the 1.6 wire
// they must stay JSON numbers: a station that gets "1" where the schema says
// integer will reject the answer, or worse, misparse it.
func TestStartTransactionConfCarriesANumericTransactionID(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)
	svc.Connected(testCP)

	conf := call[map[string]json.RawMessage](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: testTag, MeterStart: 0, Timestamp: "2026-08-14T12:00:00Z",
	})
	raw := string(conf["transactionId"])
	if raw == "" || strings.HasPrefix(raw, `"`) {
		t.Fatalf("transactionId on the wire = %s, want a JSON number", raw)
	}

	// And the stop that quotes it back as a number finds it.
	var id int
	if err := json.Unmarshal(conf["transactionId"], &id); err != nil {
		t.Fatal(err)
	}
	call[v16.StopTransactionConf](t, h, v16.ActionStopTransaction, v16.StopTransactionReq{
		TransactionID: id, MeterStop: 10,
	})
	snap, _ := svc.Snapshot().ChargePoint(testCP)
	if snap.Transactions[0].Active() {
		t.Error("StopTransaction with the numeric id did not close the transaction")
	}
}

func TestStartTransactionPassesTheReservationIDToCore(t *testing.T) {
	t.Parallel()

	h, svc := newHandler(t)
	svc.Connected(testCP)
	pending, err := svc.BeginReservation(core.Reservation{
		ChargePointID: testCP, EVSEUID: "EVSE-1", IDTag: "RESERVED-FOR", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.ActivateReservation(pending.ID)

	// A different tag, so only the reservationId can tie the two together.
	reservationID := pending.ID
	call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: testTag, Timestamp: "2026-08-14T12:00:00Z", ReservationID: &reservationID,
	})

	snap, _ := svc.Snapshot().ChargePoint(testCP)
	if len(snap.Reservations) != 0 {
		t.Errorf("reservation %d was not consumed: %+v", pending.ID, snap.Reservations)
	}
	if got := snap.Transactions[0].ReservationID; got != pending.ID {
		t.Errorf("transaction redeems reservation %d, want %d", got, pending.ID)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A disk error must never make us refuse the station: it would retry the
// StartTransaction forever.
func TestStartTransactionIsAcceptedWhenStateCannotBeWritten(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	store := statetest.New(t)
	store.Fail(true)
	cfg := testConfig()
	svc := core.New(cfg, core.WithStore(store), core.WithLogger(logger))
	h := v16.NewHandler(cfg, svc, logger)
	svc.Connected(testCP)

	conf := call[v16.StartTransactionConf](t, h, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: 1, IDTag: testTag, Timestamp: "2026-08-14T12:00:00Z",
	})
	if conf.IDTagInfo.Status != v16.AuthAccepted || conf.TransactionID < 1 {
		t.Fatalf("start = %+v, want an accepted transaction", conf)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "could not persist") {
		t.Errorf("the failed write was not logged as an error; logs:\n%s", logs.String())
	}
}
