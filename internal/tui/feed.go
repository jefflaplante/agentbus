package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/mqtt/a2a"
)

// Item is one message shown in a feed.
type Item struct {
	Time     time.Time
	Topic    string
	Env      *a2a.Envelope // set for envelopes
	Presence *a2a.Presence // set for presence cards
	Raw      []byte        // anything else
}

// ItemFromMessage classifies a raw MQTT message.
func ItemFromMessage(topic string, payload []byte) Item {
	it := Item{Time: time.Now(), Topic: topic}
	if t, err := a2a.ParseTopic(topic); err == nil && t.Kind == a2a.KindPresence {
		if p, err := a2a.ParsePresence(payload); err == nil {
			it.Presence = p
			return it
		}
	}
	if e, err := a2a.Parse(payload); err == nil {
		it.Env = e
		return it
	}
	it.Raw = payload
	return it
}

// ItemFromEnvelope wraps an envelope delivered by the client library.
func ItemFromEnvelope(topic string, e *a2a.Envelope) Item {
	return Item{Time: time.Now(), Topic: topic, Env: e}
}

// FormatItem renders an item as one line, at most width cells wide.
func FormatItem(it Item, width int) string {
	ts := SubtleStyle.Render(it.Time.Local().Format("15:04:05"))
	var line string
	switch {
	case it.Env != nil:
		e := it.Env
		route := AccentStyle.Render(e.From)
		switch {
		case e.To != "":
			route += SubtleStyle.Render(" → ") + TextStyle.Render(e.To)
		case e.Capability != "":
			route += SubtleStyle.Render(" → task:") + TextStyle.Render(e.Capability)
		default:
			route += SubtleStyle.Render(" → ") + TextStyle.Render(strings.TrimPrefix(it.Topic, a2a.Root+"/"))
		}
		var tags []string
		if e.Priority != "" {
			tags = append(tags, priorityStyle(e.Priority).Render(string(e.Priority)))
		}
		if e.Event != "" {
			tags = append(tags, WarnStyle.Render(e.Event))
		}
		line = fmt.Sprintf("%s %s %s", ts, typeStyle(e.Type).Render(string(e.Type)), route)
		if len(tags) > 0 {
			line += " " + strings.Join(tags, " ")
		}
		if body := preview(e.Body); body != "" {
			line += "  " + SubtleStyle.Render(body)
		}
	case it.Presence != nil:
		p := it.Presence
		line = fmt.Sprintf("%s %s %s %s", ts, typeStyle("presence").Render("presence"),
			AccentStyle.Render(p.Agent), StatusStyle(p.Status).Render(string(p.Status)))
		if len(p.Capabilities) > 0 {
			line += SubtleStyle.Render(" [" + strings.Join(p.Capabilities, ",") + "]")
		}
	default:
		line = fmt.Sprintf("%s %s %s  %s", ts, typeStyle("raw").Render("raw"),
			TextStyle.Render(it.Topic), SubtleStyle.Render(preview(it.Raw)))
	}
	if width > 0 {
		line = ansi.Truncate(line, width, "…") // never wrap: one item, one row
	}
	return line
}

// preview compacts a JSON body (or any bytes) to one short line.
func preview(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, b) == nil {
		b = buf.Bytes()
	}
	s := strings.ReplaceAll(string(b), "\n", " ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// ItemMsg delivers one item to a running feed or dashboard.
type ItemMsg Item

// sourceClosedMsg reports that the item channel closed.
type sourceClosedMsg struct{}

// waitForItem reads the next item from ch as a tea.Cmd.
func waitForItem(ch <-chan Item) tea.Cmd {
	return func() tea.Msg {
		it, ok := <-ch
		if !ok {
			return sourceClosedMsg{}
		}
		return ItemMsg(it)
	}
}

const maxFeedItems = 1000

// feedState is the scrollback shared by the feed and dashboard views.
type feedState struct {
	items   []Item
	total   int
	paused  bool
	dropped int // items not shown while paused
}

func (f *feedState) add(it Item) {
	f.total++
	if f.paused {
		f.dropped++
		return
	}
	f.items = append(f.items, it)
	if len(f.items) > maxFeedItems {
		f.items = f.items[len(f.items)-maxFeedItems:]
	}
}

// lines renders the newest items that fit in height rows.
func (f *feedState) lines(width, height int) []string {
	if height <= 0 {
		return nil
	}
	start := max(0, len(f.items)-height)
	out := make([]string, 0, height)
	for _, it := range f.items[start:] {
		out = append(out, FormatItem(it, width))
	}
	return out
}

// FeedModel is a full-screen, scrolling message feed (watch, listen).
type FeedModel struct {
	Title  string
	Source <-chan Item
	feedState
	width, height int
	closed        bool
}

// NewFeed returns a feed that reads from src until it closes.
func NewFeed(title string, src <-chan Item) FeedModel {
	return FeedModel{Title: title, Source: src}
}

func (m FeedModel) Init() tea.Cmd {
	return tea.Batch(waitForItem(m.Source), tea.RequestBackgroundColor)
}

func (m FeedModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handleBackground(msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case ItemMsg:
		m.add(Item(msg))
		return m, waitForItem(m.Source)
	case sourceClosedMsg:
		m.closed = true
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "p", "space":
			m.paused = !m.paused
		case "c":
			m.items, m.dropped = nil, 0
		}
	}
	return m, nil
}

func (m FeedModel) View() tea.View { return fullScreen(m.render()) }

func (m FeedModel) render() string {
	status := fmt.Sprintf("%d messages", m.total)
	if m.paused {
		status += WarnStyle.Render(fmt.Sprintf("  paused (%d not shown)", m.dropped))
	}
	if m.closed {
		status += ErrStyle.Render("  disconnected")
	}
	header := TitleStyle.Render(m.Title) + " " + SubtleStyle.Render(status)
	body := m.lines(m.width, max(1, m.height-3))
	if len(body) == 0 {
		body = []string{SubtleStyle.Render("waiting for messages…")}
	}
	help := HelpStyle.Render("q quit · p pause · c clear")
	return header + "\n\n" + strings.Join(body, "\n") + "\n" + help
}
