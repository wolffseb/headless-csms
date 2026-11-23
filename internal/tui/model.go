package tui

import (
	"context"
	"fmt"
	"slices"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/wolffseb/cli-cpms/internal/control"
	"github.com/wolffseb/cli-cpms/internal/control/socket"
	"github.com/wolffseb/cli-cpms/internal/core"
)

// maxLogLines is how many lines the model keeps. The pane shows only as many
// as fit; the file has the rest.
const maxLogLines = 200

// tickEvery is how often the push status and the snapshot are read, so the
// header and the "until" times stay right with no event to prompt them.
const tickEvery = time.Second

type (
	eventMsg  core.Event
	logMsg    logLine
	tickMsg   time.Time
	actionMsg struct {
		uid string
		op  socket.Op
		// text is the success wording, empty on error.
		text string
		err  error
	}
)

type msgKind int

const (
	msgNone msgKind = iota
	msgInfo
	msgOK
	msgErr
)

// message is what the line above the hint bar says. It stays until the next
// one or esc: a timer would take the result away just as someone looks.
type message struct {
	kind msgKind
	text string
}

type lineKind int

const (
	lineInfo lineKind = iota
	lineOK
	lineErr
	lineWarn
)

type paneLine struct {
	at   time.Time
	kind lineKind
	text string
}

type chargerView struct {
	st    socket.ChargePointStatus
	evses []socket.EVSEStatus
}

// stopConfirm is a `x` waiting for its y.
type stopConfirm struct{ uid, tx string }

// Model is the Bubble Tea model. It only reads charger state, from core.
type Model struct {
	ctx  context.Context
	opts Options
	now  func() time.Time
	loc  *time.Location

	w, h int

	chargers []chargerView
	uids     []string
	sel      string

	ocpiPrefix, ocpiTail string
	ocpiFailed           bool

	log     []paneLine
	msg     message
	flights map[string]string // evse uid -> what it waits for
	confirm *stopConfirm
	help    bool

	spin     spinner.Model
	spinning bool

	events <-chan core.Event
	unsub  func()
}

// New builds the model and subscribes to the core bus. Close unsubscribes.
func New(ctx context.Context, opts Options) *Model {
	m := &Model{
		ctx: ctx, opts: opts, now: opts.Now, loc: opts.Location,
		w: 80, h: 24,
		flights: make(map[string]string),
		spin:    spinner.New(spinner.WithSpinner(spinner.MiniDot)),
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.loc == nil {
		m.loc = time.Local
	}
	if m.opts.Describe == nil {
		m.opts.Describe = func(_ socket.Op, subject string, err error) string { return subject + ": " + err.Error() }
	}
	m.events, m.unsub = opts.Core.Subscribe("tui")
	for _, l := range opts.Startup {
		m.addLine(m.now(), lineInfo, l)
	}
	m.refresh()
	m.refreshOCPI()
	return m
}

// Close unsubscribes from the bus.
func (m *Model) Close() { m.unsub() }

// Init starts the bridges from the bus and the log, and the clock.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.waitEvent(), m.waitLog(), m.tick())
}

// waitEvent waits for one bus event and hands it to Update, which asks for
// the next. Running in a Cmd keeps Update itself from ever blocking.
func (m *Model) waitEvent() tea.Cmd {
	ch, ctx := m.events, m.ctx
	return func() tea.Msg {
		select {
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			return eventMsg(e)
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Model) waitLog() tea.Cmd {
	if m.opts.Logs == nil {
		return nil
	}
	ch, ctx := m.opts.Logs.ch, m.ctx
	return func() tea.Msg {
		select {
		case l := <-ch:
			return logMsg(l)
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height

	case eventMsg:
		e := core.Event(msg)
		if e.Kind != core.EventHeartbeat && e.Kind != core.EventMeterValues {
			at := e.At
			if at.IsZero() {
				at = m.now()
			}
			m.addLine(at, lineInfo, m.eventText(e))
		}
		m.refresh()
		return m, m.waitEvent()

	case logMsg:
		m.addLine(msg.at, lineWarn, msg.text)
		return m, m.waitLog()

	case tickMsg:
		m.refresh()
		m.refreshOCPI()
		return m, m.tick()

	case spinner.TickMsg:
		if len(m.flights) == 0 {
			// The last action finished: let the chain end.
			m.spinning = false
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case actionMsg:
		m.finish(msg)

	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) addLine(at time.Time, kind lineKind, text string) {
	m.log = append(m.log, paneLine{at: at, kind: kind, text: text})
	if n := len(m.log) - maxLogLines; n > 0 {
		m.log = slices.Delete(m.log, 0, n)
	}
}

// refresh takes the charger state from core again.
func (m *Model) refresh() {
	snap := m.opts.Core.Snapshot()
	m.chargers = m.chargers[:0]
	m.uids = m.uids[:0]
	for _, ch := range m.opts.Config.Chargers {
		st := socket.BuildStatus(snap, ch.ID)
		m.chargers = append(m.chargers, chargerView{st: st.ChargePoint, evses: st.EVSEs})
		for _, e := range st.EVSEs {
			m.uids = append(m.uids, e.UID)
		}
	}
	// The selection is an EVSE, not a row number, so it stays put when rows
	// change; it only moves if its EVSE is gone.
	if !slices.Contains(m.uids, m.sel) {
		m.sel = ""
		if len(m.uids) > 0 {
			m.sel = m.uids[0]
		}
	}
}

// refreshOCPI reads the registration and the push health.
func (m *Model) refreshOCPI() {
	m.ocpiPrefix, m.ocpiTail, m.ocpiFailed = "OCPI not registered", "", false
	if m.opts.Registration == nil {
		return
	}
	o := socket.BuildOCPIStatus(m.opts.Registration.Registration())
	if !o.Registered {
		return
	}
	m.ocpiPrefix = "OCPI registered with " + o.Party
	if o.PushEndpoint == "" {
		m.ocpiTail = "push: no locations receiver"
		return
	}
	if m.opts.Push == nil {
		return
	}
	p := m.opts.Push.Status()
	failing := !p.LastErrorAt.IsZero() && (p.LastSuccess.IsZero() || p.LastErrorAt.After(p.LastSuccess))
	switch {
	case failing:
		m.ocpiTail = "last push failed " + m.clock(p.LastErrorAt) + ": " + truncate(p.LastError, 40)
		m.ocpiFailed = true
	case !p.LastSuccess.IsZero():
		m.ocpiTail = "last push " + m.clock(p.LastSuccess)
	default:
		m.ocpiTail = "nothing pushed yet"
	}
}

func (m *Model) clock(t time.Time) string { return t.In(m.loc).Format("15:04:05") }

func (m *Model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" || s == "q" {
		if m.opts.Stop != nil {
			m.opts.Stop()
		}
		return m, tea.Quit
	}

	if m.help {
		if s == "?" || s == "esc" {
			m.help = false
		}
		return m, nil
	}
	if c := m.confirm; c != nil {
		m.confirm = nil
		m.msg = message{}
		if s == "y" {
			return m, m.run(c.uid, socket.OpStop, "stop", func(ctx context.Context) (string, error) {
				return "remote stop accepted", m.opts.Control.Stop(ctx, c.uid, c.tx)
			})
		}
		return m, nil
	}

	switch s {
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "?":
		m.help = true
	case "esc":
		m.msg = message{}
	case "r":
		return m, m.reserve()
	case "c":
		return m, m.simple(socket.OpCancel, "cancel", "reservation cancelled", m.opts.Control.Cancel)
	case "s":
		tag := m.opts.Config.Auth.DefaultIDTag
		return m, m.simpleUID(socket.OpStart, "start", func(ctx context.Context, uid string) (string, error) {
			return "remote start accepted", m.opts.Control.Start(ctx, uid, tag)
		})
	case "u":
		return m, m.simple(socket.OpUnlock, "unlock", "unlocked", m.opts.Control.Unlock)
	case "t":
		return m, m.simple(socket.Op("refresh"), "refresh", "status requested", m.opts.Control.TriggerStatus)
	case "x":
		return m, m.stop()
	}
	return m, nil
}

// move selects the next or previous EVSE and stops at the ends: there is no
// wrapping, so holding a key never overshoots.
func (m *Model) move(d int) {
	i := slices.Index(m.uids, m.sel)
	if i < 0 {
		return
	}
	m.sel = m.uids[min(max(i+d, 0), len(m.uids)-1)]
}

// stop asks before it stops a session, because it cannot be undone. With no
// session there is nothing to confirm: control says so, in the CLI's words.
func (m *Model) stop() tea.Cmd {
	uid := m.sel
	if uid == "" {
		return nil
	}
	if m.flights[uid] != "" {
		return m.busy(uid)
	}
	for _, c := range m.chargers {
		for _, e := range c.evses {
			if e.UID == uid && e.Transaction != nil {
				m.confirm = &stopConfirm{uid: uid, tx: e.Transaction.ID}
				m.msg = message{msgInfo, fmt.Sprintf("Stop session %s on %s? y to confirm, any other key cancels", e.Transaction.ID, uid)}
				return nil
			}
		}
	}
	return m.run(uid, socket.OpStop, "stop", func(ctx context.Context) (string, error) {
		return "remote stop accepted", m.opts.Control.Stop(ctx, uid, "")
	})
}

func (m *Model) reserve() tea.Cmd {
	return m.simpleUID(socket.OpReserve, "reserve", func(ctx context.Context, uid string) (string, error) {
		// The default tag and length, named here rather than left to control,
		// so what the key does is visible where the key is handled.
		r, err := m.opts.Control.Reserve(ctx, control.ReserveParams{
			EVSEUID: uid, IDTag: m.opts.Config.Auth.DefaultIDTag,
			ExpiresIn: control.DefaultReservationLength, Source: "tui",
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("reserved for %s until %s (reservation %d)", r.IDTag, m.clock(r.ExpiresAt), r.ID), nil
	})
}

// simple runs a command that takes only the EVSE.
func (m *Model) simple(op socket.Op, label, ok string, f func(context.Context, string) error) tea.Cmd {
	return m.simpleUID(op, label, func(ctx context.Context, uid string) (string, error) {
		return ok, f(ctx, uid)
	})
}

func (m *Model) simpleUID(op socket.Op, label string, f func(context.Context, string) (string, error)) tea.Cmd {
	uid := m.sel
	if uid == "" {
		return nil
	}
	return m.run(uid, op, label, func(ctx context.Context) (string, error) { return f(ctx, uid) })
}

func (m *Model) busy(uid string) tea.Cmd {
	m.msg = message{msgInfo, fmt.Sprintf("%s is still waiting for %s; try again when it answers", uid, m.flights[uid])}
	return nil
}

// run starts a command in a tea.Cmd. One command per EVSE at a time: a second
// key for the same EVSE sends nothing, while the other EVSEs stay usable.
func (m *Model) run(uid string, op socket.Op, label string, f func(context.Context) (string, error)) tea.Cmd {
	if m.flights[uid] != "" {
		return m.busy(uid)
	}
	m.flights[uid] = label
	m.msg = message{msgInfo, fmt.Sprintf("%s %s: waiting for the station…", label, uid)}

	// The station may act on a command that is on the wire, so quitting does
	// not abandon it (the socket does the same). Each call is bounded by the
	// call timeout.
	ctx := context.WithoutCancel(m.ctx)
	call := func() tea.Msg {
		text, err := f(ctx)
		return actionMsg{uid: uid, op: op, text: text, err: err}
	}
	if m.spinning {
		return call
	}
	m.spinning = true
	return tea.Batch(call, m.spin.Tick)
}

// finish records the outcome of a command.
func (m *Model) finish(a actionMsg) {
	delete(m.flights, a.uid)
	if a.err != nil {
		text := "✗ " + m.opts.Describe(a.op, a.uid, a.err)
		m.msg = message{msgErr, text}
		m.addLine(m.now(), lineErr, text)
		return
	}
	text := "✓ " + a.uid + " " + a.text
	m.msg = message{msgOK, text}
	m.addLine(m.now(), lineOK, text)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
