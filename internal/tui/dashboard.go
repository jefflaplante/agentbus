package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
)

// DashboardModel shows presence on the left and live traffic on the right.
//
// tab moves focus between the panels. In the agents panel, ↑/↓ select an
// agent, enter filters the traffic to it, i inspects its presence card
// and e opens it in the config editor. In the traffic panel, ↑/↓ select a
// message and enter inspects it. esc steps back: inspector, filter,
// selection, then quit.
type DashboardModel struct {
	Agent  string // who we're connected as
	Scope  string // what traffic is visible, e.g. "all traffic (observer)"
	Agents func() []a2a.Presence
	Source <-chan Item
	// Status, if set, feeds the broker summary line (observers only).
	Status <-chan broker.Status
	// EditAgent, if set, opens the config editor on an agent (only when
	// the broker config file is on this machine).
	EditAgent func(id string) tea.Model

	summary string
	cards   []a2a.Presence
	feed    feedState
	counts  map[a2a.Type]int
	focus   int // 0 agents, 1 traffic
	cursor  int // selected agent
	inspect *Inspector
	sub     tea.Model // embedded config editor
	width   int
	height  int
	closed  bool
	updated time.Time
}

// NewDashboard returns a dashboard for agent, reading presence from
// agents and traffic from src.
func NewDashboard(agent, scope string, agents func() []a2a.Presence, src <-chan Item) DashboardModel {
	return DashboardModel{Agent: agent, Scope: scope, Agents: agents, Source: src, counts: map[a2a.Type]int{},
		cards: agents(), focus: 1, width: 120, height: 30}
}

func (m DashboardModel) Init() tea.Cmd {
	cmds := []tea.Cmd{waitForItem(m.Source), tick(), tea.RequestBackgroundColor}
	if m.Status != nil {
		cmds = append(cmds, waitForStatus(m.Status))
	}
	return tea.Batch(cmds...)
}

func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handleBackground(msg) && m.sub == nil {
		return m, nil
	}
	// Background work continues whatever is on screen.
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
		return m, nil
	case tickMsg:
		m.cards, m.updated = m.Agents(), time.Time(msg)
		m.cursor = min(m.cursor, max(0, len(m.cards)-1))
		return m, tick()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.inspect != nil {
			m.inspect.Resize(m.width, m.height-2)
		}
		if m.sub != nil {
			var cmd tea.Cmd
			m.sub, cmd = m.sub.Update(msg)
			return m, cmd
		}
		return m, nil
	case CloseMsg:
		m.sub = nil
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

	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
		return m, nil
	case "esc":
		switch {
		case m.feed.filter != nil:
			m.feed.setFilter("", nil)
		case !m.feed.following():
			m.feed.selSeq = 0
		default:
			return m, tea.Quit
		}
		return m, nil
	}

	if m.focus == 0 {
		switch k.String() {
		case "up", "k":
			m.cursor = max(0, m.cursor-1)
		case "down", "j":
			m.cursor = min(len(m.cards)-1, m.cursor+1)
		case "enter":
			if p, ok := m.selectedCard(); ok {
				if m.feed.filterName == p.Agent {
					m.feed.setFilter("", nil)
				} else {
					id := p.Agent
					m.feed.setFilter(id, func(it Item) bool { return involves(it, id) })
				}
			}
		case "i":
			if p, ok := m.selectedCard(); ok {
				m.inspect = NewInspector(Item{Topic: a2a.PresenceTopic(p.Agent), Presence: &p}, m.width, m.height-2)
			}
		case "e":
			if p, ok := m.selectedCard(); ok && m.EditAgent != nil {
				m.sub = m.EditAgent(p.Agent)
				if m.sub != nil {
					return m, tea.Batch(m.sub.Init(), func() tea.Msg { return tea.WindowSizeMsg{Width: m.width, Height: m.height} })
				}
			}
		default:
			m.feed.feedKeys(k.String())
		}
		return m, nil
	}

	switch k.String() {
	case "enter":
		if it, ok := m.feed.selected(); ok {
			m.inspect = NewInspector(it, m.width, m.height-2)
		}
	default:
		m.feed.feedKeys(k.String())
	}
	return m, nil
}

func (m DashboardModel) selectedCard() (a2a.Presence, bool) {
	if m.cursor < 0 || m.cursor >= len(m.cards) {
		return a2a.Presence{}, false
	}
	return m.cards[m.cursor], true
}

func (m DashboardModel) View() tea.View {
	if m.sub != nil {
		return m.sub.View()
	}
	return fullScreen(m.render())
}

func (m DashboardModel) render() string {
	width, height := max(m.width, 60), max(m.height, 12)
	header := TitleStyle.Render("agentbus · dashboard") + " " +
		SubtleStyle.Render("as ") + AccentStyle.Render(m.Agent) + SubtleStyle.Render(" · "+m.Scope)
	if m.summary != "" {
		header += "\n" + SubtleStyle.Render(" "+m.summary)
	}
	if m.inspect != nil {
		return header + "\n\n" + m.inspect.View()
	}
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

	bodyHeight := height - 6
	if m.summary != "" {
		bodyHeight--
	}
	leftWidth := min(36, width/3) // total panel widths, chrome included
	rightWidth := width - leftWidth
	inner := bodyHeight - panelPadY

	title := func(name string, focused bool) string {
		if focused {
			return AccentStyle.Underline(true).Render(name)
		}
		return AccentStyle.Render(name)
	}
	on, off := Counts(m.cards)
	left := []string{title("Agents", m.focus == 0) + SubtleStyle.Render(fmt.Sprintf("  %d up · %d down", on, off)), ""}
	for i, p := range m.cards {
		mark := "  "
		if i == m.cursor && m.focus == 0 {
			mark = AccentStyle.Render("▌ ")
		}
		dot := StatusStyle(p.Status).Render("●")
		line := mark + dot + " " + TextStyle.Render(p.Agent)
		if m.feed.filterName == p.Agent {
			line += WarnStyle.Render(" ⧩")
		}
		if len(p.Capabilities) > 0 {
			line += SubtleStyle.Render(" " + strings.Join(p.Capabilities, ","))
		}
		left = append(left, ansi.Truncate(line, leftWidth-panelPadX, "…"))
	}
	if len(m.cards) == 0 {
		left = append(left, SubtleStyle.Render("no presence cards yet…"))
	}

	trafficTitle := title("Traffic", m.focus == 1)
	if m.feed.filterName != "" {
		trafficTitle += WarnStyle.Render(" · only " + m.feed.filterName)
	}
	right := []string{ansi.Truncate(trafficTitle+"  "+SubtleStyle.Render(stats), rightWidth-panelPadX, "…"), ""}
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
	help := "tab switch panel · ↑/↓ select · enter inspect · p pause · c clear · q quit"
	if m.focus == 0 {
		help = "tab traffic · ↑/↓ select · enter filter traffic · i inspect card"
		if m.EditAgent != nil {
			help += " · e edit in config"
		}
		help += " · q quit"
	}
	if m.feed.filter != nil || !m.feed.following() {
		help += " · esc back"
	}
	return header + "\n" + body + "\n" + HelpStyle.Render(ansi.Truncate(help, width-1, "…"))
}
