package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/a2a"
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
			Clean(p.Agent), Clean(string(p.Status)), p.TS.Local().Format("Jan 02 15:04:05"),
			Clean(strings.Join(p.Capabilities, ",")), keys, Clean(p.Description),
		})
	}
	return rows
}

// AgentsModel is a live table of every agent's presence card.
//
// enter inspects the selected agent's card; e opens it in the config
// editor when the broker config is on this machine.
type AgentsModel struct {
	Source    func() []a2a.Presence
	EditAgent func(id string) tea.Model
	inspect   *Inspector
	sub       tea.Model
	height    int
	table     table.Model
	cards     []a2a.Presence
	width     int
	at        time.Time
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
	var sel string
	if i := m.table.Cursor(); i >= 0 && i < len(m.cards) {
		sel = m.cards[i].Agent
	}
	m.cards = m.Source()
	m.table.SetRows(AgentRows(m.cards))
	// Keep the cursor on the same agent when cards come and go.
	if i := slices.IndexFunc(m.cards, func(p a2a.Presence) bool { return p.Agent == sel }); sel != "" && i >= 0 {
		m.table.SetCursor(i)
	}
	m.at = now
}

func (m AgentsModel) Init() tea.Cmd { return tea.Batch(tick(), tea.RequestBackgroundColor) }

func (m AgentsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handleBackground(msg) && m.sub == nil {
		restyleTable(&m.table)
		return m, nil
	}
	switch msg := msg.(type) {
	case tickMsg:
		m.refresh(time.Time(msg))
		return m, tick()
	case CloseMsg:
		m.sub = nil
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.table.SetColumns(agentColumns(msg.Width))
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(max(3, msg.Height-6))
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
	if k, ok := msg.(tea.KeyPressMsg); ok {
		i := m.table.Cursor()
		switch k.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "enter", "i":
			if i >= 0 && i < len(m.cards) {
				p := m.cards[i]
				m.inspect = NewInspector(Item{Topic: a2a.PresenceTopic(p.Agent), Presence: &p}, m.width, max(10, m.height-2))
			}
			return m, nil
		case "e":
			if i >= 0 && i < len(m.cards) && m.EditAgent != nil {
				if m.sub = m.EditAgent(m.cards[i].Agent); m.sub != nil {
					w, h := m.width, m.height
					return m, tea.Batch(m.sub.Init(), func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} })
				}
			}
			return m, nil
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

func (m AgentsModel) View() tea.View {
	if m.sub != nil {
		return m.sub.View()
	}
	return fullScreen(m.render())
}

func (m AgentsModel) render() string {
	on, off := Counts(m.cards)
	header := TitleStyle.Render("agentbus · agents") + " " +
		OnlineStyle.Render(fmt.Sprintf("%d online", on)) + SubtleStyle.Render(" · ") +
		OfflineStyle.Render(fmt.Sprintf("%d offline", off))
	body := m.table.View()
	if len(m.cards) == 0 {
		body = SubtleStyle.Render("no presence cards yet…")
	}
	if m.inspect != nil {
		return header + "\n\n" + m.inspect.View()
	}
	helpText := "↑/↓ select · enter inspect card"
	if m.EditAgent != nil {
		helpText += " · e edit in config"
	}
	help := HelpStyle.Render(helpText + " · q quit · updated " + m.at.Format("15:04:05"))
	return header + "\n\n" + body + "\n" + help
}
