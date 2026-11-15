package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
)

// DashboardModel shows presence on the left and live traffic on the right.
type DashboardModel struct {
	Agent  string // who we're connected as
	Scope  string // what traffic is visible, e.g. "all traffic (observer)"
	Agents func() []a2a.Presence
	Source <-chan Item
	// Status, if set, feeds the broker summary line (observers only).
	Status  <-chan broker.Status
	summary string
	cards   []a2a.Presence
	feed    feedState
	counts  map[a2a.Type]int
	width   int
	height  int
	closed  bool
	updated time.Time
}

// NewDashboard returns a dashboard for agent, reading presence from
// agents and traffic from src.
func NewDashboard(agent, scope string, agents func() []a2a.Presence, src <-chan Item) DashboardModel {
	return DashboardModel{Agent: agent, Scope: scope, Agents: agents, Source: src, counts: map[a2a.Type]int{}, cards: agents()}
}

func (m DashboardModel) Init() tea.Cmd {
	cmds := []tea.Cmd{waitForItem(m.Source), tick(), tea.RequestBackgroundColor}
	if m.Status != nil {
		cmds = append(cmds, waitForStatus(m.Status))
	}
	return tea.Batch(cmds...)
}

func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handleBackground(msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case ItemMsg:
		it := Item(msg)
		if it.Env != nil {
			m.counts[it.Env.Type]++
		}
		m.feed.add(it)
		return m, waitForItem(m.Source)
	case StatusMsg:
		m.summary = BrokerSummary(broker.Status(msg))
		return m, waitForStatus(m.Status)
	case statusClosedMsg:
		return m, nil
	case sourceClosedMsg:
		m.closed = true
	case tickMsg:
		m.cards, m.updated = m.Agents(), time.Time(msg)
		return m, tick()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "p", "space":
			m.feed.paused = !m.feed.paused
		case "c":
			m.feed.items, m.feed.dropped = nil, 0
		}
	}
	return m, nil
}

func (m DashboardModel) View() tea.View { return fullScreen(m.render()) }

func (m DashboardModel) render() string {
	width, height := max(m.width, 60), max(m.height, 12)
	header := TitleStyle.Render("agentbus · dashboard") + " " +
		SubtleStyle.Render("as ") + AccentStyle.Render(m.Agent) + SubtleStyle.Render(" · "+m.Scope)
	stats := fmt.Sprintf("%d msgs · %s %d · %s %d · %s %d · %s %d",
		m.feed.total,
		typeStyle(a2a.TypeRequest).UnsetWidth().Render("req"), m.counts[a2a.TypeRequest],
		typeStyle(a2a.TypeResponse).UnsetWidth().Render("resp"), m.counts[a2a.TypeResponse],
		typeStyle(a2a.TypeEvent).UnsetWidth().Render("event"), m.counts[a2a.TypeEvent],
		typeStyle(a2a.TypeError).UnsetWidth().Render("err"), m.counts[a2a.TypeError])
	if m.feed.paused {
		stats += WarnStyle.Render(fmt.Sprintf(" · paused (%d not shown)", m.feed.dropped))
	}
	if m.closed {
		stats += ErrStyle.Render(" · disconnected")
	}

	if m.summary != "" {
		header += "\n" + SubtleStyle.Render(" "+m.summary)
	}
	bodyHeight := height - 6
	if m.summary != "" {
		bodyHeight--
	}
	leftWidth := min(36, width/3) // total panel widths, chrome included
	rightWidth := width - leftWidth
	inner := bodyHeight - panelPadY

	on, off := Counts(m.cards)
	left := []string{AccentStyle.Render("Agents") + SubtleStyle.Render(fmt.Sprintf("  %d up · %d down", on, off)), ""}
	for _, p := range m.cards {
		dot := StatusStyle(p.Status).Render("●")
		line := dot + " " + TextStyle.Render(p.Agent)
		if len(p.Capabilities) > 0 {
			line += SubtleStyle.Render(" " + strings.Join(p.Capabilities, ","))
		}
		left = append(left, ansi.Truncate(line, leftWidth-panelPadX, "…"))
	}
	if len(m.cards) == 0 {
		left = append(left, SubtleStyle.Render("no presence cards yet…"))
	}

	right := []string{AccentStyle.Render("Traffic") + "  " + SubtleStyle.Render(stats), ""}
	lines := m.feed.lines(rightWidth-panelPadX, inner-2) // minus the title and blank rows
	if len(lines) == 0 {
		lines = []string{SubtleStyle.Render("waiting for messages…")}
	}
	right = append(right, lines...)

	panel := func(w int, rows []string) string {
		if len(rows) > inner {
			rows = rows[:inner]
		}
		return PanelStyle.Width(w).Height(bodyHeight).Render(strings.Join(rows, "\n"))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, panel(leftWidth, left), panel(rightWidth, right))
	help := HelpStyle.Render("q quit · p pause · c clear")
	return header + "\n" + body + "\n" + help
}
