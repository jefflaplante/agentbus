package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// Answerer is the console's inbox handler. Every message sent to the
// console's agent goes to the Inbox tab. While holding (the default),
// requests wait there for the user to answer or refuse them, until their
// TTL runs out; otherwise they're refused at once.
type Answerer struct {
	agent string
	c     chan *InboxEntry
	hold  atomic.Bool
}

// NewAnswerer returns an Answerer for agent's inbox that holds requests
// for the user.
func NewAnswerer(agent string) *Answerer {
	a := &Answerer{agent: agent, c: make(chan *InboxEntry, 256)}
	a.hold.Store(true)
	return a
}

// C delivers inbox entries; the console reads it.
func (a *Answerer) C() <-chan *InboxEntry { return a.c }

// Holding reports whether requests wait for the user.
func (a *Answerer) Holding() bool { return a.hold.Load() }

// SetHolding sets whether requests wait for the user.
func (a *Answerer) SetHolding(on bool) { a.hold.Store(on) }

// answer is the user's reply to a held request.
type answer struct {
	body json.RawMessage
	err  error
}

// InboxEntry is one message delivered to the console's inbox.
type InboxEntry struct {
	Item     Item
	At       time.Time
	Deadline time.Time    // when a held request stops waiting
	reply    chan answer  // nil unless the request is held
	result   atomic.Value // string: what became of a held request
}

func (e *InboxEntry) pending() bool {
	return e.reply != nil && e.outcome() == "" && time.Now().Before(e.Deadline)
}

func (e *InboxEntry) outcome() string {
	s, _ := e.result.Load().(string)
	return s
}

// Handle is a client.Handler for the console's inbox.
func (a *Answerer) Handle(ctx context.Context, env *a2a.Envelope) (any, error) {
	e := &InboxEntry{Item: ItemFromEnvelope(a2a.InboxTopic(a.agent), env), At: time.Now()}
	holding := env.Type == a2a.TypeRequest && a.hold.Load()
	if holding {
		e.reply = make(chan answer, 1)
		e.Deadline, _ = ctx.Deadline()
	}
	select {
	case a.c <- e:
	default:
		if env.Type == a2a.TypeRequest {
			return nil, client.Errorf("busy", "the console's inbox is full")
		}
		return nil, nil
	}
	if env.Type != a2a.TypeRequest {
		return nil, nil
	}
	if !holding {
		e.result.Store("refused at once (not holding)")
		return nil, client.Errorf("not_answering", "this agent is a console that isn't answering requests now")
	}
	select {
	case ans := <-e.reply:
		if ans.err != nil {
			return nil, ans.err
		}
		if ans.body == nil {
			return nil, nil
		}
		return ans.body, nil
	case <-ctx.Done():
		if e.result.CompareAndSwap(nil, "expired unanswered") {
			return nil, client.Errorf("timeout", "nobody answered at the console in time")
		}
		return nil, ctx.Err()
	}
}

// inboxTab lists the console's inbox and answers held requests.
type inboxTab struct {
	entries []*InboxEntry // newest first
	seen    int           // entries when the tab was last shown
	cursor  int
	mode    int // 0 list, 1 answering, 2 refusing
	body    textarea.Model
	reason  textinput.Model
	width   int
	height  int
}

const (
	inboxList = iota
	inboxAnswer
	inboxRefuse
)

func (t *inboxTab) add(e *InboxEntry) {
	t.entries = append([]*InboxEntry{e}, t.entries...)
	if len(t.entries) > maxFeedItems {
		t.entries = t.entries[:maxFeedItems]
	}
	if t.cursor > 0 || t.mode != inboxList {
		t.cursor++ // stay on the same entry
	}
	t.seen++
	if t.seen > len(t.entries) {
		t.seen = len(t.entries)
	}
}

func newInboxTab() inboxTab {
	t := inboxTab{body: textarea.New(), reason: textinput.New()}
	t.body.ShowLineNumbers = false
	t.body.Placeholder = `{"ok": true}`
	t.reason.Prompt = ""
	t.reason.Placeholder = "refused at the console"
	return t
}

func (t *inboxTab) resize(w, h int) {
	t.width, t.height = w, h
	t.body.SetWidth(max(10, w-4))
	t.body.SetHeight(max(3, h/3))
	t.reason.SetWidth(max(10, w-14))
}

func (t *inboxTab) editing() bool { return t.mode != inboxList }

func (t *inboxTab) waiting() int {
	n := 0
	for _, e := range t.entries {
		if e.pending() {
			n++
		}
	}
	return n
}

func (t *inboxTab) selected() *InboxEntry {
	if t.cursor < 0 || t.cursor >= len(t.entries) {
		return nil
	}
	return t.entries[t.cursor]
}

func (t *inboxTab) editHelp() string {
	if t.mode == inboxAnswer {
		return "type the reply body (JSON) · ctrl+s send · esc cancel"
	}
	return "type a reason · enter refuse · esc cancel"
}

// resolve answers a held request, unless it already ended.
func (e *InboxEntry) resolve(ans answer, outcome string) bool {
	if !e.result.CompareAndSwap(nil, outcome) {
		return false
	}
	e.reply <- ans
	return true
}

func (m *Console) inboxUpdate(msg tea.Msg) tea.Cmd {
	t := &m.inbox
	k, isKey := msg.(tea.KeyPressMsg)
	switch t.mode {
	case inboxAnswer:
		if isKey {
			switch k.String() {
			case "esc":
				t.mode = inboxList
				return nil
			case "ctrl+s":
				e := t.selected()
				s := strings.TrimSpace(t.body.Value())
				var body json.RawMessage
				if s != "" {
					if !json.Valid([]byte(s)) {
						m.say(true, "the reply body is not valid JSON")
						return nil
					}
					body = json.RawMessage(s)
				}
				t.mode = inboxList
				if e == nil || !e.resolve(answer{body: body}, "answered") {
					m.say(true, "too late: the request already ended")
					return nil
				}
				m.say(false, "answered %s", Clean(e.Item.Env.From))
				return nil
			}
		}
		var cmd tea.Cmd
		t.body, cmd = t.body.Update(msg)
		return cmd
	case inboxRefuse:
		if isKey {
			switch k.String() {
			case "esc":
				t.mode = inboxList
				return nil
			case "enter":
				e := t.selected()
				reason := strings.TrimSpace(t.reason.Value())
				if reason == "" {
					reason = "refused at the console"
				}
				t.mode = inboxList
				if e == nil || !e.resolve(answer{err: client.Errorf("refused", "%s", reason)}, "refused: "+reason) {
					m.say(true, "too late: the request already ended")
					return nil
				}
				m.say(false, "refused %s", Clean(e.Item.Env.From))
				return nil
			}
		}
		var cmd tea.Cmd
		t.reason, cmd = t.reason.Update(msg)
		return cmd
	}
	if !isKey {
		return nil
	}
	switch k.String() {
	case "up", "k":
		t.cursor = max(0, t.cursor-1)
	case "down", "j":
		t.cursor = min(len(t.entries)-1, t.cursor+1)
	case "home", "g":
		t.cursor = 0
	case "enter":
		if e := t.selected(); e != nil {
			m.openInspector(e.Item)
		}
	case "h":
		if m.opt.Inbox == nil {
			return nil
		}
		on := !m.opt.Inbox.Holding()
		m.opt.Inbox.SetHolding(on)
		if on {
			m.say(false, "holding requests: they wait here for your answer")
		} else {
			m.say(false, "not holding: requests are refused at once")
		}
	case "a", "x":
		e := t.selected()
		if e == nil || !e.pending() {
			m.say(true, "select a request that's waiting for an answer")
			return nil
		}
		if k.String() == "a" {
			t.body.Reset()
			t.body.SetStyles(textarea.DefaultStyles(pal == darkPalette))
			t.mode = inboxAnswer
			return t.body.Focus()
		}
		t.reason.Reset()
		t.reason.SetStyles(textinput.DefaultStyles(pal == darkPalette))
		t.mode = inboxRefuse
		return t.reason.Focus()
	case "r":
		if e := t.selected(); e != nil && e.Item.Env != nil {
			return m.composeTo(kindSend, e.Item.Env.From)
		}
	}
	return nil
}

func (m *Console) inboxHelp() string {
	return "↑/↓ select · enter inspect · a answer · x refuse · h hold on/off · r message sender"
}

func (m *Console) renderInbox(w, h int) string {
	t := &m.inbox
	hold := SubtleStyle.Render("answering off")
	if m.opt.Inbox != nil {
		if m.opt.Inbox.Holding() {
			hold = OKStyle.Render("holding requests") + SubtleStyle.Render(" for you to answer · h to refuse them at once")
		} else {
			hold = WarnStyle.Render("refusing requests at once") + SubtleStyle.Render(" · h to hold them for you")
		}
	}
	head := AccentStyle.Render("Inbox of "+Clean(m.opt.Agent)) + SubtleStyle.Render(fmt.Sprintf("  %d messages · %d waiting · ", len(t.entries), t.waiting())) + hold
	lines := []string{ansi.Truncate(head, w, "…"), ""}
	if len(t.entries) == 0 {
		lines = append(lines, SubtleStyle.Render("nothing yet. Messages sent to "+Clean(m.opt.Agent)+" arrive here, including the broker's refusals of what you send."))
		return strings.Join(lines, "\n")
	}
	editH := 0
	if t.mode == inboxAnswer {
		editH = t.body.Height() + 3
	} else if t.mode == inboxRefuse {
		editH = 3
	}
	listH := max(3, min(len(t.entries)+1, (h-len(lines)-editH)/2))
	rows := make([][]string, 0, len(t.entries))
	for _, e := range t.entries {
		env := e.Item.Env
		rows = append(rows, []string{e.At.Local().Format("15:04:05"), typeStyle(env.Type).Render(Clean(string(env.Type))),
			AccentStyle.Render(Clean(env.From)), m.inboxState(e), SubtleStyle.Render(preview(env.Body))})
	}
	cols := []column{{"AT", 8}, {"TYPE", 8}, {"FROM", 14}, {"STATE", 24}, {"BODY", 0}}
	lines = append(lines, grid(cols, rows, t.cursor, w, listH, t.mode == inboxList)...)
	lines = append(lines, "")
	if e := t.selected(); e != nil {
		env := e.Item.Env
		lines = append(lines, AccentStyle.Render(fmt.Sprintf("%s %s from %s", env.Type, Clean(env.ID), Clean(env.From))))
		switch t.mode {
		case inboxAnswer:
			lines = append(lines, SubtleStyle.Render("Reply body:"), t.body.View())
		case inboxRefuse:
			lines = append(lines, SubtleStyle.Render("Refuse with reason: ")+t.reason.View())
		default:
			rest := max(1, h-len(lines))
			body := strings.Split(strings.TrimRight(prettyJSON(env.Body), "\n"), "\n")
			if len(body) > rest {
				body = append(body[:rest-1], SubtleStyle.Render("… enter to see it all"))
			}
			lines = append(lines, body...)
		}
	}
	return strings.Join(lines, "\n")
}

// inboxState describes an entry: what a held request is waiting for, or
// what happened to it.
func (m *Console) inboxState(e *InboxEntry) string {
	env := e.Item.Env
	if out := e.outcome(); out != "" {
		if strings.HasPrefix(out, "answered") {
			return OKStyle.Render("✓ " + out)
		}
		return SubtleStyle.Render(Clean(out))
	}
	switch {
	case e.reply != nil && m.now.Before(e.Deadline):
		return WarnStyle.Render(fmt.Sprintf("waiting · %s left", e.Deadline.Sub(m.now).Round(time.Second)))
	case e.reply != nil:
		return SubtleStyle.Render("expired unanswered")
	case env.Type == a2a.TypeError:
		code, _ := errorParts(env)
		return ErrStyle.Render("✗ " + code)
	}
	return ""
}
