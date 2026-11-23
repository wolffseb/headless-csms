package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/wolffseb/cli-cpms/internal/control/socket"
)

// narrowBelow is the width under which a row's detail moves to a line of its
// own and the kW figure is dropped.
const narrowBelow = 60

const (
	hintWide   = "↑↓ select  r reserve  c cancel  s start  x stop  u unlock  t refresh  ? help  q quit"
	hintNarrow = "↑↓ r c s x u t   ? help   q quit"
	// messageLines is the room the message line has, always reserved so the
	// log does not jump when a long error arrives.
	messageLines = 2
	// keyCol is the width of the key column in the help.
	keyCol = 12
	// rowPrefix is cursor, spinner and their spaces: " ▸ ⠋ ".
	rowPrefix = 5
)

var (
	dim   = lipgloss.NewStyle().Faint(true)
	bold  = lipgloss.NewStyle().Bold(true)
	green = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	red   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// line is one screen line: its text, already cut to fit, and how to style it.
type line struct {
	text  string
	style *lipgloss.Style
}

// View is the screen, on the alternate screen: the terminal is given back
// as it was when the program ends.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// render draws the screen. Every line is cut to the width and the whole is
// exactly the height, so a resize never leaves anything overlapping.
func (m *Model) render() string {
	var lines []line
	if m.help {
		lines = m.helpLines()
	} else {
		lines = m.mainLines()
	}

	out := make([]string, 0, m.h)
	for i := 0; i < m.h; i++ {
		if i >= len(lines) {
			out = append(out, "")
			continue
		}
		l := lines[i]
		if l.style != nil {
			out = append(out, l.style.Render(l.text))
		} else {
			out = append(out, l.text)
		}
	}
	return strings.Join(out, "\n")
}

func (m *Model) narrow() bool { return m.w < narrowBelow }

// fit cuts plain text to the width, marking the cut.
func (m *Model) fit(s string) string { return ansi.Truncate(s, m.w, "…") }

func (m *Model) plain(s string) line { return line{text: m.fit(s)} }

func (m *Model) rule(title string) line {
	t := "──"
	if title != "" {
		t += " " + title + " "
	}
	if n := m.w - ansi.StringWidth(t); n > 0 {
		t += strings.Repeat("─", n)
	}
	return line{text: m.fit(t)}
}

func (m *Model) mainLines() []line {
	head := m.headerLines()
	head = append(head, m.rule(""))

	table, cursor := m.tableLines()
	tail := []line{m.rule("log")}

	foot := m.messageLines()
	foot = append(foot, m.plain(m.hint()))

	// What is left goes to the log. If the table alone does not fit, show the
	// part around the cursor rather than cut the bottom off.
	room := m.h - len(head) - len(tail) - len(foot)
	logH := max(room-len(table), 0)
	if room < len(table) {
		table = window(table, cursor, max(room, 0))
	}

	var out []line
	out = append(out, head...)
	out = append(out, table...)
	out = append(out, tail...)
	out = append(out, m.logLines(logH)...)
	// The footer sits at the bottom whatever the log's length.
	for len(out) < m.h-len(foot) {
		out = append(out, line{})
	}
	return append(out, foot...)
}

// window is h lines of lines that include the one at cursor.
func window(lines []line, cursor, h int) []line {
	if h >= len(lines) {
		return lines
	}
	start := min(max(cursor-h/2, 0), len(lines)-h)
	return lines[start : start+h]
}

func (m *Model) headerLines() []line {
	online := 0
	for _, c := range m.chargers {
		if c.st.Online {
			online++
		}
	}
	title := fmt.Sprintf("cpms · %d/%d chargers online", online, len(m.chargers))

	if m.narrow() {
		return []line{m.plain(title), m.ocpiLine(m.ocpiPrefix, m.ocpiTail)}
	}
	return []line{m.ocpiLine(title+" · "+m.ocpiPrefix, m.ocpiTail)}
}

// ocpiLine is the header line with the push failure, if any, in red: the
// part after prefix is cut and coloured as a whole.
func (m *Model) ocpiLine(prefix, tail string) line {
	if tail == "" {
		return m.plain(prefix)
	}
	full := m.fit(prefix + " · " + tail)
	if !m.ocpiFailed {
		return line{text: full}
	}
	// Style only what was cut from the tail.
	head := m.fit(prefix + " · ")
	if len(full) <= len(head) || !strings.HasPrefix(full, head) {
		return line{text: full}
	}
	return line{text: head + red.Render(full[len(head):])}
}

// tableLines is the chargers with their EVSE rows, and the index of the
// selected row in it.
func (m *Model) tableLines() ([]line, int) {
	idW, statusW := len("EVSE"), len("STATUS")
	for _, c := range m.chargers {
		for _, e := range c.evses {
			idW = max(idW, len(e.UID))
			statusW = max(statusW, len(statusText(e)))
		}
	}
	statusW = max(statusW, len("UNAVAILABLE"))

	var out []line
	cursor := 0
	for i, c := range m.chargers {
		var style *lipgloss.Style
		if !c.st.Online {
			style = &dim
		}
		out = append(out, line{text: m.fit(m.chargerText(c.st)), style: style})
		if i == 0 && !m.narrow() {
			out = append(out, line{text: m.fit(strings.Repeat(" ", rowPrefix) +
				pad("EVSE", idW) + "  " + pad("STATUS", statusW) + "  DETAIL"), style: style})
		}
		for _, e := range c.evses {
			selected := e.UID == m.sel
			if selected {
				cursor = len(out)
			}
			prefix := "   "
			if selected {
				prefix = " ▸ "
			}
			spin := " "
			if m.flights[e.UID] != "" {
				spin = m.spin.View()
			}
			detail := m.detail(e)
			row := prefix + spin + " " + pad(e.UID, idW) + "  " + pad(statusText(e), statusW)
			st := style
			if selected && st == nil {
				st = &bold
			}
			if m.narrow() {
				out = append(out, line{text: m.fit(strings.TrimRight(row, " ")), style: st})
				if detail != "" {
					out = append(out, line{text: m.fit(strings.Repeat(" ", rowPrefix) + detail), style: st})
				}
				continue
			}
			if detail == "" {
				detail = "-"
			}
			out = append(out, line{text: m.fit(row + "  " + detail), style: st})
		}
	}
	return out, cursor
}

func pad(s string, w int) string {
	if n := w - ansi.StringWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// chargerText is the charger's own line.
func (m *Model) chargerText(c socket.ChargePointStatus) string {
	state := "offline"
	if c.Online {
		state = "online"
	}
	s := c.ID + "  " + state
	if c.Online {
		if station := strings.TrimSpace(c.Vendor + " " + c.Model); station != "" {
			s += "  " + station
			if c.Firmware != "" {
				s += " fw " + c.Firmware
			}
		}
		return s
	}
	if c.LastSeen == nil {
		return s + "  never seen"
	}
	s += "  last seen " + m.clock(*c.LastSeen)
	if d := c.LastDisconnect; d != nil && d.Reason != "" {
		s += " (" + offlineReason(d.Reason) + ")"
	}
	return s
}

// offlineReason keeps the reasons that are words of ours (see closeReason in
// ocppj) and calls anything
// else, which is the transport's own error text, a lost connection. The full
// error is in cpms.log.
func offlineReason(reason string) string {
	switch reason {
	case "heartbeat timeout", "closed by charge point":
		return reason
	}
	return "connection lost"
}

// statusText is the status word the CLI prints, with the error code if any.
func statusText(e socket.EVSEStatus) string {
	if e.ErrorCode != "" && e.ErrorCode != "NoError" {
		return e.Status + " (" + e.ErrorCode + ")"
	}
	return e.Status
}

// detail is what an EVSE is holding or doing, or "" for nothing.
func (m *Model) detail(e socket.EVSEStatus) string {
	var parts []string
	if tx := e.Transaction; tx != nil {
		s := fmt.Sprintf("session %s · %s · since %s", tx.ID, tx.IDTag, tx.StartedAt.In(m.loc).Format("15:04"))
		if e.PowerW != nil && !m.narrow() {
			s += fmt.Sprintf(" · %.1f kW", *e.PowerW/1000)
		}
		parts = append(parts, s)
	}
	if r := e.Reservation; r != nil {
		s := fmt.Sprintf("#%d %s until %s", r.ID, r.IDTag, r.ExpiresAt.In(m.loc).Format("15:04"))
		if r.State != "active" {
			s += " (" + r.State + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// logLines is the last h lines of the log, newest at the bottom.
func (m *Model) logLines(h int) []line {
	if h <= 0 {
		return nil
	}
	shown := m.log
	if len(shown) > h {
		shown = shown[len(shown)-h:]
	}
	out := make([]line, 0, h)
	for i := len(shown); i < h; i++ {
		out = append(out, line{})
	}
	for _, l := range shown {
		text := l.at.In(m.loc).Format("15:04:05") + " " + l.text
		var style *lipgloss.Style
		switch l.kind {
		case lineErr:
			style = &red
		}
		out = append(out, line{text: m.fit(text), style: style})
	}
	return out
}

// messageLines is the message, wrapped to the lines reserved for it, with
// the last line cut if it still does not fit.
func (m *Model) messageLines() []line {
	out := make([]line, messageLines)
	if m.msg.kind == msgNone {
		return out
	}
	var style *lipgloss.Style
	switch m.msg.kind {
	case msgOK:
		style = &green
	case msgErr:
		style = &red
	}
	rows := wrap(m.msg.text, m.w, "  ")
	for i := range out {
		if i >= len(rows) {
			break
		}
		text := rows[i]
		if i == len(out)-1 && len(rows) > len(out) {
			text += "…"
		}
		out[i] = line{text: m.fit(text), style: style}
	}
	return out
}

func (m *Model) hint() string {
	if !m.narrow() && ansi.StringWidth(hintWide) <= m.w {
		return hintWide
	}
	return hintNarrow
}

// wrap breaks s at spaces into lines of at most w columns, indenting every
// line after the first. A word longer than a line is cut.
func wrap(s string, w int, indent string) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(s) {
		for ansi.StringWidth(word) > w-len(indent) && w > len(indent) {
			if cur != "" && cur != indent {
				out = append(out, cur)
				cur = indent
			}
			head := ansi.Truncate(word, w-len(cur), "")
			if head == "" {
				// Not even one rune fits (a double-width one at width 3):
				// take it anyway so the word shrinks. fit cuts the line.
				head = string([]rune(word)[:1])
			}
			out = append(out, cur+head)
			word = word[len(head):]
			cur = indent
		}
		switch {
		case cur == "" || cur == indent:
			cur += word
		case ansi.StringWidth(cur)+1+ansi.StringWidth(word) <= w:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = indent + word
		}
	}
	if strings.TrimSpace(cur) != "" {
		out = append(out, cur)
	}
	return out
}

func (m *Model) helpLines() []line {
	var out []line
	add := func(s string) {
		for _, l := range wrap(s, m.w, "    ") {
			out = append(out, line{text: m.fit(l)})
		}
	}
	blank := func() { out = append(out, line{}) }

	add("Keys act on the selected EVSE.")
	blank()
	for _, k := range [][2]string{
		{"↑ ↓  k j", "select an EVSE"},
		{"r", "reserve it for the default tag (auth.default_id_tag), 15 minutes"},
		{"c", "cancel its reservation"},
		{"s", "start a session with the default tag"},
		{"x", "stop its session (asks first)"},
		{"u", "unlock the cable"},
		{"t", "ask the station for its status again"},
		{"?", "this help"},
		{"q  ctrl-c", "quit cpms"},
	} {
		// The description wraps under itself, not under the key.
		for i, l := range wrap(k[1], m.w-keyCol, "") {
			lead := strings.Repeat(" ", keyCol)
			if i == 0 {
				lead = pad(k[0], keyCol)
			}
			out = append(out, line{text: m.fit(lead + l)})
		}
	}
	blank()
	add("Other tags, expiry, limits: run the CLI in another terminal, for example:")
	// The example keeps to a line of its own; at a narrow width it wraps under
	// itself, which is better than cutting the end off.
	for _, l := range wrap("cpms reserve --evse <uid> --tag <tag> --expires 30m", m.w-2, "  ") {
		out = append(out, line{text: m.fit("  " + l)})
	}
	if m.opts.LogPath != "" {
		blank()
		out = append(out, m.plain("Full log:"))
		// Whole, and on its own line, so it can be copied. If the screen is too
		// narrow the start goes: the file name is what says where to look.
		path := m.opts.LogPath
		if room := m.w - 2; ansi.StringWidth(path) > room && room > 1 {
			path = ansi.TruncateLeft(path, ansi.StringWidth(path)-(room-1), "…")
		}
		out = append(out, m.plain("  "+path))
	}
	for len(out) < m.h-1 {
		out = append(out, line{})
	}
	return append(out, m.plain("? or esc closes"))
}
