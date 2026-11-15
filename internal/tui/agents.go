package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/mqtt/a2a"
)

// refreshEvery is how often live views re-read presence.
const refreshEvery = 500 * time.Millisecond

type tickMsg time.Time

func tick() tea.Cmd { return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) }) }

// AgentRows renders presence cards as table rows: agent, status, since,
// capabilities, keys, description.
func AgentRows(cards []a2a.Presence) []table.Row {
	rows := make([]table.Row, 0, len(cards))
	for _, p := range cards {
		keys := ""
		switch {
		case p.Keys != nil && p.KeysPinned:
			keys = "pinned"
		case p.Keys != nil:
			keys = "claimed"
		}
		rows = append(rows, table.Row{
			p.Agent, string(p.Status), p.TS.Local().Format("Jan 02 15:04:05"),
			strings.Join(p.Capabilities, ","), keys, p.Description,
		})
	}
	return rows
}

// AgentsModel is a live table of every agent's presence card.
type AgentsModel struct {
	Source func() []a2a.Presence
	table  table.Model
	cards  []a2a.Presence
	width  int
	at     time.Time
}

// NewAgents returns a live agents table reading from src.
func NewAgents(src func() []a2a.Presence) AgentsModel {
	t := table.New(
		table.WithColumns(agentColumns(100)),
		table.WithFocused(true),
		table.WithHeight(12),
	)
	restyleTable(&t)
	t.SetWidth(100) // bubbles v2 tables clip to their width; resized on WindowSizeMsg
	m := AgentsModel{Source: src, table: t}
	m.refresh(time.Now())
	return m
}

func agentColumns(width int) []table.Column {
	w := []int{16, 8, 16, 20, 8} // AGENT STATUS SINCE CAPABILITIES KEYS
	if width < 110 {
		w = []int{12, 7, 15, 14, 7}
	}
	used := 6 * 2 // 1 cell of padding each side of 6 columns
	for _, n := range w {
		used += n
	}
	return []table.Column{
		{Title: "AGENT", Width: w[0]},
		{Title: "STATUS", Width: w[1]},
		{Title: "SINCE", Width: w[2]},
		{Title: "CAPABILITIES", Width: w[3]},
		{Title: "KEYS", Width: w[4]},
		{Title: "DESCRIPTION", Width: max(6, width-used)},
	}
}

func (m *AgentsModel) refresh(now time.Time) {
	m.cards = m.Source()
	m.table.SetRows(AgentRows(m.cards))
	m.at = now
}

func (m AgentsModel) Init() tea.Cmd { return tea.Batch(tick(), tea.RequestBackgroundColor) }

func (m AgentsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handleBackground(msg) {
		restyleTable(&m.table)
		return m, nil
	}
	switch msg := msg.(type) {
	case tickMsg:
		m.refresh(time.Time(msg))
		return m, tick()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.table.SetColumns(agentColumns(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-6))
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// Counts returns how many agents are online and offline.
func Counts(cards []a2a.Presence) (online, offline int) {
	for _, p := range cards {
		if p.Status == a2a.StatusOnline {
			online++
		} else {
			offline++
		}
	}
	return
}

func (m AgentsModel) View() tea.View { return fullScreen(m.render()) }

func (m AgentsModel) render() string {
	on, off := Counts(m.cards)
	header := TitleStyle.Render("agentbus · agents") + " " +
		OnlineStyle.Render(fmt.Sprintf("%d online", on)) + SubtleStyle.Render(" · ") +
		OfflineStyle.Render(fmt.Sprintf("%d offline", off))
	body := m.table.View()
	if len(m.cards) == 0 {
		body = SubtleStyle.Render("no presence cards yet…")
	}
	help := HelpStyle.Render("↑/↓ select · q quit · updated " + m.at.Format("15:04:05"))
	return header + "\n\n" + body + "\n" + help
}
