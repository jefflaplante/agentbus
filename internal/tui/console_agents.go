package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
)

// overviewTab has no state of its own: it draws the broker status.
type overviewTab struct{}

func (m *Console) renderOverview(w, h int) string {
	if !m.haveStatus {
		on, off := Counts(m.cards)
		msg := []string{
			AccentStyle.Render("Broker status"),
			"",
			TextStyle.Render(fmt.Sprintf("%d agents online, %d offline (from presence cards)", on, off)),
			"",
		}
		if m.opt.Status == nil {
			msg = append(msg,
				SubtleStyle.Render("Broker status and all-traffic monitoring need an observer identity."),
				SubtleStyle.Render(`Connect as one, for example --agent admin, or set "observer": true for this agent`),
				SubtleStyle.Render("in the Config tab. Agents, Compose and Inbox work without it."))
		} else {
			msg = append(msg, SubtleStyle.Render("waiting for the first status snapshot…"))
		}
		return strings.Join(msg, "\n")
	}
	st := m.st
	col := w / 3
	valueWidth := max(8, col-panelPadX-11)
	kv := func(k, v string) string { return SubtleStyle.Render(fmt.Sprintf("%-11s", k)) + TextStyle.Render(v) }
	onOff := func(on bool, v string) string {
		if !on {
			return SubtleStyle.Render("off")
		}
		return OKStyle.Render(tailTrunc(Clean(v), valueWidth))
	}
	up := (time.Duration(st.Uptime) * time.Second).String()
	server := []string{AccentStyle.Render("Server"),
		kv("listen", Clean(st.Listen)),
		kv("version", Clean(st.Version)+" · up "+up),
		kv("tls", onOff(st.TLS, "on")),
		kv("data_dir", onOff(st.DataDir != "", st.DataDir)),
		kv("audit", onOff(st.AuditFile != "", st.AuditFile)),
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
	codes := make([]string, 0, len(d.Rejected))
	for c := range d.Rejected {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	var totalRej int64
	var rejects []string
	for _, c := range codes {
		totalRej += d.Rejected[c]
		rejects = append(rejects, fmt.Sprintf("%s %d", Clean(c), d.Rejected[c]))
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
		kv("requests", requestSummary(st.Requests)),
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, panelRows(col, server), panelRows(col, traffic), panelRows(col, decisions))

	l := st.Limits
	limits := SubtleStyle.Render(fmt.Sprintf("limits: %s/envelope · %.0f/s burst %d · queue %d · session %s · skew %s · conns %d (%d/ip)",
		humanBytes(int64(l.MaxEnvelopeBytes)), l.RatePerSecond, l.RateBurst, l.MaxQueued, l.SessionExpiry,
		l.MaxClockSkew, l.MaxConnections, l.MaxConnectionsPerIP))

	// Throughput history: one bar per status snapshot.
	spark := max(10, w-24)
	var in, out []float64
	var peak float64
	for _, r := range m.rates {
		in, out = append(in, r.In), append(out, r.Out)
		peak = max(peak, r.In, r.Out)
	}
	every := "status_interval"
	switch n := len(m.rates); {
	case n == 1:
		every = "snapshot"
	case n > 1:
		every = fmt.Sprintf("%d snapshots", n)
	}
	lines := []string{top, limits, "",
		AccentStyle.Render("Throughput") + SubtleStyle.Render(fmt.Sprintf("  peak %.1f/s over the last %s", peak, every)),
		SubtleStyle.Render(" in  ") + OKStyle.Render(sparkline(in, spark)),
		SubtleStyle.Render(" out ") + AccentStyle.Render(sparkline(out, spark)),
		"",
	}

	used := lipgloss.Height(strings.Join(lines, "\n"))
	reqs := slices.Clone(st.Requests)
	slices.SortFunc(reqs, func(a, b broker.RequestStats) int { return strings.Compare(a.Target, b.Target) })
	title := AccentStyle.Render("Requests by target")
	if len(reqs) == 0 {
		lines = append(lines, title+SubtleStyle.Render("  none yet"))
		return strings.Join(lines, "\n")
	}
	lines = append(lines, title)
	rows := make([][]string, 0, len(reqs))
	for _, r := range reqs {
		exp := fmt.Sprint(r.Expired)
		if r.Expired > 0 {
			exp = ErrStyle.Render(exp)
		}
		rows = append(rows, []string{Clean(r.Target), fmt.Sprint(r.Open), fmt.Sprint(r.Answered), fmt.Sprint(r.Errored),
			exp, fmt.Sprint(r.Evicted), latency(r.P50), latency(r.P95)})
	}
	cols := []column{{"TARGET", 0}, {"OPEN", 6}, {"ANSWERED", 9}, {"ERRORED", 8}, {"EXPIRED", 8}, {"EVICTED", 8}, {"P50", 8}, {"P95", 8}}
	lines = append(lines, grid(cols, rows, -1, w, max(2, h-used-1), false)...)
	return strings.Join(lines, "\n")
}

func latency(s float64) string {
	switch {
	case s <= 0:
		return "-"
	case s < 1:
		return fmt.Sprintf("≤%.0fms", s*1000)
	}
	return fmt.Sprintf("≤%.1fs", s)
}

// agentRow joins what the broker reports about an agent with its
// presence card; either may be missing.
type agentRow struct {
	id     string
	status *broker.AgentStatus
	card   *a2a.Presence
}

func (r agentRow) capabilities() []string {
	if r.status != nil && len(r.status.Capabilities) > 0 {
		return r.status.Capabilities
	}
	if r.card != nil {
		return r.card.Capabilities
	}
	return nil
}

func (r agentRow) state() string {
	switch {
	case r.status != nil:
		return r.status.State
	case r.card != nil:
		return string(r.card.Status)
	}
	return ""
}

// rows lists every agent known from status snapshots or presence.
func (m *Console) rows() []agentRow {
	byID := map[string]*agentRow{}
	var ids []string
	get := func(id string) *agentRow {
		if r, ok := byID[id]; ok {
			return r
		}
		r := &agentRow{id: id}
		byID[id] = r
		ids = append(ids, id)
		return r
	}
	if m.haveStatus {
		for i := range m.st.Agents {
			get(m.st.Agents[i].ID).status = &m.st.Agents[i]
		}
	}
	for i := range m.cards {
		get(m.cards[i].Agent).card = &m.cards[i]
	}
	slices.Sort(ids)
	out := make([]agentRow, 0, len(ids))
	for _, id := range ids {
		out = append(out, *byID[id])
	}
	return out
}

// agentsTab is a table of every agent, and a detail view of one.
type agentsTab struct {
	cursor int
	selID  string    // selected agent, so the cursor follows it as rows change
	detail string    // agent shown in detail; "" for the table
	feed   feedState // the detail's traffic: messages involving detail
}

// reseat moves the cursor to the selected agent after the rows change.
func (a *agentsTab) reseat(rows []agentRow) {
	if a.selID != "" {
		if i := slices.IndexFunc(rows, func(r agentRow) bool { return r.id == a.selID }); i >= 0 {
			a.cursor = i
			return
		}
	}
	a.setCursor(rows, a.cursor)
}

func (a *agentsTab) setCursor(rows []agentRow, i int) {
	a.cursor = max(0, min(i, len(rows)-1))
	a.selID = ""
	if a.cursor < len(rows) {
		a.selID = rows[a.cursor].id
	}
}

func (m *Console) selectedRow() (agentRow, bool) {
	rows := m.rows()
	if m.agents.detail != "" {
		for _, r := range rows {
			if r.id == m.agents.detail {
				return r, true
			}
		}
		return agentRow{id: m.agents.detail}, true
	}
	if m.agents.cursor < 0 || m.agents.cursor >= len(rows) {
		return agentRow{}, false
	}
	return rows[m.agents.cursor], true
}

func (m *Console) agentsUpdate(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	a := &m.agents
	rows := m.rows()
	r, have := m.selectedRow()
	switch s := k.String(); s {
	case "esc", "backspace", "left":
		if a.detail != "" {
			if !a.feed.following() && s == "esc" {
				a.feed.selSeq = 0
				return nil
			}
			a.detail = ""
		}
		return nil
	case "i":
		if have && r.card != nil {
			p := *r.card
			m.openInspector(Item{Topic: a2a.PresenceTopic(p.Agent), Presence: &p})
		} else if have {
			m.say(true, "%s has no presence card: it hasn't announced itself", Clean(r.id))
		}
		return nil
	case "r", "s":
		if have {
			kind := kindRequest
			if s == "s" {
				kind = kindSend
			}
			return m.composeTo(kind, r.id)
		}
		return nil
	case "t":
		if have {
			caps := r.capabilities()
			if len(caps) == 0 || caps[0] == "*" {
				m.say(true, "%s serves no task queue", Clean(r.id))
				return nil
			}
			return m.composeTo(kindTask, caps[0])
		}
		return nil
	case "f":
		if have {
			m.traffic.setAgent(r.id)
			return m.switchTab(TabTraffic)
		}
		return nil
	case "e":
		if !have {
			return nil
		}
		if m.opt.Config == nil {
			m.say(true, "the broker config isn't on this machine; edit it where the broker runs")
			return nil
		}
		m.openConfig(r.id)
		return m.switchTab(TabConfig)
	}
	if a.detail != "" {
		switch k.String() {
		case "enter":
			if it, ok := a.feed.selected(); ok {
				m.openInspector(it)
			}
		case "c":
			// The detail's traffic is a view of the shared feed: leave it.
		default:
			a.feed.feedKeys(k.String())
		}
		return nil
	}
	switch k.String() {
	case "up", "k":
		a.setCursor(rows, a.cursor-1)
	case "down", "j":
		a.setCursor(rows, a.cursor+1)
	case "pgup":
		a.setCursor(rows, a.cursor-10)
	case "pgdown":
		a.setCursor(rows, a.cursor+10)
	case "home", "g":
		a.setCursor(rows, 0)
	case "end", "G":
		a.setCursor(rows, len(rows)-1)
	case "enter", "right", "l":
		if have {
			a.detail = r.id
			a.feed = feedState{}
			for _, it := range m.traffic.feed.items {
				if involves(it, r.id) {
					a.feed.add(it)
				}
			}
		}
	}
	return nil
}

func (m *Console) agentsHelp() string {
	keys := "r request · t task · s send · f traffic · i card"
	if m.opt.Config != nil {
		keys += " · e edit"
	}
	if m.agents.detail != "" {
		return "↑/↓ select · enter inspect · " + keys + " · esc back"
	}
	return "↑/↓ select · enter detail · " + keys
}

func (m *Console) renderAgents(w, h int) string {
	if m.agents.detail != "" {
		return m.renderAgentDetail(w, h)
	}
	rows := m.rows()
	on, off := Counts(m.cards)
	head := AccentStyle.Render("Agents") + SubtleStyle.Render(fmt.Sprintf("  %d known · %d online · %d offline by presence", len(rows), on, off))
	if !m.haveStatus {
		head += SubtleStyle.Render(" · broker columns need an observer")
	}
	if len(rows) == 0 {
		return head + "\n\n" + SubtleStyle.Render("no agents seen yet…")
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		state := r.state()
		stateCell := StatusStyle(a2a.Status(state)).Render("● " + Clean(state))
		var remote, queued, acc, rej, seen string
		if s := r.status; s != nil {
			remote, queued, acc, rej = Clean(s.Remote), fmt.Sprint(s.Queued), fmt.Sprint(s.Accepted), fmt.Sprint(s.Rejected)
			if s.Rejected > 0 {
				rej = ErrStyle.Render(rej)
			}
			if !s.LastSeen.IsZero() {
				seen = s.LastSeen.Local().Format("15:04:05")
			}
		}
		role := Clean(strings.Join(r.capabilities(), ","))
		if r.status != nil && r.status.Observer {
			role = strings.TrimPrefix(role+",observer", ",")
		}
		desc := ""
		if r.card != nil {
			desc = Clean(r.card.Description)
		} else {
			desc = SubtleStyle.Render("no presence card")
		}
		cells = append(cells, []string{TextStyle.Render(Clean(r.id)), stateCell, role, remote, queued, acc, rej, seen, desc})
	}
	cols := []column{{"AGENT", 16}, {"STATE", 10}, {"SERVES", 16}, {"REMOTE", 21}, {"QUEUED", 6}, {"ACC", 6}, {"REJ", 5}, {"SEEN", 8}, {"DESCRIPTION", 0}}
	if w < 120 {
		cols = []column{{"AGENT", 14}, {"STATE", 10}, {"SERVES", 12}, {"QUEUED", 6}, {"REJ", 5}, {"DESCRIPTION", 0}}
		for i, c := range cells {
			cells[i] = []string{c[0], c[1], c[2], c[4], c[6], c[8]}
		}
	}
	return head + "\n\n" + strings.Join(grid(cols, cells, m.agents.cursor, w, h-2, true), "\n")
}

func (m *Console) renderAgentDetail(w, h int) string {
	r, _ := m.selectedRow()
	id := r.id
	head := AccentStyle.Render("Agent "+Clean(id)) + " " + StatusStyle(a2a.Status(r.state())).Render(Clean(r.state()))
	if r.status != nil && r.status.Remote != "" {
		head += SubtleStyle.Render("  " + Clean(r.status.Remote))
	}
	kv := func(k, v string) string { return SubtleStyle.Render(fmt.Sprintf("%-13s", k)) + TextStyle.Render(v) }
	brokerRows := []string{AccentStyle.Render("Broker")}
	if as := r.status; as != nil {
		role := Clean(strings.Join(as.Capabilities, ","))
		if role == "" {
			role = "none"
		}
		send := "anyone"
		if len(as.SendTo) > 0 && !slices.Contains(as.SendTo, "*") {
			send = Clean(strings.Join(as.SendTo, ", "))
		}
		seen := "never"
		if !as.LastSeen.IsZero() {
			seen = as.LastSeen.Local().Format("15:04:05") + " (" + since(as.LastSeen) + " ago)"
		}
		brokerRows = append(brokerRows,
			kv("state", Clean(as.State)), kv("queued", fmt.Sprint(as.Queued)),
			kv("accepted", OKStyle.Render(fmt.Sprint(as.Accepted))), kv("rejected", fmt.Sprint(as.Rejected)),
			kv("last seen", seen), kv("serves", role), kv("may send to", send), kv("observer", fmt.Sprint(as.Observer)))
	} else {
		brokerRows = append(brokerRows, SubtleStyle.Render("needs an observer connection"))
	}
	card := []string{AccentStyle.Render("Presence card")}
	if p := r.card; p != nil {
		keys := "none"
		if p.Keys != nil {
			keys = "claimed"
			if p.KeysPinned {
				keys = "pinned"
			}
		}
		card = append(card, kv("status", StatusStyle(p.Status).Render(Clean(string(p.Status)))+SubtleStyle.Render(" since "+p.TS.Local().Format("15:04:05"))),
			kv("description", Clean(p.Description)), kv("capabilities", Clean(strings.Join(p.Capabilities, ","))), kv("keys", keys),
			kv("meta", fmt.Sprintf("%d entries", len(p.Meta))))
	} else {
		card = append(card, SubtleStyle.Render("no card: the agent hasn't announced itself"))
	}
	col := w / 2
	top := lipgloss.JoinHorizontal(lipgloss.Top, panelRows(col, brokerRows), panelRows(col, card))

	f := &m.agents.feed
	traffic := AccentStyle.Render("Recent traffic") + SubtleStyle.Render(fmt.Sprintf("  %d messages involving %s", len(f.visible()), Clean(id)))
	if f.paused {
		traffic += WarnStyle.Render(fmt.Sprintf("  paused (%d not shown) · p to resume", f.dropped))
	}
	lines := f.lines(w, max(3, h-lipgloss.Height(top)-2))
	if len(lines) == 0 {
		lines = []string{SubtleStyle.Render("  none seen since the console started")}
	}
	return head + "\n" + top + "\n" + ansi.Truncate(traffic, w, "…") + "\n" + strings.Join(lines, "\n")
}
