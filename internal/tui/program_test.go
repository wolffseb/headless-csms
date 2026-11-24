package tui

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// syncBuf is what the program draws, safe to read while it is written.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) Contains(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Contains(s.b.Bytes(), []byte(want))
}

// With a control call that never returns, the real program keeps moving the
// selection and rendering bus events: Update has not waited for the call.
func TestProgramStaysResponsiveWhileACallIsStuck(t *testing.T) {
	r := newRig(t)
	r.ctl.gate = make(chan struct{})
	out := &syncBuf{}
	in, w := io.Pipe()
	p := tea.NewProgram(r.m, tea.WithInput(in), tea.WithOutput(out), tea.WithWindowSize(100, 30),
		tea.WithoutSignalHandler())
	finished := make(chan struct{})
	go func() { _, _ = p.Run(); close(finished) }()
	// Cleanups run last in, first out: the stuck call is released before the
	// program is torn down.
	t.Cleanup(func() {
		p.Quit()
		<-finished
		_ = w.Close()
	})
	t.Cleanup(func() { close(r.ctl.gate) })

	seen := func(want string) {
		t.Helper()
		waitFor(t, "the screen to show "+want, func() bool { return out.Contains(want) })
	}

	// A send that waits for an Update stuck behind the call would hang the
	// test; bound it so that failure is a failure.
	send := func(k string) {
		t.Helper()
		sent := make(chan struct{})
		go func() { p.Send(keyMsg(k)); close(sent) }()
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatalf("the program did not take %q: Update is stuck behind the call", k)
		}
	}

	send("s")
	seen("start ALP-HYC-001-1: waiting for the station")

	// The renderer redraws only the cells that changed, so the moved cursor is
	// seen by what a key does next: it acts on the second EVSE, while the
	// first is still waiting.
	send("down")
	send("s")
	waitFor(t, "the second start", func() bool { return len(r.ctl.Calls()) == 2 })

	// The bus bridge re-arms itself: two events in a row both arrive.
	r.core.Connected(cp1)
	seen("ALP-HYC-001 connected")
	r.core.Connected(cp2)
	seen("ALP-HYC-002 connected")

	if got := r.ctl.Calls(); len(got) != 2 || !strings.HasPrefix(got[0], "start ALP-HYC-001-1") || !strings.HasPrefix(got[1], "start ALP-HYC-001-2") {
		t.Errorf("calls = %v", got)
	}
}
