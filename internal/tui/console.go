package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

// Bus is what the console needs from a connected client. *client.Client
// implements it; tests use a fake.
type Bus interface {
	Agents() []a2a.Presence
	Send(ctx context.Context, to string, body any, opts ...client.MsgOption) error
	Request(ctx context.Context, to string, body any, opts ...client.MsgOption) (*a2a.Envelope, error)
	RequestTask(ctx context.Context, capability string, body any, opts ...client.MsgOption) (*a2a.Envelope, error)
	Emit(ctx context.Context, name string, body any, opts ...client.MsgOption) error
}

// Tab is one page of the console.
type Tab int

// The console's tabs, in order.
const (
	TabOverview Tab = iota
	TabAgents
	TabTraffic
	TabCompose
	TabInbox
	TabConfig
	TabMap
	numTabs
)

var tabNames = [numTabs]string{"Overview", "Agents", "Traffic", "Compose", "Inbox", "Config", "Map"}

// ConsoleOptions configures a console.
type ConsoleOptions struct {
	Agent string // who we're connected as
	URL   string // broker URL
	Scope string // what traffic is visible, e.g. "all agent traffic (observer)"
	Bus   Bus
	// Traffic is every message the connection can see.
	Traffic <-chan Item
	// Status carries broker status snapshots; nil when this identity isn't
	// an observer.
	Status <-chan broker.Status
	// Inbox delivers messages sent to this agent, with a way to answer
	// requests. Nil hides the Inbox tab's answering.
	Inbox *Answerer
	// Config opens the config editor; nil when the broker config file
	// isn't on this machine.
	Config func() (*ConfigModel, error)
	// Joining lists the agent IDs with a join request waiting for the
	// operator; nil when the broker config file isn't on this machine.
	Joining func() []string
	Start   Tab
	// Env names the AGENTBUS_* variables the connection settings came
	// from, shown in the header so a stale one is noticed.
	Env []string
}

// Console is the full-screen management console: broker overview,
// agents, live traffic, compose, inbox, config and a live map of the bus,
// one tab each.
//
// 1-7 or [ and ] switch tabs, ? shows every key, q quits. While a text
// field has focus, keys go to it; esc leaves the field.
type Console struct {
	opt  ConsoleOptions
	tab  Tab
	help bool

	// Shared state, fed by background messages whatever tab is showing.
	st, prev   broker.Status
	haveStatus bool
	statusAt   time.Time
	rates      []Rates // throughput history, newest last
	cards      []a2a.Presence
	closed     bool
	now        time.Time

	overview overviewTab
	agents   agentsTab
	traffic  trafficTab
	compose  composeTab
	inbox    inboxTab
	bus      mapTab
	config   *ConfigModel
	cfgErr   error
	joining  map[string]bool // join requests already announced

	inspect *Inspector

	flash    string
	flashErr bool
	flashAt  time.Time

	width, height int
}

// NewConsole returns a console with the given options.
func NewConsole(opt ConsoleOptions) *Console {
	m := &Console{opt: opt, tab: opt.Start, width: 120, height: 32, now: time.Now()}
	if opt.Bus != nil {
		m.cards = opt.Bus.Agents()
	}
	m.compose = newComposeTab()
	m.inbox = newInboxTab()
	if m.tab == TabConfig {
		m.openConfig("")
	}
	return m
}

// consoleTickMsg refreshes presence, elapsed times and flash messages.
type consoleTickMsg time.Time

func consoleTick() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return consoleTickMsg(t) })
}

// InboxMsg delivers one inbox entry to a running console.
type InboxMsg struct{ Entry *InboxEntry }

func waitForInbox(a *Answerer) tea.Cmd {
	if a == nil {
		return nil
	}
	return func() tea.Msg { return InboxMsg{<-a.c} }
}

func (m *Console) Init() tea.Cmd {
	cmds := []tea.Cmd{consoleTick(), tea.RequestBackgroundColor, waitForInbox(m.opt.Inbox), m.startMap()}
	if m.opt.Traffic != nil {
		cmds = append(cmds, waitForItem(m.opt.Traffic))
	}
	if m.opt.Status != nil {
		cmds = append(cmds, waitForStatus(m.opt.Status))
	}
	return tea.Batch(cmds...)
}

// bodySize is the area a tab draws in: everything but the two header
// lines and the two footer lines.
func (m *Console) bodySize() (int, int) {
	return max(m.width, 40), max(m.height-4, 6)
}

func (m *Console) say(err bool, format string, a ...any) {
	m.flash, m.flashErr, m.flashAt = fmt.Sprintf(format, a...), err, time.Now()
}

func (m *Console) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if bg, ok := msg.(tea.BackgroundColorMsg); ok {
		SetDark(bg.IsDark())
		m.compose.restyle()
		if m.config != nil {
			m.config.Update(msg)
		}
		return m, nil
	}
	// Background work continues whatever is on screen.
	switch msg := msg.(type) {
	case ItemMsg:
		m.addItem(Item(msg))
		return m, waitForItem(m.opt.Traffic)
	case sourceClosedMsg:
		m.closed = true
		return m, nil
	case StatusMsg:
		m.prev, m.st, m.haveStatus, m.statusAt = m.st, broker.Status(msg), true, time.Now()
		if !m.prev.Time.IsZero() {
			m.rates = append(m.rates, RatesBetween(m.prev, m.st))
			if len(m.rates) > 240 {
				m.rates = m.rates[len(m.rates)-240:]
			}
		}
		m.agents.reseat(m.rows())
		return m, waitForStatus(m.opt.Status)
	case statusClosedMsg:
		return m, nil
	case consoleTickMsg:
		m.now = time.Time(msg)
		if m.opt.Bus != nil {
			m.cards = m.opt.Bus.Agents()
		}
		m.agents.reseat(m.rows())
		if m.flash != "" && time.Since(m.flashAt) > 6*time.Second {
			m.flash = ""
		}
		m.noteJoining()
		if m.config != nil {
			m.config.Update(pendingTickMsg{}) // the console's tick drives the editor's
		}
		return m, consoleTick()
	case InboxMsg:
		m.inbox.add(msg.Entry)
		m.compose.noteReply(msg.Entry.Item.Env)
		if msg.Entry.pending() {
			m.say(false, "request from %s waiting in the Inbox tab (5)", Clean(msg.Entry.Item.Env.From))
		}
		return m, waitForInbox(m.opt.Inbox)
	case composeDoneMsg:
		m.compose.done(msg)
		return m, nil
	case mapTickMsg:
		m.bus.ticking = false
		return m, m.startMap()
	case CloseMsg: // the embedded config editor wants to leave
		m.tab = TabOverview
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		w, h := m.bodySize()
		if m.inspect != nil {
			m.inspect.Resize(w, h)
		}
		m.compose.resize(w, h)
		m.inbox.resize(w, h)
		if m.config != nil {
			m.config.Update(tea.WindowSizeMsg{Width: w, Height: h})
		}
		return m, nil
	case formDoneMsg:
		if m.config != nil {
			_, cmd := m.config.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	k, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		// Cursor blinks and other widget messages go to whatever has focus.
		return m, m.forward(msg)
	}
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.help {
		m.help = false
		return m, nil
	}
	if m.inspect != nil {
		done, cmd := m.inspect.Update(msg)
		if done {
			m.inspect = nil
		}
		return m, cmd
	}
	if m.capturing() {
		return m, m.forward(msg)
	}
	switch s := k.String(); s {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "]", "ctrl+right":
		return m, m.switchTab((m.tab + 1) % numTabs)
	case "[", "ctrl+left":
		return m, m.switchTab((m.tab + numTabs - 1) % numTabs)
	case "1", "2", "3", "4", "5", "6", "7":
		return m, m.switchTab(Tab(s[0] - '1'))
	}
	return m, m.forward(msg)
}

// capturing reports whether the current tab is taking every key (a text
// field or form has focus), so the console's own keys don't apply.
func (m *Console) capturing() bool {
	switch m.tab {
	case TabTraffic:
		return m.traffic.editing
	case TabCompose:
		return m.compose.editing
	case TabInbox:
		return m.inbox.editing()
	case TabConfig:
		return m.config != nil && m.config.mode != modeList
	}
	return false
}

func (m *Console) switchTab(t Tab) tea.Cmd {
	m.tab = t
	switch t {
	case TabConfig:
		if m.config == nil {
			m.openConfig("")
		}
	case TabInbox:
		m.inbox.seen = len(m.inbox.entries)
	case TabMap:
		return m.startMap()
	}
	return nil
}

// noteJoining announces join requests that arrived since the last tick.
func (m *Console) noteJoining() {
	if m.opt.Joining == nil {
		return
	}
	seen := map[string]bool{}
	for _, id := range m.opt.Joining() {
		seen[id] = true
		if !m.joining[id] {
			m.say(false, "%s asked to join: approve or deny it in the Config tab (%d), under Join requests", Clean(id), int(TabConfig)+1)
		}
	}
	m.joining = seen
}

// openConfig creates the embedded config editor, focused on id.
func (m *Console) openConfig(id string) {
	if m.opt.Config == nil {
		return
	}
	if m.config == nil {
		cfg, err := m.opt.Config()
		if err != nil {
			m.cfgErr = err
			return
		}
		cfg.Embedded, cfg.Tabbed = true, true
		w, h := m.bodySize()
		cfg.Update(tea.WindowSizeMsg{Width: w, Height: h})
		m.config = cfg
	}
	if id != "" {
		m.config.Focus(id)
	}
}

// forward sends msg to the current tab.
func (m *Console) forward(msg tea.Msg) tea.Cmd {
	switch m.tab {
	case TabOverview:
		return nil
	case TabAgents:
		return m.agentsUpdate(msg)
	case TabTraffic:
		return m.trafficUpdate(msg)
	case TabCompose:
		return m.composeUpdate(msg)
	case TabInbox:
		return m.inboxUpdate(msg)
	case TabConfig:
		if m.config == nil {
			return nil
		}
		if k, ok := msg.(tea.KeyPressMsg); ok && m.config.mode == modeList && (k.String() == "esc" || k.String() == "q") {
			return nil // the console owns leaving
		}
		_, cmd := m.config.Update(msg)
		return cmd
	case TabMap:
		return m.mapUpdate(msg)
	}
	return nil
}

// openInspector shows it over the current tab.
func (m *Console) openInspector(it Item) {
	w, h := m.bodySize()
	m.inspect = NewInspector(it, w, h)
}

func (m *Console) View() tea.View { return fullScreen(m.render()) }

func (m *Console) render() string {
	w, h := m.bodySize()
	header := m.renderTabs(w) + "\n" + m.renderConnection(w)

	var body string
	switch {
	case m.help:
		body = m.renderHelp(w, h)
	case m.inspect != nil:
		body = m.inspect.View()
	default:
		switch m.tab {
		case TabOverview:
			body = m.renderOverview(w, h)
		case TabAgents:
			body = m.renderAgents(w, h)
		case TabTraffic:
			body = m.renderTraffic(w, h)
		case TabCompose:
			body = m.renderCompose(w, h)
		case TabInbox:
			body = m.renderInbox(w, h)
		case TabConfig:
			body = m.renderConfig(w, h)
		case TabMap:
			body = m.renderMap(w, h)
		}
	}
	body = fitBlock(body, w, h)

	flash := ""
	if m.flash != "" {
		style, mark := OKStyle, "✓ "
		if m.flashErr {
			style, mark = ErrStyle, "✗ "
		}
		flash = " " + style.Render(mark+m.flash)
	}
	help := m.helpLine()
	if m.inspect != nil {
		help = "" // the inspector draws its own
	}
	return header + "\n" + body + "\n" + ansi.Truncate(flash, w, "…") + "\n" +
		HelpStyle.Render(ansi.Truncate(help, w-1, "…"))
}

// fitBlock pads or cuts s to exactly h lines, each at most w cells.
func fitBlock(s string, w, h int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			lines[i] = ansi.Truncate(l, w, "…")
		}
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m *Console) renderTabs(w int) string {
	parts := []string{TitleStyle.Render("agentbus")}
	for t := range numTabs {
		name := fmt.Sprintf("%d %s", t+1, tabNames[t])
		switch t {
		case TabInbox:
			if n := m.inbox.waiting(); n > 0 {
				name += fmt.Sprintf(" (%d)", n)
			} else if n := len(m.inbox.entries) - m.inbox.seen; n > 0 && m.tab != TabInbox {
				name += fmt.Sprintf(" +%d", n)
			}
		case TabCompose:
			if n := m.compose.inFlight(); n > 0 {
				name += fmt.Sprintf(" (%d)", n)
			}
		}
		if t == m.tab {
			parts = append(parts, AccentStyle.Underline(true).Render(name))
		} else if t == TabInbox && m.inbox.waiting() > 0 {
			parts = append(parts, WarnStyle.Render(name))
		} else {
			parts = append(parts, SubtleStyle.Render(name))
		}
	}
	return ansi.Truncate(strings.Join(parts, "  "), w, "…")
}

func (m *Console) renderConnection(w int) string {
	line := SubtleStyle.Render(" as ") + AccentStyle.Render(Clean(m.opt.Agent))
	if len(m.opt.Env) > 0 {
		line += WarnStyle.Render(" (from " + Clean(strings.Join(m.opt.Env, ", ")) + ")")
	}
	line += SubtleStyle.Render(" · " + Clean(m.opt.URL) + " · " + m.opt.Scope)
	if m.haveStatus {
		line += SubtleStyle.Render(" · " + BrokerSummary(m.st))
		if age := m.now.Sub(m.statusAt); age > 10*time.Second {
			line += WarnStyle.Render(fmt.Sprintf(" · no status for %s", age.Round(time.Second)))
		}
	}
	if m.closed {
		line += ErrStyle.Render(" · disconnected")
	}
	return ansi.Truncate(line, w, "…")
}

func (m *Console) helpLine() string {
	global := "1-7 tabs · ? keys · q quit"
	switch m.tab {
	case TabOverview:
		return global
	case TabAgents:
		return m.agentsHelp() + " · " + global
	case TabTraffic:
		if m.traffic.editing {
			return "type to filter · enter apply · esc cancel"
		}
		return m.trafficHelp() + " · " + global
	case TabCompose:
		if m.compose.editing {
			return "tab/↓ next field · shift+tab/↑ back · ←/→ change choice · ctrl+s send · esc leave form"
		}
		return "e edit form · ↑/↓ select · enter inspect reply · R reuse · x cancel · " + global
	case TabInbox:
		if m.inbox.editing() {
			return m.inbox.editHelp()
		}
		return m.inboxHelp() + " · " + global
	case TabConfig:
		if m.config == nil {
			return global
		}
		if m.config.mode != modeList {
			return "" // the editor shows its own help
		}
		return global
	case TabMap:
		return m.mapHelp() + " · " + global
	}
	return global
}

func (m *Console) renderHelp(w, h int) string {
	type section struct {
		name string
		keys []string
	}
	sections := []section{
		{"Everywhere", []string{"1-7 or [ ]  switch tab", "?  this help", "q  quit (ctrl+c works anywhere)", "esc  step back: leave a field, close the inspector, clear a filter"}},
		{"Agents", []string{"↑/↓  select", "enter  detail", "i  inspect presence card", "r / t / s  request, task or send to the agent", "f  show its traffic", "e  edit in config"}},
		{"Traffic", []string{"↑/↓  select", "enter  inspect", "/  filter by text", "t  filter by type", "R  copy a message into Compose", "p  pause · c clear"}},
		{"Compose", []string{"tab  next field", "←/→  change kind or priority", "ctrl+s  send", "esc  leave the form; then ↑/↓ and enter on results"}},
		{"Inbox", []string{"a  answer a request", "x  refuse it", "h  hold requests for you, or refuse them at once", "enter  inspect"}},
		{"Config", []string{"the config editor: agents, tokens, client setup, broker settings"}},
		{"Map", []string{"each message drawn moving through the broker", "p  pause · c clear"}},
	}
	render := func(ss []section) string {
		var b strings.Builder
		for i, s := range ss {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(AccentStyle.Render(s.name) + "\n")
			for _, k := range s.keys {
				b.WriteString("  " + TextStyle.Render(k) + "\n")
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	head := AccentStyle.Render("Keys") + SubtleStyle.Render("  any key to close") + "\n\n"
	if w < 100 {
		return head + render(sections)
	}
	left := lipgloss.NewStyle().Width(w / 2).Render(render(sections[:3]))
	return head + lipgloss.JoinHorizontal(lipgloss.Top, left, render(sections[3:]))
}

// panelRows renders rows in a bordered panel of total width w, each row
// cut to fit.
func panelRows(w int, rows []string) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = ansi.Truncate(r, max(1, w-panelPadX), "…")
	}
	return PanelStyle.Width(w).Render(strings.Join(out, "\n"))
}

// column is one column of a grid.
type column struct {
	title string
	width int // 0: take the rest
}

// grid renders a header and rows with a cursor, scrolled so the cursor
// stays visible, in height lines (header included). cells must already
// be cleaned of control characters; they may be styled.
func grid(cols []column, rows [][]string, cursor, width, height int, focused bool) []string {
	used := 2 + len(cols) - 1 // the cursor mark, and a space between columns
	flex := -1
	for i, c := range cols {
		if c.width == 0 {
			flex = i
			continue
		}
		used += c.width
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = c.width
		if i == flex {
			widths[i] = max(4, width-used)
		}
	}
	cell := func(s string, w int) string {
		s = ansi.Truncate(s, w, "…")
		return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
	}
	var head strings.Builder
	head.WriteString("  ")
	for i, c := range cols {
		if i > 0 {
			head.WriteString(" ")
		}
		head.WriteString(cell(c.title, widths[i]))
	}
	out := []string{SubtleStyle.Bold(true).Render(ansi.Truncate(head.String(), width, ""))}
	visible := max(1, height-1)
	first := 0
	if cursor >= visible {
		first = cursor - visible + 1
	}
	for i := first; i < len(rows) && i < first+visible; i++ {
		var line strings.Builder
		mark := "  "
		if i == cursor && focused {
			mark = AccentStyle.Render("▌ ")
		}
		line.WriteString(mark)
		for j := range cols {
			v := ""
			if j < len(rows[i]) {
				v = rows[i][j]
			}
			if j > 0 {
				line.WriteString(" ")
			}
			line.WriteString(cell(v, widths[j]))
		}
		out = append(out, ansi.Truncate(line.String(), width, "…"))
	}
	return out
}

// sparkline draws values as a row of block characters, scaled to the
// largest value, using the newest width values.
func sparkline(vals []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	top := 0.0
	for _, v := range vals {
		top = max(top, v)
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if top > 0 {
			i = min(len(bars)-1, int(v/top*float64(len(bars)-1)+0.5))
		}
		b.WriteRune(bars[i])
	}
	return b.String()
}

func (m *Console) renderConfig(w, h int) string {
	switch {
	case m.config != nil:
		return m.config.render()
	case m.cfgErr != nil:
		return AccentStyle.Render("Config") + "\n\n" + ErrStyle.Render("✗ "+Clean(m.cfgErr.Error()))
	}
	return AccentStyle.Render("Config") + "\n\n" +
		TextStyle.Render("The broker config file isn't on this machine, or it belongs to a different broker.") + "\n" +
		SubtleStyle.Render("Agents, tokens and settings are edited where the broker runs: agentbus config there,") + "\n" +
		SubtleStyle.Render("or start the console with --config pointing at this broker's broker.json.")
}
