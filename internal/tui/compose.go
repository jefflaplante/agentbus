package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

type msgKind int

const (
	kindRequest msgKind = iota // request to an agent's inbox; waits for the reply
	kindTask                   // request to a capability's queue; waits for the reply
	kindSend                   // one-way event to an agent's inbox
	kindEmit                   // broadcast on this agent's event topic
	numKinds
)

var (
	kindNames   = [numKinds]string{"request", "task", "send", "emit"}
	targetLabel = [numKinds]string{"To agent", "Capability", "To agent", "Event name"}
	priorities  = []a2a.Priority{"", a2a.PriorityHigh, a2a.PriorityNormal, a2a.PriorityLow, a2a.PriorityInfo}
)

// The compose form's fields, in tab order.
const (
	fieldKind = iota
	fieldTarget
	fieldBody
	fieldPriority
	fieldEvent
	fieldHeaders
	fieldTimeout
	numFields
)

// sent is one message sent from the Compose tab, and what came of it.
type sent struct {
	id       int
	kind     msgKind
	target   string
	body     json.RawMessage
	priority int
	event    string
	headers  string
	timeout  string

	envID   atomic.Value // string: the envelope ID, set while sending
	start   time.Time
	done    bool
	elapsed time.Duration
	reply   *a2a.Envelope // the response or error, if any
	err     error
	cancel  context.CancelFunc
}

func (s *sent) envelopeID() string {
	id, _ := s.envID.Load().(string)
	return id
}

// composeDoneMsg reports the outcome of a send.
type composeDoneMsg struct {
	id      int
	reply   *a2a.Envelope
	err     error
	elapsed time.Duration
}

// composeTab is a form for sending any kind of message, and the history
// of what was sent with each reply.
type composeTab struct {
	editing  bool
	field    int
	kind     msgKind
	priority int
	target   textinput.Model
	event    textinput.Model
	headers  textinput.Model
	timeout  textinput.Model
	body     textarea.Model

	history []*sent // newest first
	cursor  int
	nextID  int
	width   int
	height  int
}

func newComposeTab() composeTab {
	in := func(placeholder string) textinput.Model {
		t := textinput.New()
		t.Prompt = ""
		t.Placeholder = placeholder
		return t
	}
	c := composeTab{
		target:  in("worker"),
		event:   in("optional, e.g. doorbell_ring"),
		headers: in("optional, key=value, key=value"),
		timeout: in("60s"),
		body:    textarea.New(),
		editing: true,
		field:   fieldTarget,
	}
	c.timeout.SetValue("60s")
	c.body.Placeholder = `{"op": "ping"}`
	c.body.ShowLineNumbers = false
	c.body.Prompt = "│ "
	c.restyle()
	c.resize(120, 28)
	c.focus()
	return c
}

func (c *composeTab) restyle() {
	c.body.SetStyles(textarea.DefaultStyles(pal == darkPalette))
	st := textinput.DefaultStyles(pal == darkPalette)
	for _, t := range []*textinput.Model{&c.target, &c.event, &c.headers, &c.timeout} {
		t.SetStyles(st)
	}
}

// formWidth is the width of the form column; the rest shows results.
func (c *composeTab) formWidth() int {
	if c.width >= 110 {
		return c.width * 11 / 20
	}
	return c.width
}

func (c *composeTab) resize(w, h int) {
	c.width, c.height = w, h
	fw := c.formWidth() - 13
	for _, t := range []*textinput.Model{&c.target, &c.event, &c.headers, &c.timeout} {
		t.SetWidth(max(10, fw))
	}
	c.body.SetWidth(max(10, fw))
	bodyH := max(3, min(10, h-16))
	if c.width < 110 {
		bodyH = max(3, min(8, h/2-9))
	}
	c.body.SetHeight(bodyH)
}

// focus gives the current field the cursor.
func (c *composeTab) focus() tea.Cmd {
	for _, t := range []*textinput.Model{&c.target, &c.event, &c.headers, &c.timeout} {
		t.Blur()
	}
	c.body.Blur()
	if !c.editing {
		return nil
	}
	switch c.field {
	case fieldTarget:
		return c.target.Focus()
	case fieldBody:
		return c.body.Focus()
	case fieldEvent:
		return c.event.Focus()
	case fieldHeaders:
		return c.headers.Focus()
	case fieldTimeout:
		return c.timeout.Focus()
	}
	return nil
}

func (c *composeTab) move(delta int) tea.Cmd {
	c.field = (c.field + delta + numFields) % numFields
	if c.field == fieldTimeout && (c.kind == kindSend || c.kind == kindEmit) {
		c.field = (c.field + delta + numFields) % numFields // no reply to wait for
	}
	return c.focus()
}

func (c *composeTab) inFlight() int {
	n := 0
	for _, s := range c.history {
		if !s.done {
			n++
		}
	}
	return n
}

// done records the outcome of a send.
func (c *composeTab) done(msg composeDoneMsg) {
	for _, s := range c.history {
		if s.id == msg.id {
			s.done, s.elapsed, s.err = true, msg.elapsed, msg.err
			if msg.reply != nil {
				s.reply = msg.reply
			}
			if s.cancel != nil {
				s.cancel()
			}
		}
	}
}

// noteReply matches a message from the inbox to a send or emit: the
// broker rejects those with an error envelope to the sender's inbox.
func (c *composeTab) noteReply(e *a2a.Envelope) {
	if e == nil || e.CorrelationID == "" || (e.Type != a2a.TypeError && e.Type != a2a.TypeResponse) {
		return
	}
	for _, s := range c.history {
		if (s.kind == kindSend || s.kind == kindEmit) && s.envelopeID() == e.CorrelationID {
			s.reply = e
		}
	}
}

// load fills the form from a past message.
func (c *composeTab) load(kind msgKind, target string, body []byte, priority a2a.Priority, event string, headers map[string]string) {
	c.kind = kind
	c.target.SetValue(target)
	c.body.SetValue(prettyBody(body))
	c.priority = max(0, slices.Index(priorities, priority))
	c.event.SetValue(event)
	var hs []string
	for k, v := range headers {
		hs = append(hs, k+"="+v)
	}
	slices.Sort(hs)
	c.headers.SetValue(strings.Join(hs, ", "))
}

// prettyBody indents a JSON body for editing.
func prettyBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var out bytes.Buffer
	if json.Indent(&out, b, "", "  ") != nil {
		return string(b)
	}
	return out.String()
}

// composeTo opens the form to message target.
func (m *Console) composeTo(kind msgKind, target string) tea.Cmd {
	c := &m.compose
	c.kind = kind
	c.target.SetValue(target)
	c.editing, c.field = true, fieldBody
	m.tab = TabCompose
	return c.focus()
}

// composeFrom opens the form with a copy of e.
func (m *Console) composeFrom(e *a2a.Envelope) tea.Cmd {
	c := &m.compose
	kind, target := kindSend, e.To
	switch {
	case e.Type == a2a.TypeRequest && e.Capability != "":
		kind, target = kindTask, e.Capability
	case e.Type == a2a.TypeRequest:
		kind = kindRequest
	case e.To == "":
		kind, target = kindEmit, e.Event
	}
	c.load(kind, target, e.Body, e.Priority, e.Event, e.Headers)
	c.editing, c.field = true, fieldBody
	m.tab = TabCompose
	m.say(false, "copied %s %s into the form; ctrl+s sends it", Clean(string(e.Type)), Clean(e.ID))
	return c.focus()
}

// knownTargets lists what the target field can complete to.
func (m *Console) knownTargets() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && s != "*" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, r := range m.rows() {
		if m.compose.kind == kindTask {
			for _, c := range r.capabilities() {
				add(c)
			}
		} else if m.compose.kind != kindEmit {
			add(r.id)
		}
	}
	slices.Sort(out)
	return out
}

// complete extends the target to the longest prefix shared by the known
// targets it starts, reporting whether it changed anything.
func (m *Console) complete() bool {
	c := &m.compose
	v := c.target.Value()
	var match []string
	for _, t := range m.knownTargets() {
		if strings.HasPrefix(t, v) {
			match = append(match, t)
		}
	}
	if len(match) == 0 {
		return false
	}
	common := match[0]
	for _, t := range match[1:] {
		for !strings.HasPrefix(t, common) {
			common = common[:len(common)-1]
		}
	}
	if common == v {
		return false
	}
	c.target.SetValue(common)
	c.target.CursorEnd()
	return true
}

func (m *Console) composeUpdate(msg tea.Msg) tea.Cmd {
	c := &m.compose
	k, isKey := msg.(tea.KeyPressMsg)
	if !c.editing {
		if !isKey {
			return nil
		}
		switch k.String() {
		case "e", "n", "i", "tab":
			c.editing = true
			return c.focus()
		case "up", "k":
			c.cursor = max(0, c.cursor-1)
		case "down", "j":
			c.cursor = min(len(c.history)-1, c.cursor+1)
		case "enter":
			if s := c.selected(); s != nil && s.reply != nil {
				m.openInspector(ItemFromEnvelope(a2a.InboxTopic(m.opt.Agent), s.reply))
			}
		case "R":
			if s := c.selected(); s != nil {
				var h map[string]string
				c.load(s.kind, s.target, s.body, priorities[s.priority], s.event, h)
				c.headers.SetValue(s.headers)
				c.timeout.SetValue(s.timeout)
				c.editing, c.field = true, fieldBody
				return c.focus()
			}
		case "x":
			if s := c.selected(); s != nil && !s.done && s.cancel != nil {
				s.cancel()
			}
		}
		return nil
	}
	if !isKey {
		return c.forward(msg)
	}
	switch k.String() {
	case "esc":
		c.editing = false
		return c.focus()
	case "ctrl+s":
		return m.composeSend()
	case "tab":
		if c.field == fieldTarget && m.complete() {
			return nil
		}
		return c.move(1)
	case "shift+tab":
		return c.move(-1)
	}
	switch c.field {
	case fieldKind, fieldPriority:
		step := 0
		switch k.String() {
		case "right", "l", "space":
			step = 1
		case "left", "h":
			step = -1
		case "enter", "down", "j":
			return c.move(1)
		case "up", "k":
			return c.move(-1)
		}
		if c.field == fieldKind {
			c.kind = msgKind((int(c.kind) + step + int(numKinds)) % int(numKinds))
		} else {
			c.priority = (c.priority + step + len(priorities)) % len(priorities)
		}
		return nil
	case fieldBody:
		return c.forward(msg)
	}
	switch k.String() {
	case "enter", "down":
		return c.move(1)
	case "up":
		return c.move(-1)
	}
	return c.forward(msg)
}

func (c *composeTab) forward(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch c.field {
	case fieldTarget:
		c.target, cmd = c.target.Update(msg)
	case fieldBody:
		c.body, cmd = c.body.Update(msg)
	case fieldEvent:
		c.event, cmd = c.event.Update(msg)
	case fieldHeaders:
		c.headers, cmd = c.headers.Update(msg)
	case fieldTimeout:
		c.timeout, cmd = c.timeout.Update(msg)
	}
	return cmd
}

func (c *composeTab) selected() *sent {
	if c.cursor < 0 || c.cursor >= len(c.history) {
		return nil
	}
	return c.history[c.cursor]
}

// bodyJSON validates the body field: blank, or JSON.
func (c *composeTab) bodyJSON() (json.RawMessage, error) {
	s := strings.TrimSpace(c.body.Value())
	if s == "" {
		return nil, nil
	}
	if !json.Valid([]byte(s)) {
		var v any
		err := json.Unmarshal([]byte(s), &v)
		return nil, fmt.Errorf("body is not valid JSON: %v", err)
	}
	var out bytes.Buffer
	_ = json.Compact(&out, []byte(s))
	return out.Bytes(), nil
}

func parseHeaders(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	h := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("header %q: want key=value", strings.TrimSpace(kv))
		}
		h[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return h, nil
}

// composeSend validates the form and sends the message.
func (m *Console) composeSend() tea.Cmd {
	c := &m.compose
	if m.opt.Bus == nil {
		m.say(true, "not connected")
		return nil
	}
	target := strings.TrimSpace(c.target.Value())
	if target == "" {
		m.say(true, "%s is required", strings.ToLower(targetLabel[c.kind]))
		return nil
	}
	if c.kind != kindEmit && !a2a.ValidName(target) {
		m.say(true, "%q: names are 1-64 of a-z 0-9 . _ -", target)
		return nil
	}
	body, err := c.bodyJSON()
	if err != nil {
		m.say(true, "%v", err)
		return nil
	}
	headers, err := parseHeaders(c.headers.Value())
	if err != nil {
		m.say(true, "%v", err)
		return nil
	}
	timeout := 60 * time.Second
	if c.kind == kindRequest || c.kind == kindTask {
		if v := strings.TrimSpace(c.timeout.Value()); v != "" {
			if timeout, err = time.ParseDuration(v); err != nil || timeout <= 0 {
				m.say(true, "timeout %q: use a duration such as 30s or 2m", v)
				return nil
			}
		}
	}
	s := &sent{kind: c.kind, target: target, body: body, priority: c.priority, event: strings.TrimSpace(c.event.Value()),
		headers: c.headers.Value(), timeout: c.timeout.Value(), start: time.Now()}
	c.nextID++
	s.id = c.nextID
	c.history = append([]*sent{s}, c.history...)
	c.cursor = 0

	opts := []client.MsgOption{func(e *a2a.Envelope) { s.envID.Store(e.ID) }}
	if p := priorities[c.priority]; p != "" {
		opts = append(opts, client.WithPriority(p))
	}
	if s.event != "" {
		opts = append(opts, client.WithEvent(s.event))
	}
	if headers != nil {
		opts = append(opts, client.WithHeaders(headers))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	s.cancel = cancel
	bus, kind, id := m.opt.Bus, s.kind, s.id
	var payload any
	if body != nil {
		payload = body
	}
	m.say(false, "sent %s to %s", kindNames[kind], target)
	return func() tea.Msg {
		start := time.Now()
		var reply *a2a.Envelope
		var err error
		switch kind {
		case kindRequest:
			reply, err = bus.Request(ctx, target, payload, opts...)
		case kindTask:
			reply, err = bus.RequestTask(ctx, target, payload, opts...)
		case kindSend:
			err = bus.Send(ctx, target, payload, opts...)
		case kindEmit:
			err = bus.Emit(ctx, target, payload, opts...)
		}
		return composeDoneMsg{id: id, reply: reply, err: err, elapsed: time.Since(start)}
	}
}

func (m *Console) renderCompose(w, h int) string {
	c := &m.compose
	form := m.renderForm(c.formWidth())
	if c.width >= 110 {
		right := m.renderHistory(w-c.formWidth()-2, h)
		return lipgloss.JoinHorizontal(lipgloss.Top, fitBlock(form, c.formWidth(), h), "  ", right)
	}
	formH := lipgloss.Height(form)
	return form + "\n" + m.renderHistory(w, max(3, h-formH-1))
}

func (m *Console) renderForm(w int) string {
	c := &m.compose
	label := func(field int, name string) string {
		l := fmt.Sprintf("%-11s", name)
		if c.editing && c.field == field {
			return AccentStyle.Render("▌" + l)
		}
		return SubtleStyle.Render(" " + l)
	}
	choice := func(names []string, cur int) string {
		parts := make([]string, len(names))
		for i, n := range names {
			if i == cur {
				parts[i] = AccentStyle.Render("‹" + n + "›")
			} else {
				parts[i] = SubtleStyle.Render(" " + n + " ")
			}
		}
		return strings.Join(parts, " ")
	}
	var prio []string
	for _, p := range priorities {
		if p == "" {
			prio = append(prio, "default")
		} else {
			prio = append(prio, string(p))
		}
	}
	title := AccentStyle.Render("Compose") + SubtleStyle.Render("  as "+Clean(m.opt.Agent))
	if !c.editing {
		title += SubtleStyle.Render(" · e to edit")
	}
	lines := []string{title, "",
		label(fieldKind, "Kind") + choice(kindNames[:], int(c.kind)),
		label(fieldTarget, targetLabel[c.kind]) + c.target.View(),
	}
	if c.editing && c.field == fieldTarget {
		if known := m.knownTargets(); len(known) > 0 {
			lines = append(lines, SubtleStyle.Render(strings.Repeat(" ", 12)+"tab completes: "+Clean(strings.Join(known, ", "))))
		}
	}
	bodyLines := strings.Split(c.body.View(), "\n")
	for i, l := range bodyLines {
		if i == 0 {
			lines = append(lines, label(fieldBody, "Body")+l)
		} else {
			lines = append(lines, strings.Repeat(" ", 12)+l)
		}
	}
	if _, err := c.bodyJSON(); err != nil {
		lines = append(lines, strings.Repeat(" ", 12)+ErrStyle.Render("✗ ")+SubtleStyle.Render(strings.TrimPrefix(err.Error(), "body is not valid JSON: ")))
	} else if strings.TrimSpace(c.body.Value()) != "" {
		lines = append(lines, strings.Repeat(" ", 12)+OKStyle.Render("✓ ")+SubtleStyle.Render("JSON"))
	} else {
		lines = append(lines, strings.Repeat(" ", 12)+SubtleStyle.Render("blank sends no body"))
	}
	lines = append(lines,
		label(fieldPriority, "Priority")+choice(prio, c.priority),
		label(fieldEvent, "Event")+c.event.View(),
		label(fieldHeaders, "Headers")+c.headers.View(),
	)
	if c.kind == kindRequest || c.kind == kindTask {
		lines = append(lines, label(fieldTimeout, "Timeout")+c.timeout.View())
	}
	lines = append(lines, "", strings.Repeat(" ", 12)+AccentStyle.Render("ctrl+s")+SubtleStyle.Render(" send "+kindNames[c.kind]))
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "…")
	}
	return strings.Join(lines, "\n")
}

func (m *Console) renderHistory(w, h int) string {
	c := &m.compose
	head := AccentStyle.Render("Sent") + SubtleStyle.Render(fmt.Sprintf("  %d this session", len(c.history)))
	if len(c.history) == 0 {
		return head + "\n" + SubtleStyle.Render("nothing yet: fill in the form and press ctrl+s")
	}
	listH := min(len(c.history)+1, max(3, h/2))
	rows := make([][]string, 0, len(c.history))
	for _, s := range c.history {
		rows = append(rows, []string{s.start.Local().Format("15:04:05"), kindNames[s.kind] + " " + Clean(s.target), m.outcome(s)})
	}
	cols := []column{{"SENT", 8}, {"MESSAGE", min(28, w/3)}, {"OUTCOME", 0}}
	lines := []string{head}
	lines = append(lines, grid(cols, rows, c.cursor, w, listH, !c.editing)...)
	lines = append(lines, "")
	if s := c.selected(); s != nil {
		lines = append(lines, m.renderOutcome(s, w, max(2, h-len(lines)))...)
	}
	return strings.Join(lines, "\n")
}

// outcome is a one-line summary of what happened to s.
func (m *Console) outcome(s *sent) string {
	var re *client.RemoteError
	switch {
	case s.reply != nil && s.reply.Type == a2a.TypeError:
		code, reason := errorParts(s.reply)
		return ErrStyle.Render("✗ "+code) + SubtleStyle.Render(" from "+Clean(s.reply.From)+" · "+reason)
	case !s.done:
		return WarnStyle.Render(fmt.Sprintf("… waiting %s", m.now.Sub(s.start).Round(time.Second)))
	case errors.As(s.err, &re):
		return ErrStyle.Render("✗ "+Clean(re.Code)) + SubtleStyle.Render(" from "+Clean(re.From))
	case errors.Is(s.err, context.DeadlineExceeded):
		return ErrStyle.Render("✗ no reply") + SubtleStyle.Render(" within "+s.elapsed.Round(time.Millisecond).String())
	case errors.Is(s.err, context.Canceled):
		return SubtleStyle.Render("canceled")
	case s.err != nil:
		return ErrStyle.Render("✗ " + Clean(s.err.Error()))
	case s.reply != nil:
		return OKStyle.Render("✓ "+Clean(string(s.reply.Type))) + SubtleStyle.Render(" from "+Clean(s.reply.From)+" in "+s.elapsed.Round(time.Millisecond).String()+"  "+preview(s.reply.Body))
	}
	return OKStyle.Render("✓ published") + SubtleStyle.Render(" in "+s.elapsed.Round(time.Millisecond).String())
}

func errorParts(e *a2a.Envelope) (code, reason string) {
	var b client.ErrorBody
	_ = json.Unmarshal(e.Body, &b)
	return Clean(b.Code), Clean(b.Reason)
}

// renderOutcome shows the selected message's reply in full.
func (m *Console) renderOutcome(s *sent, w, h int) []string {
	lines := []string{AccentStyle.Render(kindNames[s.kind]+" "+Clean(s.target)) + SubtleStyle.Render("  "+Clean(s.envelopeID()))}
	switch {
	case s.reply != nil:
		lines = append(lines, SubtleStyle.Render(fmt.Sprintf("%s from %s · enter inspects", s.reply.Type, Clean(s.reply.From))))
		lines = append(lines, strings.Split(strings.TrimRight(prettyJSON(s.reply.Body), "\n"), "\n")...)
	case !s.done:
		lines = append(lines, SubtleStyle.Render("waiting for the reply · x cancels"))
	case s.err != nil:
		lines = append(lines, ErrStyle.Render(Clean(s.err.Error())))
	case s.kind == kindSend:
		lines = append(lines, SubtleStyle.Render("delivered to the broker; send has no reply. A refusal would show here."))
	default:
		lines = append(lines, SubtleStyle.Render("published; events have no reply"))
	}
	if len(lines) > h {
		lines = append(lines[:h-1], SubtleStyle.Render("… enter to see it all"))
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "…")
	}
	return lines
}
