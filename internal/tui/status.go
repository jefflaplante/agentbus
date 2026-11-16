package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
)

// StatusMsg delivers a broker status snapshot.
type StatusMsg broker.Status

type statusClosedMsg struct{}

func waitForStatus(ch <-chan broker.Status) tea.Cmd {
	return func() tea.Msg {
		st, ok := <-ch
		if !ok {
			return statusClosedMsg{}
		}
		return StatusMsg(st)
	}
}

// Rates are per-second message rates between two snapshots.
type Rates struct{ In, Out float64 }

// RatesBetween computes message rates from prev to cur.
func RatesBetween(prev, cur broker.Status) Rates {
	dt := cur.Time.Sub(prev.Time).Seconds()
	if dt <= 0 || prev.Time.IsZero() {
		return Rates{}
	}
	return Rates{
		In:  float64(cur.Traffic.MessagesReceived-prev.Traffic.MessagesReceived) / dt,
		Out: float64(cur.Traffic.MessagesSent-prev.Traffic.MessagesSent) / dt,
	}
}

// StatusModel is a live, top-like view of a running broker.
//
// ↑/↓ select an agent and enter opens its detail: what the broker knows
// about it, its presence card and its recent traffic, where ↑/↓ and enter
// inspect a message, i inspects the card and e opens the agent in the
// config editor.
type StatusModel struct {
	Source <-chan broker.Status
	// Agents, Traffic and EditAgent are optional: presence cards and live
	// traffic for the agent detail view, and the config editor (only when
	// the broker config file is on this machine).
	Agents    func() []a2a.Presence
	Traffic   <-chan Item
	EditAgent func(id string) tea.Model

	feed    feedState // all traffic seen, for agent details
	detail  string    // agent shown in the detail view; "" for the overview
	inspect *Inspector
	sub     tea.Model

	st     broker.Status
	prev   broker.Status
	have   bool
	closed bool
	table  table.Model
	width  int
	height int
}

// NewStatus returns a broker status view fed by src.
func NewStatus(src <-chan broker.Status) StatusModel {
	t := table.New(table.WithColumns(statusColumns(110)), table.WithFocused(true), table.WithHeight(8))
	restyleTable(&t)
	t.SetWidth(110) // bubbles v2 tables clip to their width; resized on WindowSizeMsg
	return StatusModel{Source: src, table: t}
}

func statusColumns(width int) []table.Column {
	type col struct {
		title       string
		wide, tight int
	}
	cols := []col{
		{"AGENT", 16, 12}, {"STATE", 8, 7}, {"REMOTE", 22, 15}, {"QUEUED", 7, 6},
		{"ACCEPTED", 9, 5}, {"REJECTED", 9, 5}, {"LAST SEEN", 14, 8},
	}
	tight := width < 110
	out := make([]table.Column, 0, len(cols)+1)
	used := (len(cols) + 1) * 2 // 1 cell of padding each side
	for _, c := range cols {
		w, title := c.wide, c.title
		if tight {
			w = c.tight
			title = map[string]string{"ACCEPTED": "ACC", "REJECTED": "REJ", "LAST SEEN": "SEEN"}[c.title]
			if title == "" {
				title = c.title
			}
		}
		used += w
		out = append(out, table.Column{Title: title, Width: w})
	}
	return append(out, table.Column{Title: "ROLE", Width: max(6, width-used)})
}

// AgentStatusRows renders the per-agent table rows.
func AgentStatusRows(agents []broker.AgentStatus) []table.Row {
	rows := make([]table.Row, 0, len(agents))
	for _, a := range agents {
		role := strings.Join(a.Capabilities, ",")
		if a.Observer {
			role = strings.TrimPrefix(role+",observer", ",")
		}
		if len(a.SendTo) > 0 && !slices.Contains(a.SendTo, "*") {
			role += " → " + strings.Join(a.SendTo, ",")
		}
		seen := ""
		if !a.LastSeen.IsZero() {
			seen = a.LastSeen.Local().Format("15:04:05")
		}
		rows = append(rows, table.Row{
			a.ID, a.State, a.Remote, fmt.Sprint(a.Queued), fmt.Sprint(a.Accepted), fmt.Sprint(a.Rejected), seen, role,
		})
	}
	return rows
}

func (m StatusModel) Init() tea.Cmd {
	cmds := []tea.Cmd{waitForStatus(m.Source), tea.RequestBackgroundColor}
	if m.Traffic != nil {
		cmds = append(cmds, waitForItem(m.Traffic))
	}
	return tea.Batch(cmds...)
}

func (m StatusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handleBackground(msg) && m.sub == nil {
		restyleTable(&m.table)
		return m, nil
	}
	switch msg := msg.(type) {
	case StatusMsg:
		m.prev, m.st, m.have = m.st, broker.Status(msg), true
		m.table.SetRows(AgentStatusRows(m.st.Agents))
		return m, waitForStatus(m.Source)
	case statusClosedMsg:
		m.closed = true
		return m, nil
	case ItemMsg:
		m.feed.add(Item(msg))
		return m, waitForItem(m.Traffic)
	case sourceClosedMsg:
		return m, nil
	case CloseMsg:
		m.sub = nil
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetColumns(statusColumns(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-18))
		if m.inspect != nil {
			m.inspect.Resize(m.width, m.height-2)
		}
		if m.sub != nil {
			var cmd tea.Cmd
			m.sub, cmd = m.sub.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	if m.sub != nil {
		var cmd tea.Cmd
		m.sub, cmd = m.sub.Update(msg)
		return m, cmd
	}
	if m.inspect != nil {
		if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "ctrl+c" {
			return m, tea.Quit
		}
		done, cmd := m.inspect.Update(msg)
		if done {
			m.inspect = nil
		}
		return m, cmd
	}
	k, isKey := msg.(tea.KeyPressMsg)
	if m.detail != "" && isKey {
		return m.detailKey(k)
	}
	if isKey {
		switch k.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "enter":
			if a, ok := m.selectedAgent(); ok {
				m.detail = a.ID
				id := a.ID
				m.feed.setFilter(id, func(it Item) bool { return involves(it, id) })
			}
			return m, nil
		case "e":
			if a, ok := m.selectedAgent(); ok {
				return m, m.openEditor(a.ID)
			}
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m *StatusModel) openEditor(id string) tea.Cmd {
	if m.EditAgent == nil {
		return nil
	}
	m.sub = m.EditAgent(id)
	if m.sub == nil {
		return nil
	}
	w, h := m.width, m.height
	return tea.Batch(m.sub.Init(), func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} })
}

func (m StatusModel) selectedAgent() (broker.AgentStatus, bool) {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.st.Agents) {
		return broker.AgentStatus{}, false
	}
	return m.st.Agents[i], true
}

func (m StatusModel) card(id string) (a2a.Presence, bool) {
	if m.Agents == nil {
		return a2a.Presence{}, false
	}
	for _, p := range m.Agents() {
		if p.Agent == id {
			return p, true
		}
	}
	return a2a.Presence{}, false
}

func (m StatusModel) detailKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc", "backspace", "left":
		if !m.feed.following() && k.String() == "esc" {
			m.feed.selSeq = 0
			return m, nil
		}
		m.detail = ""
		m.feed.setFilter("", nil)
	case "enter":
		if it, ok := m.feed.selected(); ok {
			m.inspect = NewInspector(it, m.width, m.height-2)
		}
	case "i":
		if p, ok := m.card(m.detail); ok {
			m.inspect = NewInspector(Item{Topic: a2a.PresenceTopic(p.Agent), Presence: &p}, m.width, m.height-2)
		}
	case "e":
		return m, m.openEditor(m.detail)
	default:
		m.feed.feedKeys(k.String())
	}
	return m, nil
}

func (m StatusModel) View() tea.View {
	if m.sub != nil {
		return m.sub.View()
	}
	return fullScreen(m.render())
}

// renderDetail is the agent detail view.
func (m StatusModel) renderDetail(width int) string {
	var as broker.AgentStatus
	for _, a := range m.st.Agents {
		if a.ID == m.detail {
			as = a
		}
	}
	header := TitleStyle.Render("agentbus · agent "+m.detail) + " " + StatusStyle(a2a.Status(as.State)).Render(as.State)
	if as.Remote != "" {
		header += SubtleStyle.Render("  " + as.Remote)
	}
	if m.inspect != nil {
		return header + "\n\n" + m.inspect.View()
	}
	kv := func(k, v string) string { return SubtleStyle.Render(fmt.Sprintf("%-13s", k)) + TextStyle.Render(v) }
	role := strings.Join(as.Capabilities, ",")
	if role == "" {
		role = "none"
	}
	send := "anyone"
	if len(as.SendTo) > 0 && !slices.Contains(as.SendTo, "*") {
		send = strings.Join(as.SendTo, ", ")
	}
	seen := "never"
	if !as.LastSeen.IsZero() {
		seen = as.LastSeen.Local().Format("15:04:05") + " (" + since(as.LastSeen) + " ago)"
	}
	broker := []string{AccentStyle.Render("Broker"),
		kv("state", as.State), kv("queued", fmt.Sprint(as.Queued)),
		kv("accepted", OKStyle.Render(fmt.Sprint(as.Accepted))), kv("rejected", fmt.Sprint(as.Rejected)),
		kv("last seen", seen), kv("serves", role), kv("may send to", send), kv("observer", fmt.Sprint(as.Observer)),
	}
	card := []string{AccentStyle.Render("Presence card")}
	if p, ok := m.card(m.detail); ok {
		keys := "none"
		if p.Keys != nil {
			keys = "claimed"
			if p.KeysPinned {
				keys = "pinned"
			}
		}
		card = append(card, kv("status", StatusStyle(p.Status).Render(string(p.Status))+SubtleStyle.Render(" since "+p.TS.Local().Format("15:04:05"))),
			kv("description", p.Description), kv("capabilities", strings.Join(p.Capabilities, ",")), kv("keys", keys),
			kv("meta", fmt.Sprintf("%d entries", len(p.Meta))))
	} else {
		card = append(card, SubtleStyle.Render("no card: the agent hasn't announced itself"))
	}
	col := width / 2
	panel := func(rows []string) string {
		for i, r := range rows {
			rows[i] = ansi.Truncate(r, col-panelPadX, "…")
		}
		return PanelStyle.Width(col).Render(strings.Join(rows, "\n"))
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, panel(broker), panel(card))

	traffic := AccentStyle.Render("Recent traffic") + SubtleStyle.Render(fmt.Sprintf("  %d messages involving %s", len(m.feed.visible()), m.detail))
	lines := m.feed.lines(width, max(3, m.height-lipgloss.Height(top)-6))
	if len(lines) == 0 {
		lines = []string{SubtleStyle.Render("  none seen since this view opened")}
		if m.Traffic == nil {
			lines = []string{SubtleStyle.Render("  traffic needs an observer connection")}
		}
	}
	help := "↑/↓ select · enter inspect · i inspect card"
	if m.EditAgent != nil {
		help += " · e edit in config"
	}
	help += " · esc back"
	return header + "\n" + top + "\n" + traffic + "\n" + strings.Join(lines, "\n") + "\n" + HelpStyle.Render(ansi.Truncate(help, width-1, "…"))
}

func (m StatusModel) render() string {
	if m.detail != "" && m.have {
		width := m.width
		if width <= 0 {
			width = 120
		}
		return m.renderDetail(width)
	}
	if !m.have {
		msg := "waiting for broker status… (needs an observer identity)"
		if m.closed {
			msg = "disconnected before any status arrived"
		}
		return TitleStyle.Render("agentbus · broker") + "\n\n" + SubtleStyle.Render(msg) + "\n" + HelpStyle.Render("q quit")
	}
	width := m.width
	if width <= 0 {
		width = 120 // before the first WindowSizeMsg
	}
	st := m.st
	up := (time.Duration(st.Uptime) * time.Second).String()
	header := TitleStyle.Render("agentbus · broker") + " " + AccentStyle.Render(st.Listen) +
		SubtleStyle.Render(fmt.Sprintf("  up %s · %s · mochi %s", up, st.Version, st.Mochi))
	header = ansi.Truncate(header, max(width, 20), "…")
	if m.closed {
		header += ErrStyle.Render("  disconnected")
	}

	col := width / 3                       // total panel width, chrome included
	valueWidth := max(8, col-panelPadX-11) // minus the key column
	kv := func(k, v string) string { return SubtleStyle.Render(fmt.Sprintf("%-11s", k)) + TextStyle.Render(v) }
	onOff := func(on bool, v string) string {
		if !on {
			return SubtleStyle.Render("off")
		}
		return OKStyle.Render(tailTrunc(v, valueWidth))
	}
	server := []string{AccentStyle.Render("Server"),
		kv("tls", onOff(st.TLS, "on")),
		kv("data_dir", onOff(st.DataDir != "", st.DataDir)),
		kv("audit", onOff(st.AuditFile != "", st.AuditFile)),
		kv("started", st.Started.Local().Format("Jan 02 15:04:05")),
		kv("memory", fmt.Sprintf("%.1f MiB · %d goroutines", float64(st.Traffic.MemoryAlloc)/(1<<20), st.Traffic.Goroutines)),
	}

	r := RatesBetween(m.prev, st)
	t := st.Traffic
	traffic := []string{AccentStyle.Render("Traffic"),
		kv("clients", fmt.Sprintf("%d connected · %d sessions", t.ClientsConnected, t.Sessions)),
		kv("messages", fmt.Sprintf("%d in · %d out", t.MessagesReceived, t.MessagesSent)),
		kv("rate", fmt.Sprintf("%.1f/s in · %.1f/s out", r.In, r.Out)),
		kv("bytes", fmt.Sprintf("%s in · %s out", humanBytes(t.BytesReceived), humanBytes(t.BytesSent))),
		kv("queued", fmt.Sprintf("%d in flight · %d dropped", t.Inflight, t.InflightDropped)),
		kv("store", fmt.Sprintf("%d subs · %d retained", t.Subscriptions, t.Retained)),
	}

	d := st.Decisions
	var rejects []string
	codes := make([]string, 0, len(d.Rejected))
	for c := range d.Rejected {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	var totalRej int64
	for _, c := range codes {
		totalRej += d.Rejected[c]
		rejects = append(rejects, fmt.Sprintf("%s %d", c, d.Rejected[c]))
	}
	rejLine := SubtleStyle.Render("none")
	if len(rejects) > 0 {
		rejLine = ErrStyle.Render(fmt.Sprint(totalRej)) + SubtleStyle.Render("  "+strings.Join(rejects, " · "))
	}
	decisions := []string{AccentStyle.Render("Decisions"),
		kv("accepted", OKStyle.Render(fmt.Sprint(d.Accepted))),
		kv("duplicates", fmt.Sprint(d.Duplicates)),
		kv("rejected", rejLine),
		kv("auth fails", fmt.Sprint(d.AuthFailures)),
		kv("wills", fmt.Sprintf("%d refused", d.WillsRefused)),
	}

	l := st.Limits
	limits := SubtleStyle.Render(fmt.Sprintf("limits: %s/envelope · %.0f/s burst %d · queue %d · session %s · skew %s · conns %d (%d/ip)",
		humanBytes(int64(l.MaxEnvelopeBytes)), l.RatePerSecond, l.RateBurst, l.MaxQueued, l.SessionExpiry,
		l.MaxClockSkew, l.MaxConnections, l.MaxConnectionsPerIP))

	panel := func(rows []string) string {
		for i, r := range rows {
			rows[i] = ansi.Truncate(r, col-panelPadX, "…")
		}
		return PanelStyle.Width(col).Render(strings.Join(rows, "\n"))
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, panel(server), panel(traffic), panel(decisions))
	limits = ansi.Truncate(limits, width, "…")
	helpText := "↑/↓ select · enter details"
	if m.EditAgent != nil {
		helpText += " · e edit in config"
	}
	help := HelpStyle.Render(helpText + " · q quit · updated " + st.Time.Local().Format("15:04:05"))
	return header + "\n" + top + "\n" + limits + "\n\n" + m.table.View() + "\n" + help
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// BrokerSummary is a one-line broker summary for the dashboard header.
func BrokerSummary(st broker.Status) string {
	var rej int64
	for _, n := range st.Decisions.Rejected {
		rej += n
	}
	return fmt.Sprintf("broker up %s · %d clients · %d accepted · %d rejected · %d queued",
		time.Duration(st.Uptime)*time.Second, st.Traffic.ClientsConnected, st.Decisions.Accepted, rej, st.Traffic.Inflight)
}

// tailTrunc keeps the end of s (the useful part of a path) within width.
func tailTrunc(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return "…" + string(r[len(r)-width+1:])
}
