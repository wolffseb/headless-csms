package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest"
)

// With a control call that never returns, the real program keeps moving the
// selection and rendering bus events: Update has not waited for the call.
func TestProgramStaysResponsiveWhileACallIsStuck(t *testing.T) {
	r := newRig(t)
	r.ctl.gate = make(chan struct{})
	tm := teatest.NewTestModel(t, r.m, teatest.WithInitialTermSize(100, 30))
	// Cleanups run last in, first out: the stuck call is released before the
	// program is torn down.
	t.Cleanup(func() { close(r.ctl.gate) })

	seen := func(want string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(want)) },
			teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
	}

	// A send that waits for an Update stuck behind the call would hang the
	// test; bound it so that failure is a failure.
	send := func(k string) {
		t.Helper()
		sent := make(chan struct{})
		go func() { tm.Send(keyMsg(k)); close(sent) }()
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatalf("the program did not take %q: Update is stuck behind the call", k)
		}
	}

	send("s")
	seen("start ALP-HYC-001-1: waiting for the station")

	send("down")
	seen("▸   ALP-HYC-001-2")

	// The bus bridge re-arms itself: two events in a row both arrive.
	r.core.Connected(cp1)
	seen("ALP-HYC-001 connected")
	r.core.Connected(cp2)
	seen("ALP-HYC-002 connected")

	if got := r.ctl.Calls(); len(got) != 1 || !strings.HasPrefix(got[0], "start ALP-HYC-001-1") {
		t.Errorf("calls = %v", got)
	}
}
