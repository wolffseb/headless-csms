package commands_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

func settle() { time.Sleep(150 * time.Millisecond) }

// RESERVE_NOW is answered at once, with the station still holding the
// ReserveNow unanswered, and the CommandResult follows once it has answered.
// The timeout the partner is told is the longest the command can take (a turn
// in the queue and then the answer: twice call_timeout) plus five seconds, and
// is the call timeout of the charger the EVSE is on.
func TestReserveNowIsAnsweredBeforeTheStationAnswers(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	release := s.station(cpA).holdOp("reserve")
	defer release()

	got := s.post("RESERVE_NOW", s.reserveBody("r1", nil))
	if got.http != 200 || got.code != 1000 || got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v, want HTTP 200, 1000, ACCEPTED", got)
	}
	if got.timeout != 2*7+5 {
		t.Errorf("timeout = %d, want 19 (twice charger A's 7s, plus 5)", got.timeout)
	}
	select {
	case op := <-s.station(cpA).entered:
		if op != "reserve" {
			t.Fatalf("the station got %q first", op)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the station never got the ReserveNow")
	}
	if n := len(s.emsp.got("r1")); n != 0 {
		t.Fatalf("the partner has %d results while the station has not answered", n)
	}
	if _, ok := s.core.ActiveReservation("EVSE-A1"); ok {
		t.Fatal("a reservation is active before the station accepted it")
	}

	release()
	if res, msg := s.result("r1"); res != "ACCEPTED" || msg != "" {
		t.Fatalf("result = %s %q, want ACCEPTED", res, msg)
	}
	held, ok := s.core.ActiveReservation("EVSE-A1")
	if !ok || held.IDTag != tag || held.OCPIReservationID != "RES-1" || held.Source != "ocpi" ||
		held.ResponseURL != s.emsp.url("r1") {
		t.Fatalf("reservation = %+v, want %s for %s from OCPI with its response_url", held, "RES-1", tag)
	}
	sent := s.station(cpA).reserved()
	if len(sent) != 1 || sent[0].IDTag != tag || sent[0].EVSEUID != "EVSE-A1" || sent[0].ReservationID != held.ID ||
		!sent[0].ExpiresAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("the station was sent %+v", sent)
	}
	// The result is posted with our TOKEN_B.
	if auth, want := s.emsp.got("r1")[0].auth, "Token "+base64.StdEncoding.EncodeToString([]byte(tokenB)); auth != want {
		t.Errorf("authorization = %q, want %q (TOKEN_B)", auth, want)
	}
}

// The timeout is the owning charger's: B's call_timeout is shorter than A's.
func TestTheTimeoutIsThatOfTheChargerOwningTheEVSE(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	got := s.post("RESERVE_NOW", s.reserveBody("r1", map[string]any{"evse_uid": "EVSE-B1"}))
	if got.result != "ACCEPTED" || got.timeout != 2*3+5 {
		t.Fatalf("answer = %+v, want ACCEPTED with timeout 11", got)
	}
	s.result("r1")
	if s.station(cpA).total() != 0 || len(s.station(cpB).reserved()) != 1 {
		t.Errorf("the command reached station A (%d) or missed B", s.station(cpA).total())
	}
}

// Everything that needs no station is answered at once, and nothing reaches a
// station or the partner.
func TestSynchronousAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		body    func(s *stage) any
		setup   func(s *stage)
		http    int
		code    int
		result  string
		mention string
	}{
		{name: "unknown location", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"location_id": "NOPE"}) },
			http: 200, code: 2003, result: "REJECTED", mention: "unknown location"},
		{name: "unknown location on an unlock", command: "UNLOCK_CONNECTOR",
			body: func(s *stage) any { return s.unlockBody("x", map[string]any{"location_id": "other"}) },
			http: 200, code: 2003, result: "REJECTED"},
		{name: "unknown EVSE", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"evse_uid": "EVSE-Z"}) },
			http: 200, code: 1000, result: "REJECTED", mention: "unknown EVSE"},
		{name: "unknown connector", command: "UNLOCK_CONNECTOR",
			body: func(s *stage) any { return s.unlockBody("x", map[string]any{"connector_id": "9"}) },
			http: 200, code: 1000, result: "REJECTED", mention: "unknown connector"},
		{name: "start with a connector and no EVSE", command: "START_SESSION",
			body: func(s *stage) any { return s.startBody("x", map[string]any{"evse_uid": nil, "connector_id": "1"}) },
			http: 200, code: 1000, result: "REJECTED", mention: "needs an evse_uid"},
		{name: "charge point offline", command: "RESERVE_NOW",
			body:  func(s *stage) any { return s.reserveBody("x", nil) },
			setup: func(s *stage) { s.core.Disconnected(cpA, "test") },
			http:  200, code: 1000, result: "REJECTED", mention: "charge point offline"},
		{name: "the other charger being offline", command: "UNLOCK_CONNECTOR",
			body:  func(s *stage) any { return s.unlockBody("x", map[string]any{"evse_uid": "EVSE-B1"}) },
			setup: func(s *stage) { s.core.Disconnected(cpB, "test") },
			http:  200, code: 1000, result: "REJECTED", mention: "charge point offline (CP-B)"},
		{name: "token uid of 21 characters", command: "RESERVE_NOW",
			body: func(s *stage) any {
				return s.reserveBody("x", map[string]any{"token": map[string]any{"uid": "123456789012345678901"}})
			},
			http: 200, code: 1000, result: "REJECTED", mention: "longer than 20"},
		{name: "token uid that is not an idTag", command: "START_SESSION",
			body: func(s *stage) any {
				return s.startBody("x", map[string]any{"token": map[string]any{"uid": "Straße"}})
			},
			http: 200, code: 1000, result: "REJECTED", mention: "printable ASCII"},
		{name: "expiry in the past", command: "RESERVE_NOW",
			body: func(s *stage) any {
				return s.reserveBody("x", map[string]any{"expiry_date": ocpi.FormatDateTime(t0.Add(-time.Hour))})
			},
			http: 200, code: 1000, result: "REJECTED", mention: "at least"},
		{name: "expiry in under a minute", command: "RESERVE_NOW",
			body: func(s *stage) any {
				return s.reserveBody("x", map[string]any{"expiry_date": ocpi.FormatDateTime(t0.Add(59 * time.Second))})
			},
			http: 200, code: 1000, result: "REJECTED", mention: "at least"},
		{name: "EVSE that is not reservable", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"evse_uid": "EVSE-A3"}) },
			http: 200, code: 1000, result: "NOT_SUPPORTED", mention: "not reservable"},
		{name: "nothing available to reserve", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"evse_uid": nil}) },
			setup: func(s *stage) {
				for _, e := range s.cfg.Location.EVSEs {
					s.setStatus(e.UID, core.StatusCharging)
				}
			},
			http: 200, code: 1000, result: "REJECTED", mention: "no EVSE"},
		{name: "nothing available to start on", command: "START_SESSION",
			body: func(s *stage) any { return s.startBody("x", map[string]any{"evse_uid": nil}) },
			setup: func(s *stage) {
				for _, e := range s.cfg.Location.EVSEs {
					s.setStatus(e.UID, core.StatusCharging)
				}
			},
			http: 200, code: 1000, result: "REJECTED", mention: "no EVSE"},
		{name: "stop of a session we do not know", command: "STOP_SESSION",
			body: func(s *stage) any { return map[string]any{"response_url": s.emsp.url("x"), "session_id": "99"} },
			http: 200, code: 1000, result: "UNKNOWN_SESSION", mention: "unknown session"},
		{name: "unknown command", command: "FOO",
			body: func(s *stage) any { return map[string]any{"response_url": s.emsp.url("x")} },
			http: 200, code: 1000, result: "NOT_SUPPORTED", mention: "FOO"},
		{name: "malformed JSON", command: "RESERVE_NOW",
			body: func(*stage) any { return `{"response_url": ` },
			http: 400, code: 2001},
		{name: "text after the JSON", command: "RESERVE_NOW",
			body: func(*stage) any { return `{} {}` },
			http: 400, code: 2001},
		{name: "no response_url", command: "UNLOCK_CONNECTOR",
			body: func(s *stage) any { return s.unlockBody("x", map[string]any{"response_url": nil}) },
			http: 400, code: 2001},
		{name: "a response_url that is not http", command: "UNLOCK_CONNECTOR",
			body: func(s *stage) any { return s.unlockBody("x", map[string]any{"response_url": "ftp://emsp/x"}) },
			http: 400, code: 2001},
		{name: "no token", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"token": nil}) },
			http: 400, code: 2001},
		{name: "no reservation_id", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"reservation_id": nil}) },
			http: 400, code: 2001},
		{name: "an expiry that is not a date", command: "RESERVE_NOW",
			body: func(s *stage) any { return s.reserveBody("x", map[string]any{"expiry_date": "tomorrow"}) },
			http: 400, code: 2001},
		{name: "no connector_id for an unlock", command: "UNLOCK_CONNECTOR",
			body: func(s *stage) any { return s.unlockBody("x", map[string]any{"connector_id": nil}) },
			http: 400, code: 2001},
		{name: "no session_id", command: "STOP_SESSION",
			body: func(s *stage) any { return map[string]any{"response_url": s.emsp.url("x")} },
			http: 400, code: 2001},
		{name: "a reservation_id over 36 characters", command: "CANCEL_RESERVATION",
			body: func(s *stage) any {
				return map[string]any{"response_url": s.emsp.url("x"), "reservation_id": strings.Repeat("r", 37)}
			},
			http: 400, code: 2001},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStage(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			got := s.post(tc.command, tc.body(s))
			if got.http != tc.http || got.code != tc.code || got.result != tc.result {
				t.Fatalf("answer = %+v, want HTTP %d, %d, %q", got, tc.http, tc.code, tc.result)
			}
			if !strings.Contains(got.message, tc.mention) {
				t.Errorf("message %q should mention %q", got.message, tc.mention)
			}
			if tc.result != "" && got.timeout != 0 {
				t.Errorf("timeout = %d, want 0: no result is coming", got.timeout)
			}
			s.quiet(tc.name)
		})
	}
}

// Ids are CiStrings: the location, the EVSE and the reservation are found
// whatever their case.
func TestIdsAreCaseInsensitive(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	got := s.post("RESERVE_NOW", s.reserveBody("r1", map[string]any{"location_id": "site-01", "evse_uid": "evse-a1"}))
	if got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	s.result("r1")
	if _, ok := s.core.ActiveReservation("EVSE-A1"); !ok {
		t.Error("EVSE-A1 is not reserved")
	}
}

// The module needs module access: a request without the partner's token is
// turned away before it gets anywhere.
func TestCommandsNeedTheModulesToken(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	req, err := http.NewRequest(http.MethodPost, s.cmdURL+"UNLOCK_CONNECTOR", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP %d without a token, want 401", resp.StatusCode)
	}
	s.quiet("an unauthenticated request")
}

// Without an evse_uid the first AVAILABLE EVSE of the whole location is
// taken: across chargers, skipping what is busy, and for a reservation what
// is not reservable.
func TestReserveWithoutAnEVSEPicksTheFirstAvailable(t *testing.T) {
	t.Parallel()
	s := newStage(t)

	s.setStatus("EVSE-A1", core.StatusCharging)
	got := s.post("RESERVE_NOW", s.reserveBody("r1", map[string]any{"reservation_id": "R1", "evse_uid": nil}))
	if got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	s.result("r1")
	if _, ok := s.core.ActiveReservation("EVSE-A2"); !ok {
		t.Fatal("EVSE-A2 (the first AVAILABLE one) is not reserved")
	}

	// A is now full except for A3, which cannot be reserved: B takes it. A2
	// still reads AVAILABLE (the fake station reports nothing), but is held.
	got = s.post("RESERVE_NOW", s.reserveBody("r2", map[string]any{"reservation_id": "R2", "evse_uid": nil}))
	if got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	s.result("r2")
	if r, ok := s.core.ActiveReservation("EVSE-B1"); !ok || r.ChargePointID != cpB {
		t.Fatalf("EVSE-B1 is not reserved: %+v", r)
	}
	if n := len(s.station(cpB).reserved()); n != 1 {
		t.Errorf("charger B got %d ReserveNow, want 1", n)
	}

	got = s.post("RESERVE_NOW", s.reserveBody("r3", map[string]any{"reservation_id": "R3", "evse_uid": nil}))
	if got.result != "REJECTED" || !strings.Contains(got.message, "no EVSE") {
		t.Fatalf("answer = %+v, want a synchronous REJECTED", got)
	}
}

// A reservation_id we know replaces that reservation: one reservation, the
// same OCPP id, the second request's expiry, tag and response_url. A second
// reservation on the same EVSE under another id, or one that would move it to
// another EVSE, is refused at once.
func TestReserveNowTwiceReplacesTheReservation(t *testing.T) {
	t.Parallel()
	s := newStage(t)

	s.post("RESERVE_NOW", s.reserveBody("first", nil))
	s.result("first")
	original, _ := s.core.ActiveReservation("EVSE-A1")

	later := ocpi.FormatDateTime(t0.Add(45 * time.Minute))
	got := s.post("RESERVE_NOW", s.reserveBody("second", map[string]any{
		"reservation_id": "res-1", // a CiString: the case is not significant
		"expiry_date":    later,
		"token":          map[string]any{"uid": "OTHER-TAG"},
	}))
	if got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("second"); res != "ACCEPTED" {
		t.Fatalf("second result = %s", res)
	}

	snap, _ := s.core.Snapshot().ChargePoint(cpA)
	if len(snap.Reservations) != 1 {
		t.Fatalf("core holds %+v, want one reservation", snap.Reservations)
	}
	r := snap.Reservations[0]
	if r.ID != original.ID || r.Status != core.ReservationActive || !r.ExpiresAt.Equal(t0.Add(45*time.Minute)) ||
		r.IDTag != "OTHER-TAG" || r.ResponseURL != s.emsp.url("second") {
		t.Errorf("reservation = %+v, want id %d, active, expiring at +45m, for OTHER-TAG, answering second", r, original.ID)
	}
	sent := s.station(cpA).reserved()
	if len(sent) != 2 || sent[0].ReservationID != original.ID || sent[1].ReservationID != original.ID ||
		!sent[1].ExpiresAt.Equal(t0.Add(45*time.Minute)) || sent[1].IDTag != "OTHER-TAG" {
		t.Errorf("the station was sent %+v, want two ReserveNow under id %d", sent, original.ID)
	}
	if n := s.station(cpA).count("cancel"); n != 0 {
		t.Errorf("%d CancelReservation sent for a replacement", n)
	}

	// It expires at the new time, not the old one.
	s.clk.Advance(31 * time.Minute)
	if _, ok := s.core.ActiveReservation("EVSE-A1"); !ok {
		t.Error("the replaced reservation expired at its old time")
	}

	// Moving it to another EVSE is not something 1.6 defines.
	got = s.post("RESERVE_NOW", s.reserveBody("move", map[string]any{"evse_uid": "EVSE-A2", "expiry_date": ocpi.FormatDateTime(s.clk.Now().Add(time.Hour))}))
	if got.result != "REJECTED" || !strings.Contains(got.message, "held on EVSE EVSE-A1") {
		t.Errorf("a move: %+v, want REJECTED", got)
	}
	// Another id on a reserved EVSE.
	got = s.post("RESERVE_NOW", s.reserveBody("clash", map[string]any{
		"reservation_id": "RES-2", "expiry_date": ocpi.FormatDateTime(s.clk.Now().Add(time.Hour)),
	}))
	if got.result != "REJECTED" || !strings.Contains(got.message, "already reserved") {
		t.Errorf("a second reservation: %+v, want REJECTED", got)
	}
	// A replacement may leave the EVSE out.
	got = s.post("RESERVE_NOW", s.reserveBody("again", map[string]any{
		"evse_uid": nil, "expiry_date": ocpi.FormatDateTime(s.clk.Now().Add(time.Hour)),
	}))
	if got.result != "ACCEPTED" {
		t.Errorf("a replacement without an evse_uid: %+v, want ACCEPTED", got)
	}
	s.result("again")
	if n := len(s.core.Snapshot().ChargePoints[0].Reservations); n != 1 {
		t.Errorf("%d reservations after the third request", n)
	}
}

// A replacement the station does not take sends no CancelReservation (the id
// is the one the station already holds, so it would drop the original), and
// reports the failure. The original ends locally only when what the station
// holds is unknown (it was silent, or the connection dropped); when the
// station answered no, it holds the original, and so do we.
func TestFailedReplacementSendsNoCancel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		answer func(n int) (ocpp.Result, error)
		want   string
		kept   bool
	}{
		{"the station is silent", func(int) (ocpp.Result, error) { return ocpp.Result{}, ocpp.ErrTimeout }, "TIMEOUT", false},
		{"the station says no", func(int) (ocpp.Result, error) {
			return ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}, nil
		}, "REJECTED", true},
		{"the connection drops", func(int) (ocpp.Result, error) { return ocpp.Result{}, ocpp.ErrNotConnected }, "FAILED", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStage(t)
			s.post("RESERVE_NOW", s.reserveBody("first", nil))
			s.result("first")

			s.station(cpA).setAnswer("reserve", func(n int) (ocpp.Result, error) {
				if n == 1 {
					return ocpp.Result{Status: ocpp.CommandAccepted, Raw: "Accepted"}, nil
				}
				return tc.answer(n)
			})
			later := ocpi.FormatDateTime(t0.Add(45 * time.Minute))
			s.post("RESERVE_NOW", s.reserveBody("second", map[string]any{"expiry_date": later}))
			if res, _ := s.result("second"); res != tc.want {
				t.Fatalf("result = %s, want %s", res, tc.want)
			}

			settle() // a cancel, if one were sent, goes out in the background
			if got := s.station(cpA).cancelled(); len(got) != 0 {
				t.Errorf("CancelReservation sent for %v: it would cancel the original, which the station may still hold", got)
			}
			rs := s.core.Snapshot().ChargePoints[0].Reservations
			_, found := s.core.ReservationByOCPIID("RES-1")
			if tc.kept {
				if len(rs) != 1 || rs[0].Status != core.ReservationActive || !found {
					t.Errorf("core holds %+v (found by OCPI id: %v), want the original, active", rs, found)
				}
			} else if len(rs) != 0 || found {
				t.Errorf("core still holds %+v (found by OCPI id: %v), want nothing", rs, found)
			}
			// The partner is told by the command's own result; the original's
			// response_url hears nothing more.
			if n := len(s.emsp.got("first")); n != 1 {
				t.Errorf("the first response_url got %d posts, want only its ACCEPTED", n)
			}
			if n := len(s.emsp.got("second")); n != 1 {
				t.Errorf("the second response_url got %d posts, want one", n)
			}
		})
	}
}

// What the station says to a new reservation becomes the result, and a
// reservation that was not taken is not left in core. A reservation that is
// silent is cancelled in the background, and goes once that has answered.
func TestReserveResults(t *testing.T) {
	t.Parallel()

	answer := func(status ocpp.CommandStatus, raw string) func(int) (ocpp.Result, error) {
		return func(int) (ocpp.Result, error) { return ocpp.Result{Status: status, Raw: raw}, nil }
	}
	tests := []struct {
		name   string
		answer func(int) (ocpp.Result, error)
		want   string
		cancel bool
	}{
		{"rejected", answer(ocpp.CommandRejected, "Rejected"), "REJECTED", false},
		{"occupied", answer(ocpp.CommandOccupied, "Occupied"), "EVSE_OCCUPIED", false},
		{"faulted", answer(ocpp.CommandFaulted, "Faulted"), "EVSE_INOPERATIVE", false},
		{"unavailable", answer(ocpp.CommandUnavailable, "Unavailable"), "EVSE_INOPERATIVE", false},
		{"silent", func(int) (ocpp.Result, error) { return ocpp.Result{}, ocpp.ErrTimeout }, "TIMEOUT", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStage(t)
			s.station(cpA).setAnswer("reserve", tc.answer)
			s.post("RESERVE_NOW", s.reserveBody("r1", nil))
			if res, _ := s.result("r1"); res != tc.want {
				t.Fatalf("result = %s, want %s", res, tc.want)
			}
			if tc.cancel {
				eventually(t, "the orphan cancel", func() bool { return len(s.station(cpA).cancelled()) == 1 })
			}
			eventually(t, "the reservation to be gone from core", func() bool {
				return len(s.core.Snapshot().ChargePoints[0].Reservations) == 0
			})
			if _, ok := s.core.ActiveReservation("EVSE-A1"); ok {
				t.Error("a reservation the station did not take is active")
			}
			settle()
			if n := len(s.emsp.got("r1")); n != 1 {
				t.Errorf("the partner got %d posts, want its one result (no CANCELED_RESERVATION for a reservation that never was)", n)
			}
		})
	}
}

func TestCancelReservation(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	s.post("RESERVE_NOW", s.reserveBody("r1", nil))
	s.result("r1")
	held, _ := s.core.ActiveReservation("EVSE-A1")

	got := s.post("CANCEL_RESERVATION", map[string]any{"response_url": s.emsp.url("c1"), "reservation_id": "res-1"})
	// Even for an active reservation, which needs one station call now: a
	// replacement can start after this is read, and the cancel then waits for
	// its ReserveNow and sends its own, so the time is given for two calls
	// (twice twice charger A's 7s, plus 5).
	if got.http != 200 || got.code != 1000 || got.result != "ACCEPTED" || got.timeout != 33 {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("c1"); res != "ACCEPTED" {
		t.Fatalf("result = %s", res)
	}
	if ids := s.station(cpA).cancelled(); len(ids) != 1 || ids[0] != held.ID {
		t.Errorf("the station was asked to cancel %v, want %d", ids, held.ID)
	}
	if _, ok := s.core.ActiveReservation("EVSE-A1"); ok {
		t.Error("the reservation is still held")
	}
	settle()
	if n := len(s.emsp.got("r1")); n != 1 {
		t.Errorf("the reserve's response_url got %d posts: the partner asked for the cancel, so it is not also told the CPO cancelled", n)
	}

	// An id we do not know is accepted, and then reported as unknown.
	before := s.station(cpA).total()
	got = s.post("CANCEL_RESERVATION", map[string]any{"response_url": s.emsp.url("c2"), "reservation_id": "NOPE"})
	if got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v, want ACCEPTED", got)
	}
	if res, _ := s.result("c2"); res != "UNKNOWN_RESERVATION" {
		t.Errorf("result = %s, want UNKNOWN_RESERVATION", res)
	}
	if s.station(cpA).total() != before {
		t.Error("a cancel of an unknown reservation reached the station")
	}
}

// When the CPO ends a reservation the partner holds, the partner is told:
// cpms cancel, and another tag starting a session on the EVSE. Expiry and
// redemption are not reported.
func TestReservationsTheCPOEndsAreReportedToThePartner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	reserve := func(t *testing.T) *stage {
		s := newStage(t)
		s.post("RESERVE_NOW", s.reserveBody("r1", nil))
		s.result("r1")
		return s
	}
	canceled := func(t *testing.T, s *stage, msgPart string) {
		t.Helper()
		eventually(t, "CANCELED_RESERVATION", func() bool { return len(s.emsp.got("r1")) == 2 })
		p := s.emsp.got("r1")[1]
		if p.body["result"] != "CANCELED_RESERVATION" {
			t.Errorf("second result = %v, want CANCELED_RESERVATION", p.body["result"])
		}
		if !strings.Contains(strings.ToLower(strings.TrimSpace(textOf(p.body))), msgPart) {
			t.Errorf("message %q should mention %q", textOf(p.body), msgPart)
		}
		settle()
		if n := len(s.emsp.got("r1")); n != 2 {
			t.Errorf("the partner got %d posts, want its ACCEPTED and one CANCELED_RESERVATION", n)
		}
	}

	t.Run("cpms cancel", func(t *testing.T) {
		t.Parallel()
		s := reserve(t)
		if err := s.ctl.Cancel(ctx, "EVSE-A1"); err != nil {
			t.Fatal(err)
		}
		canceled(t, s, "cancelled by the cpo")
	})
	t.Run("another tag starts a session", func(t *testing.T) {
		t.Parallel()
		s := reserve(t)
		s.core.StartTransaction(cpA, core.TransactionStart{ID: "7", ConnectorID: 1, IDTag: "SOMEBODY-ELSE"})
		canceled(t, s, "another tag")
	})
	t.Run("expiry says nothing", func(t *testing.T) {
		t.Parallel()
		s := reserve(t)
		s.clk.Advance(31 * time.Minute)
		eventually(t, "the reservation to expire", func() bool { return len(s.core.Snapshot().ChargePoints[0].Reservations) == 0 })
		settle()
		if n := len(s.emsp.got("r1")); n != 1 {
			t.Errorf("the partner got %d posts after expiry, want only its ACCEPTED", n)
		}
	})
	t.Run("redeeming says nothing", func(t *testing.T) {
		t.Parallel()
		s := reserve(t)
		tx := s.core.StartTransaction(cpA, core.TransactionStart{ID: "7", ConnectorID: 1, IDTag: tag})
		if tx.ReservationID == 0 {
			t.Fatal("the session did not redeem the reservation")
		}
		settle()
		if n := len(s.emsp.got("r1")); n != 1 {
			t.Errorf("the partner got %d posts after the redemption, want only its ACCEPTED", n)
		}
	})
}

func textOf(body map[string]any) string {
	var out string
	if ms, ok := body["message"].([]any); ok {
		for _, m := range ms {
			if mm, ok := m.(map[string]any); ok {
				out += mm["text"].(string)
			}
		}
	}
	return out
}

// START_SESSION sends RemoteStartTransaction with the token's uid, and the
// token is accepted for the next 15 minutes only.
func TestStartSession(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	if s.core.TagAuthorised(tag) {
		t.Fatal("the token is authorised before any command")
	}

	got := s.post("START_SESSION", s.startBody("s1", nil))
	if got.http != 200 || got.result != "ACCEPTED" || got.timeout != 19 {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("s1"); res != "ACCEPTED" {
		t.Fatalf("result = %s", res)
	}
	st := s.station(cpA)
	st.mu.Lock()
	starts := append(st.starts[:0:0], st.starts...)
	st.mu.Unlock()
	if len(starts) != 1 || starts[0].IDTag != tag || starts[0].EVSEUID != "EVSE-A1" {
		t.Fatalf("the station was sent %+v", starts)
	}
	if !s.core.TagAuthorised(tag) || !s.core.TagAuthorised(strings.ToLower(tag)) {
		t.Error("the station would refuse the token it was just told to start for")
	}
	s.clk.Advance(core.TagGrantWindow + time.Second)
	if s.core.TagAuthorised(tag) {
		t.Error("the token is still authorised after the 15 minutes")
	}
}

func TestStartSessionWithoutAnEVSEPicksTheFirstAvailable(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	for _, e := range []string{"EVSE-A1", "EVSE-A2", "EVSE-A3"} {
		s.setStatus(e, core.StatusCharging)
	}
	got := s.post("START_SESSION", s.startBody("s1", map[string]any{"evse_uid": nil}))
	if got.result != "ACCEPTED" {
		t.Fatalf("answer = %+v", got)
	}
	s.result("s1")
	if s.station(cpA).total() != 0 || s.station(cpB).count("start") != 1 {
		t.Errorf("the start went to A (%d) instead of B's EVSE-B1", s.station(cpA).total())
	}

	// With B offline, nothing is available: A's EVSEs are charging.
	s.core.Disconnected(cpB, "test")
	got = s.post("START_SESSION", s.startBody("s2", map[string]any{"evse_uid": nil}))
	if got.result != "REJECTED" || !strings.Contains(got.message, "no EVSE") {
		t.Errorf("answer = %+v, want REJECTED", got)
	}
}

func TestStartSessionResults(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	s.station(cpA).setAnswer("start", func(int) (ocpp.Result, error) {
		return ocpp.Result{Status: ocpp.CommandRejected, Raw: "Rejected"}, nil
	})
	s.post("START_SESSION", s.startBody("s1", nil))
	if res, _ := s.result("s1"); res != "REJECTED" {
		t.Errorf("result = %s, want REJECTED", res)
	}
}

func TestStopSession(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	s.core.StartTransaction(cpA, core.TransactionStart{ID: "42", ConnectorID: 2, IDTag: tag})
	s.core.StartTransaction(cpB, core.TransactionStart{ID: "43", ConnectorID: 1, IDTag: tag})
	stop := func(id, name string) reply {
		return s.post("STOP_SESSION", map[string]any{"response_url": s.emsp.url(name), "session_id": id})
	}

	got := stop("42", "p1")
	if got.http != 200 || got.result != "ACCEPTED" || got.timeout != 19 {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("p1"); res != "ACCEPTED" {
		t.Fatalf("result = %s", res)
	}
	st := s.station(cpA)
	st.mu.Lock()
	stops := append([]string(nil), st.stops...)
	st.mu.Unlock()
	if len(stops) != 1 || stops[0] != "42" || s.station(cpB).total() != 0 {
		t.Fatalf("station A was sent %v (B: %d), want a stop of 42 on A only", stops, s.station(cpB).total())
	}

	// A session that has ended is unknown, and so is one that never was.
	s.core.StopTransaction(cpA, "42", 0, "Remote", time.Time{})
	before := s.station(cpA).total()
	for _, id := range []string{"42", "99"} {
		if got := stop(id, "p2"); got.result != "UNKNOWN_SESSION" || got.timeout != 0 {
			t.Errorf("stop %s: %+v, want UNKNOWN_SESSION", id, got)
		}
	}
	// A session on a charger that is not connected.
	s.core.Disconnected(cpB, "test")
	if got := stop("43", "p3"); got.result != "REJECTED" || !strings.Contains(got.message, "offline") {
		t.Errorf("stop 43 with CP-B offline: %+v, want REJECTED", got)
	}
	settle()
	if s.station(cpA).total() != before || len(s.emsp.got("p2"))+len(s.emsp.got("p3")) != 0 {
		t.Error("a refused stop reached a station or the partner")
	}
}

func TestUnlockConnector(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	got := s.post("UNLOCK_CONNECTOR", s.unlockBody("u1", nil))
	if got.result != "ACCEPTED" || got.timeout != 19 {
		t.Fatalf("answer = %+v", got)
	}
	if res, _ := s.result("u1"); res != "ACCEPTED" {
		t.Errorf("result = %s, want ACCEPTED", res)
	}
	st := s.station(cpA)
	st.mu.Lock()
	unlocks := append([]string(nil), st.unlocks...)
	st.mu.Unlock()
	if len(unlocks) != 1 || unlocks[0] != "EVSE-A1" {
		t.Errorf("unlocks = %v", unlocks)
	}

	st.setAnswer("unlock", func(int) (ocpp.Result, error) {
		return ocpp.Result{Status: ocpp.CommandUnlockFailed, Raw: "UnlockFailed"}, nil
	})
	s.post("UNLOCK_CONNECTOR", s.unlockBody("u2", nil))
	if res, _ := s.result("u2"); res != "FAILED" {
		t.Errorf("result under UnlockFailed = %s, want FAILED", res)
	}
}

// A CommandResult the partner will not take is retried with backoff, up to
// ocpi.push.max_retries, and then logged. It never holds up the next command.
func TestResultDeliveryRetriesWithoutBlockingTheNextCommand(t *testing.T) {
	t.Parallel()
	retry := newClock()
	s := newStage(t, func(o *stageOptions) {
		o.backoff = func(int) time.Duration { return time.Second }
		o.afterFunc = retry.afterFunc
	})
	s.emsp.failWith("bad", 500)

	s.post("UNLOCK_CONNECTOR", s.unlockBody("bad", nil))
	eventually(t, "the first attempt", func() bool { return len(s.emsp.got("bad")) == 1 })
	eventually(t, "the wait before the retry", func() bool { return retry.Pending() == 1 })

	// The partner is still failing the first result; the next command is
	// carried out and its result delivered meanwhile.
	s.post("UNLOCK_CONNECTOR", s.unlockBody("good", nil))
	if res, _ := s.result("good"); res != "ACCEPTED" {
		t.Fatalf("second result = %s", res)
	}
	if retry.Pending() != 1 {
		t.Fatalf("the first result is no longer waiting to retry (%d timers)", retry.Pending())
	}

	// max_retries is 3: one attempt and three retries.
	for want := 2; want <= 4; want++ {
		retry.Advance(time.Second)
		w := want
		eventually(t, "a retry", func() bool { return len(s.emsp.got("bad")) == w })
		if w < 4 {
			eventually(t, "the next wait", func() bool { return retry.Pending() == 1 })
		}
	}
	eventually(t, "the give-up to be logged", func() bool {
		return strings.Contains(s.logs.String(), "could not post a command result, giving up")
	})
	retry.Advance(time.Minute)
	settle()
	if n := len(s.emsp.got("bad")); n != 4 {
		t.Errorf("%d attempts, want 4 (the first and three retries)", n)
	}
}

// A partner that refuses the request itself is not asked again.
func TestResultDeliveryDoesNotRetryARefusal(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	s.emsp.failWith("gone", 404)
	s.post("UNLOCK_CONNECTOR", s.unlockBody("gone", nil))
	eventually(t, "the refusal to be logged", func() bool {
		return strings.Contains(s.logs.String(), "will not be retried")
	})
	settle()
	if n := len(s.emsp.got("gone")); n != 1 {
		t.Errorf("%d attempts after a 404, want 1", n)
	}
}

// Two chargers: a command for an EVSE reaches that charger only, and it is
// that charger being offline that refuses it.
func TestOfflineMeansTheChargerThatOwnsTheEVSE(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	s.core.Disconnected(cpB, "test")

	got := s.post("RESERVE_NOW", s.reserveBody("b", map[string]any{"evse_uid": "EVSE-B1"}))
	if got.result != "REJECTED" || !strings.Contains(got.message, "CP-B") {
		t.Fatalf("EVSE-B1 with CP-B offline: %+v", got)
	}
	got = s.post("RESERVE_NOW", s.reserveBody("a", nil))
	if got.result != "ACCEPTED" {
		t.Fatalf("EVSE-A1 with only CP-B offline: %+v", got)
	}
	s.result("a")
	if s.station(cpB).total() != 0 {
		t.Error("a command reached the offline charger's station")
	}
}

// The background command outlives the request that started it: the HTTP
// request context ends when the answer is written, and the station is
// asked afterwards.
func TestTheCommandOutlivesTheRequest(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	release := s.station(cpA).holdOp("unlock")
	got := s.post("UNLOCK_CONNECTOR", s.unlockBody("u1", nil))
	if got.result != "ACCEPTED" {
		t.Fatal(got)
	}
	<-s.station(cpA).entered
	time.Sleep(50 * time.Millisecond) // the request is long over
	release()
	if res, _ := s.result("u1"); res != "ACCEPTED" {
		t.Errorf("result = %s", res)
	}
}

// A response_url belongs to the registration its command arrived under. When
// another partner registers, its token is never sent there.
func TestResultsAreDroppedWhenThePartnerRegistrationChanged(t *testing.T) {
	t.Parallel()
	const dropped = "the partner registration it was for has changed"
	other := t0.Add(time.Minute)

	t.Run("a result that was still to be delivered", func(t *testing.T) {
		t.Parallel()
		s := newStage(t)
		release := s.station(cpA).holdOp("reserve")
		if got := s.post("RESERVE_NOW", s.reserveBody("r1", nil)); got.result != "ACCEPTED" {
			t.Fatalf("answer = %+v", got)
		}
		s.partner.set(other, "partner-b-token")
		release()

		eventually(t, "the result to be dropped", func() bool { return strings.Contains(s.logs.String(), dropped) })
		if got := s.emsp.got("r1"); len(got) != 0 {
			t.Errorf("a result went to the former partner's response_url: %+v", got)
		}
	})

	t.Run("the end of a reservation made before", func(t *testing.T) {
		t.Parallel()
		s := newStage(t)
		s.post("RESERVE_NOW", s.reserveBody("r1", nil))
		s.result("r1")

		s.partner.set(other, "partner-b-token")
		if err := s.ctl.Cancel(context.Background(), "EVSE-A1"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the delivery to be dropped", func() bool { return strings.Contains(s.logs.String(), dropped) })
		if got := s.emsp.got("r1"); len(got) != 1 {
			t.Errorf("the former partner got %d posts, want only its ACCEPTED: %+v", len(got), got)
		}
	})

	t.Run("a retry after the partner changed", func(t *testing.T) {
		t.Parallel()
		clk := newClock()
		s := newStage(t, func(o *stageOptions) { o.afterFunc = clk.afterFunc })
		s.emsp.status["r1"] = http.StatusInternalServerError
		s.post("RESERVE_NOW", s.reserveBody("r1", nil))
		eventually(t, "the first attempt", func() bool { return len(s.emsp.got("r1")) == 1 })
		eventually(t, "a retry to be waited for", func() bool { return clk.Pending() == 1 })

		s.partner.set(other, "partner-b-token")
		clk.Advance(time.Hour)
		eventually(t, "the retry to be dropped", func() bool { return strings.Contains(s.logs.String(), dropped) })
		if got := s.emsp.got("r1"); len(got) != 1 {
			t.Errorf("the former partner got %d posts, want only the first attempt: %+v", len(got), got)
		}
	})

	t.Run("the same registration with a new token", func(t *testing.T) {
		t.Parallel()
		s := newStage(t)
		release := s.station(cpA).holdOp("reserve")
		s.post("RESERVE_NOW", s.reserveBody("r1", nil))
		// A credentials PUT rotates TOKEN_B and keeps the registration.
		s.partner.set(s.partner.Registered(), "rotated-token")
		release()

		s.result("r1")
		want := "Token " + base64.StdEncoding.EncodeToString([]byte("rotated-token"))
		if got := s.emsp.got("r1")[0].auth; got != want {
			t.Errorf("Authorization = %q, want the rotated token %q", got, want)
		}
	})
}

// A command whose response_url is not on the registered partner's host is
// refused at once (HTTP 400, 2001), and nothing is done or sent. TOKEN_C is
// the same for every registration, so a former partner that holds it would
// otherwise be sent the current partner's TOKEN_B at a URL of its own.
func TestACommandWithAResponseURLOutsideThePartnersHostIsRefused(t *testing.T) {
	t.Parallel()
	s := newStage(t)
	foreign := newEMSP(t)
	s.partner.onlyAt(s.emsp.srv.URL)

	for _, command := range []string{"RESERVE_NOW", "CANCEL_RESERVATION", "START_SESSION", "STOP_SESSION", "UNLOCK_CONNECTOR"} {
		body := map[string]any{
			"response_url": foreign.url("x"), "reservation_id": "RES-1", "session_id": "s1",
			"token":       map[string]any{"uid": tag, "type": "RFID"},
			"expiry_date": ocpi.FormatDateTime(t0.Add(30 * time.Minute)),
			"location_id": locationID, "evse_uid": "EVSE-A1", "connector_id": "1",
		}
		got := s.post(command, body)
		if got.http != http.StatusBadRequest || got.code != 2001 {
			t.Errorf("%s: answer = %+v, want HTTP 400 and 2001", command, got)
		}
	}
	if n := s.station(cpA).total() + s.station(cpB).total(); n != 0 {
		t.Errorf("the station got %d commands, want none", n)
	}
	if n := foreign.all(); n != 0 {
		t.Errorf("the foreign host got %d requests, want none", n)
	}

	// The same command at the partner's own host goes through.
	if got := s.post("RESERVE_NOW", s.reserveBody("r1", nil)); got.result != "ACCEPTED" {
		t.Errorf("answer at the partner's host = %+v, want ACCEPTED", got)
	}
	s.result("r1")
}
