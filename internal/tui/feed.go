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
	seq      int // order within a feed; set when added
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

// feedState is the scrollback shared by the feed, dashboard and status
// views: newest items at the bottom, an optional filter, and a selection.
// The selection is by sequence number, so it stays on the same message as
// new ones arrive and old ones scroll out; selSeq < 0 follows the newest.
type feedState struct {
	items      []Item
	total      int
	paused     bool
	dropped    int // items not shown while paused
	seq        int
	selSeq     int
	filter     func(Item) bool
	filterName string
}

func (f *feedState) add(it Item) {
	f.total++
	if f.paused {
		f.dropped++
		return
	}
	f.seq++
	it.seq = f.seq
	f.items = append(f.items, it)
	if len(f.items) > maxFeedItems {
		f.items = f.items[len(f.items)-maxFeedItems:]
	}
}

func (f *feedState) clear() { f.items, f.dropped, f.selSeq = nil, 0, 0 }

// following reports whether the view tracks the newest item.
func (f *feedState) following() bool { return f.selSeq <= 0 }

// setFilter shows only items matching keep (nil for all).
func (f *feedState) setFilter(name string, keep func(Item) bool) {
	f.filter, f.filterName, f.selSeq = keep, name, 0
}

func (f *feedState) visible() []Item {
	if f.filter == nil {
		return f.items
	}
	var out []Item
	for _, it := range f.items {
		if f.filter(it) {
			out = append(out, it)
		}
	}
	return out
}

// selectedIndex is the index of the selection in vis, or -1.
func (f *feedState) selectedIndex(vis []Item) int {
	if f.following() {
		return len(vis) - 1
	}
	for i, it := range vis {
		if it.seq == f.selSeq {
			return i
		}
	}
	return len(vis) - 1 // selection scrolled out: snap to newest
}

// move moves the selection by delta; moving past the newest resumes
// following.
func (f *feedState) move(delta int) {
	vis := f.visible()
	if len(vis) == 0 {
		return
	}
	i := f.selectedIndex(vis) + delta
	switch {
	case i >= len(vis)-1 && delta > 0:
		f.selSeq = 0
	case i < 0:
		f.selSeq = vis[0].seq
	default:
		f.selSeq = vis[min(i, len(vis)-1)].seq
	}
}

// home selects the oldest item.
func (f *feedState) home() {
	if vis := f.visible(); len(vis) > 0 {
		f.selSeq = vis[0].seq
	}
}

// selected is the selected item, or the newest when following.
func (f *feedState) selected() (Item, bool) {
	vis := f.visible()
	if len(vis) == 0 {
		return Item{}, false
	}
	return vis[f.selectedIndex(vis)], true
}

// lines renders what fits in height rows: the newest items when
// following, otherwise a window around the selection, which is marked.
func (f *feedState) lines(width, height int) []string {
	if height <= 0 {
		return nil
	}
	vis := f.visible()
	sel := f.selectedIndex(vis)
	start := max(0, len(vis)-height)
	if !f.following() {
		start = min(max(0, sel-height/2), max(0, len(vis)-height))
	}
	end := min(len(vis), start+height)
	out := make([]string, 0, height)
	for i := start; i < end; i++ {
		mark := "  "
		if i == sel && !f.following() {
			mark = AccentStyle.Render("▌ ")
		}
		out = append(out, mark+FormatItem(vis[i], max(1, width-2)))
	}
	return out
}

// feedKeys handles the selection keys shared by every feed-like view. It
// reports whether it handled the key.
func (f *feedState) feedKeys(key string) bool {
	switch key {
	case "up", "k":
		f.move(-1)
	case "down", "j":
		f.move(1)
	case "pgup":
		f.move(-10)
	case "pgdown":
		f.move(10)
	case "home", "g":
		f.home()
	case "end", "G":
		f.selSeq = 0
	case "p", "space":
		f.paused = !f.paused
	case "c":
		f.clear()
	default:
		return false
	}
	return true
}

// FeedModel is a full-screen, scrolling message feed (watch, listen).
// ↑/↓ select a message and enter opens it in the inspector.
type FeedModel struct {
	Title  string
	Source <-chan Item
	feedState
	inspect       *Inspector
	width, height int
	closed        bool
}

// NewFeed returns a feed that reads from src until it closes.
func NewFeed(title string, src <-chan Item) FeedModel {
	return FeedModel{Title: title, Source: src, width: 100, height: 24}
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
		if m.inspect != nil {
			m.inspect.Resize(m.width, m.height-2)
		}
		return m, nil
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
	case "esc":
		if m.following() {
			return m, tea.Quit
		}
		m.selSeq = 0
	case "enter":
		if it, ok := m.selected(); ok {
			m.inspect = NewInspector(it, m.width, m.height-2)
		}
	default:
		m.feedKeys(k.String())
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
	if m.inspect != nil {
		return header + "\n\n" + m.inspect.View()
	}
	body := m.lines(m.width, max(1, m.height-3))
	if len(body) == 0 {
		body = []string{SubtleStyle.Render("waiting for messages…")}
	}
	help := "↑/↓ select · enter inspect · p pause · c clear · q quit"
	if !m.following() {
		help = "↑/↓ select · enter inspect · G/esc back to live · q quit"
	}
	return header + "\n\n" + strings.Join(body, "\n") + "\n" + HelpStyle.Render(help)
}
