package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
)

// trafficTypes are the type filters t cycles through.
var trafficTypes = []string{"", string(a2a.TypeRequest), string(a2a.TypeResponse), string(a2a.TypeEvent), string(a2a.TypeError), "presence"}

// trafficTab is the live message feed with filters by agent, type and
// text.
type trafficTab struct {
	feed    feedState
	counts  map[a2a.Type]int
	agent   string // only messages involving this agent
	typ     string // only this type ("" for all)
	text    string // only messages containing this, case-insensitively
	editing bool   // the text filter has focus
	input   textinput.Model
}

func (m *Console) addItem(it Item) {
	t := &m.traffic
	if t.counts == nil {
		t.counts = map[a2a.Type]int{}
	}
	if it.Env != nil {
		t.counts[it.Env.Type]++
	}
	t.feed.add(it)
	if d := m.agents.detail; d != "" && involves(it, d) {
		m.agents.feed.add(it)
	}
	now := time.Now()
	m.bus.see(it, now)
	m.bus.refusal(it.Env, now)
}

func (t *trafficTab) setAgent(id string) {
	t.agent = id
	t.apply()
}

// apply rebuilds the feed filter from the agent, type and text filters.
func (t *trafficTab) apply() {
	agent, typ, text := t.agent, t.typ, strings.ToLower(t.text)
	if agent == "" && typ == "" && text == "" {
		t.feed.setFilter("", nil)
		return
	}
	var names []string
	if agent != "" {
		names = append(names, "involving "+agent)
	}
	if typ != "" {
		names = append(names, typ+"s")
	}
	if text != "" {
		names = append(names, fmt.Sprintf("matching %q", t.text))
	}
	t.feed.setFilter(strings.Join(names, ", "), func(it Item) bool {
		if agent != "" && !involves(it, agent) {
			return false
		}
		if typ != "" && itemType(it) != typ {
			return false
		}
		return text == "" || strings.Contains(strings.ToLower(itemText(it)), text)
	})
}

func itemType(it Item) string {
	switch {
	case it.Env != nil:
		return string(it.Env.Type)
	case it.Presence != nil:
		return "presence"
	}
	return "raw"
}

// itemText is everything a text filter searches.
func itemText(it Item) string {
	parts := []string{it.Topic}
	switch {
	case it.Env != nil:
		e := it.Env
		parts = append(parts, e.From, e.To, e.Capability, e.Event, e.ID, e.CorrelationID, string(e.Body))
	case it.Presence != nil:
		p := it.Presence
		parts = append(parts, p.Agent, p.Description, strings.Join(p.Capabilities, " "))
	default:
		parts = append(parts, string(it.Raw))
	}
	return strings.Join(parts, " ")
}

func (m *Console) trafficUpdate(msg tea.Msg) tea.Cmd {
	t := &m.traffic
	k, ok := msg.(tea.KeyPressMsg)
	if t.editing {
		if ok {
			switch k.String() {
			case "enter":
				t.editing = false
				t.text = strings.TrimSpace(t.input.Value())
				t.input.Blur()
				t.apply()
				return nil
			case "esc":
				t.editing = false
				t.input.Blur()
				return nil
			}
		}
		var cmd tea.Cmd
		t.input, cmd = t.input.Update(msg)
		return cmd
	}
	if !ok {
		return nil
	}
	switch k.String() {
	case "/":
		t.input = textinput.New()
		t.input.Prompt = "/ "
		t.input.Placeholder = "text in topic, sender, recipient or body"
		t.input.SetValue(t.text)
		t.input.SetWidth(max(20, m.width-12))
		t.editing = true
		return t.input.Focus()
	case "t":
		for i, typ := range trafficTypes {
			if typ == t.typ {
				t.typ = trafficTypes[(i+1)%len(trafficTypes)]
				break
			}
		}
		t.apply()
	case "esc":
		switch {
		case !t.feed.following():
			t.feed.selSeq = 0
		case t.text != "":
			t.text = ""
			t.apply()
		case t.typ != "":
			t.typ = ""
			t.apply()
		case t.agent != "":
			t.agent = ""
			t.apply()
		}
	case "enter":
		if it, ok := t.feed.selected(); ok {
			m.openInspector(it)
		}
	case "R":
		if it, ok := t.feed.selected(); ok && it.Env != nil {
			return m.composeFrom(it.Env)
		}
		m.say(true, "only envelopes can be copied into Compose")
	default:
		t.feed.feedKeys(k.String())
	}
	return nil
}

func (m *Console) trafficHelp() string {
	help := "↑/↓ select · enter inspect · / filter · t type · R to compose · p pause · c clear"
	t := &m.traffic
	if t.feed.filter != nil || !t.feed.following() {
		help += " · esc back"
	}
	return help
}

func (m *Console) renderTraffic(w, h int) string {
	t := &m.traffic
	stats := fmt.Sprintf("%d msgs · %s %d · %s %d · %s %d · %s %d",
		t.feed.total,
		typeStyle(a2a.TypeRequest).UnsetWidth().Render("req"), t.counts[a2a.TypeRequest],
		typeStyle(a2a.TypeResponse).UnsetWidth().Render("resp"), t.counts[a2a.TypeResponse],
		typeStyle(a2a.TypeEvent).UnsetWidth().Render("event"), t.counts[a2a.TypeEvent],
		typeStyle(a2a.TypeError).UnsetWidth().Render("err"), t.counts[a2a.TypeError])
	if t.feed.paused {
		stats += WarnStyle.Render(fmt.Sprintf(" · paused (%d not shown)", t.feed.dropped))
	}
	head := AccentStyle.Render("Traffic") + "  " + SubtleStyle.Render(stats)
	lines := []string{ansi.Truncate(head, w, "…")}
	switch {
	case t.editing:
		lines = append(lines, t.input.View())
	case t.feed.filterName != "":
		lines = append(lines, WarnStyle.Render(" only "+Clean(t.feed.filterName))+SubtleStyle.Render(fmt.Sprintf(" · %d shown · esc clears", len(t.feed.visible()))))
	default:
		lines = append(lines, SubtleStyle.Render(" "+m.opt.Scope))
	}
	body := t.feed.lines(w, h-len(lines))
	if len(body) == 0 {
		body = []string{SubtleStyle.Render("  waiting for messages…")}
		if t.feed.filter != nil && t.feed.total > 0 {
			body = []string{SubtleStyle.Render("  nothing matches the filter")}
		}
	}
	return strings.Join(append(lines, body...), "\n")
}
