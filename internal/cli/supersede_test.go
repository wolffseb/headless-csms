package cli_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/cli"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/simulator"
	"github.com/wolffseb/cli-cpms/internal/state"
)

// #73 uses ports 19700-19709.

// TestRestartedStationStartingAgainCanBeStopped is #73 as it happened on a
// station emulator: a session runs, cpms restarts, and the station comes back
// as if new, boots and starts another session on the same connector without
// ever stopping the first. cpms ends the first itself, so `cpms stop` knows
// which session to stop.
func TestRestartedStationStartingAgainCanBeStopped(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19700)
	s.connect()

	s.mustCPMS("start")
	first := s.waitStatus("the first session", func(st socket.Status) bool { return st.EVSEs[0].Transaction != nil }).EVSEs[0].Transaction

	// The station drops off without a StopTransaction and forgets its
	// session; cpms restarts meanwhile.
	s.sim.Close()
	s.sim.Wait()
	s.stopRun()
	s.startRun()

	s.connect()
	s.mustCPMS("start")
	second := s.waitStatus("a second session", func(st socket.Status) bool {
		tx := st.EVSEs[0].Transaction
		return tx != nil && tx.ID != first.ID
	}).EVSEs[0].Transaction

	if out := s.mustCPMS("stop"); !strings.Contains(out, "EVSE-1 remote stop accepted") {
		t.Errorf("stop printed %q", out)
	}
	s.waitStatus("no session left", func(st socket.Status) bool { return st.EVSEs[0].Transaction == nil })

	st, err := state.Open(filepath.Join(filepath.Dir(s.configPath), cli.DefaultStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if txs := st.Get().Transactions; len(txs) != 0 {
		t.Errorf("state.json still holds %+v after stopping %s; %s should have ended at its start", txs, second.ID, first.ID)
	}
}

// TestStopAndLimitWithTx covers a state.json from before #73 that already
// holds two sessions on one EVSE: the plain commands refuse to guess, and
// --tx chooses.
func TestStopAndLimitWithTx(t *testing.T) {
	t.Parallel()
	s := newStation(t, 19701)
	s.connect(func(o *simulator.Options) {
		o.Reconnect = true
		o.ReconnectBackoff = func(int) time.Duration { return 50 * time.Millisecond }
	})

	s.mustCPMS("start")
	real := s.waitStatus("the session", func(st socket.Status) bool { return st.EVSEs[0].Transaction != nil }).EVSEs[0].Transaction

	// The second session is one an older cpms left behind.
	s.stopRun()
	statePath := filepath.Join(filepath.Dir(s.configPath), cli.DefaultStateFile)
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	const stale = "77"
	err = store.Update(func(st *state.State) error {
		st.Transactions = append(st.Transactions, state.Transaction{
			ID: stale, ChargePointID: "ALP-HYC-001", EVSEUID: "EVSE-1", ConnectorID: 1,
			IDTag: "04A1B2C3D4", StartedAt: real.StartedAt.Add(-time.Hour),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.startRun()
	s.waitStatus("the station back online", func(st socket.Status) bool { return st.ChargePoint.Online })

	_, stderr, code := s.cpms("stop")
	expectExit(t, "stop with two sessions", code, cli.ExitError, stderr, "refusing to guess", stale, real.ID, "--tx")
	_, stderr, code = s.cpms("limit", "set", "--purpose", "tx", "--kw", "7")
	expectExit(t, "tx limit with two sessions", code, cli.ExitError, stderr, "refusing to guess", "--tx")

	// Checked before anything is sent.
	_, stderr, code = s.cpms("limit", "set", "--kw", "7", "--tx", real.ID)
	expectExit(t, "--tx on a tx-default limit", code, cli.ExitError, stderr, "--tx only applies to --purpose tx")
	_, stderr, code = s.cpms("stop", "--tx", "12345")
	expectExit(t, "stop --tx unknown", code, cli.ExitRefused, stderr, "transaction 12345")

	if out := s.mustCPMS("limit", "set", "--purpose", "tx", "--kw", "7", "--tx", real.ID); !strings.Contains(out, "transaction "+real.ID) {
		t.Errorf("limit set --tx %s printed %q, want the transaction named", real.ID, out)
	}
	s.mustCPMS("stop", "--tx", real.ID)
	// The real one ends; the stale one is all cpms has left, and no longer
	// ambiguous.
	s.waitStatus("only the stale session left", func(st socket.Status) bool {
		tx := st.EVSEs[0].Transaction
		return tx != nil && tx.ID == stale
	})
}
