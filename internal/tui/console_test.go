package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

// fakeBus records what the console sends and answers requests.
type fakeBus struct {
	mu    sync.Mutex
	cards []a2a.Presence
	sent  []*a2a.Envelope
	reply func(e *a2a.Envelope) (*a2a.Envelope, error)
}

func (b *fakeBus) Agents() []a2a.Presence {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]a2a.Presence(nil), b.cards...)
}

func (b *fakeBus) record(typ a2a.Type, body any, opts []client.MsgOption) *a2a.Envelope {
	e, _ := a2a.New(typ, "me", body)
	for _, o := range opts {
		o(e)
	}
	b.mu.Lock()
	b.sent = append(b.sent, e)
	b.mu.Unlock()
	return e
}

func (b *fakeBus) roundTrip(e *a2a.Envelope) (*a2a.Envelope, error) {
	if b.reply != nil {
		return b.reply(e)
	}
	r, _ := e.Reply(a2a.TypeResponse, "worker", map[string]string{"op": "pong"})
	return r, nil
}

func (b *fakeBus) Send(_ context.Context, to string, body any, opts ...client.MsgOption) error {
	b.record(a2a.TypeEvent, body, opts).To = to
	return nil
}

func (b *fakeBus) Request(_ context.Context, to string, body any, opts ...client.MsgOption) (*a2a.Envelope, error) {
	e := b.record(a2a.TypeRequest, body, opts)
	e.To = to
	return b.roundTrip(e)
}

func (b *fakeBus) RequestTask(_ context.Context, capability string, body any, opts ...client.MsgOption) (*a2a.Envelope, error) {
	e := b.record(a2a.TypeRequest, body, opts)
	e.Capability = capability
	return b.roundTrip(e)
}

func (b *fakeBus) Emit(_ context.Context, name string, body any, opts ...client.MsgOption) error {
	b.record(a2a.TypeEvent, body, opts).Event = name
	return nil
}

func (b *fakeBus) last() *a2a.Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sent) == 0 {
		return nil
	}
	return b.sent[len(b.sent)-1]
}

func newTestConsole(t *testing.T, bus *fakeBus) *Console {
	t.Helper()
	if bus.cards == nil {
		bus.cards = cards()
	}
	return NewConsole(ConsoleOptions{Agent: "me", URL: "tcp://127.0.0.1:1883", Scope: "all agent traffic (observer)",
		Bus: bus, Status: make(chan broker.Status), Inbox: NewAnswerer("me")})
}

func ctrl(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

// typeText types s into m one key at a time.
func typeText(m tea.Model, s string) tea.Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

// run executes cmd and feeds its message back, as Bubble Tea would.
func run(m tea.Model, cmd tea.Cmd) tea.Model {
	if cmd == nil {
		return m
	}
	m, _ = m.Update(cmd())
	return m
}

func view(m tea.Model) string { return plain(m.View().Content) }

func wantAll(t *testing.T, what, v string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(v, w) {
			t.Errorf("%s missing %q:\n%s", what, w, v)
		}
	}
}

func TestConsoleTabsAndOverview(t *testing.T) {
	var m tea.Model = sizeTo(newTestConsole(t, &fakeBus{}), 160, 44)
	if v := view(m); !strings.Contains(v, "waiting for the first status snapshot") {
		t.Errorf("overview before status:\n%s", v)
	}
	now := time.Now()
	st := sampleStatus(now, 120)
	st.Requests = []broker.RequestStats{{Target: "task:textstats", Open: 1, Answered: 9, Errored: 1, Expired: 2, P50: 0.025, P95: 1.5}}
	m, _ = m.Update(StatusMsg(sampleStatus(now.Add(-2*time.Second), 100)))
	m, _ = m.Update(StatusMsg(st))
	wantAll(t, "overview", view(m), "1 Overview", "6 Config", "as me", "127.0.0.1:18830", "3 connected · 4 sessions", "10.0/s in",
		"accepted   40", "Throughput", "Requests by target", "task:textstats", "≤25ms", "≤1.5s", "256.0 KiB/envelope")

	for k, want := range map[string]string{"2": "known", "3": "Traffic", "4": "Compose", "5": "Inbox of me", "6": "Config"} {
		m, _ = press(m, "esc") // leave any form
		m, _ = press(m, "1", k)
		if v := view(m); !strings.Contains(v, want) {
			t.Errorf("tab %s missing %q:\n%s", k, want, v)
		}
	}
	m, _ = press(m, "esc", "1", "]")
	if m.(*Console).tab != TabAgents {
		t.Error("] did not move to the next tab")
	}
	m, _ = press(m, "[", "[")
	if m.(*Console).tab != TabMap {
		t.Error("[ did not wrap to the last tab")
	}
	m, _ = press(m, "?")
	wantAll(t, "help", view(m), "Keys", "Compose", "ctrl+s")
	m, _ = press(m, "x")
	if m.(*Console).help {
		t.Error("a key did not close the help")
	}
	if _, cmd := press(m, "q"); !isQuit(cmd) {
		t.Error("q did not quit")
	}
}

func TestConsoleWithoutObserver(t *testing.T) {
	c := NewConsole(ConsoleOptions{Agent: "you", Bus: &fakeBus{cards: cards()}, Scope: "events and your inbox (not an observer)"})
	var m tea.Model = sizeTo(c, 120, 30)
	wantAll(t, "overview", view(m), "need an observer identity", "1 agents online, 1 offline")
	m, _ = press(m, "2")
	wantAll(t, "agents", view(m), "worker", "coordinator", "broker columns need an observer")
	m, _ = press(m, "6")
	wantAll(t, "config", view(m), "isn't on this machine")
}

func TestConsoleAgents(t *testing.T) {
	bus := &fakeBus{}
	c := newTestConsole(t, bus)
	var m tea.Model = sizeTo(c, 160, 40)
	m, _ = m.Update(StatusMsg(sampleStatus(time.Now(), 5))) // worker, explorer, you
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "worker", "ping"))))
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "you", "explorer", "x"))))
	m, _ = press(m, "2")
	// Rows join status and presence: coordinator (card only), explorer, worker, you.
	wantAll(t, "agents", view(m), "coordinator", "explorer", "worker", "Demo worker", "127.0.0.1:5000", "no presence card")

	m, _ = press(m, "down", "down") // worker
	st := sampleStatus(time.Now(), 6)
	st.Agents = append([]broker.AgentStatus{{ID: "aaa", State: "online"}}, st.Agents...)
	m, _ = m.Update(StatusMsg(st))
	m, _ = press(m, "enter")
	v := view(m)
	wantAll(t, "detail", v, "Agent worker", "Presence card", "Demo worker", "1 messages involving worker", "coordinator → worker")
	if strings.Contains(v, "you → explorer") {
		t.Error("detail shows unrelated traffic")
	}
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "worker", "coordinator", "pong"))))
	wantAll(t, "detail after more traffic", view(m), "2 messages involving worker")
	m, _ = press(m, "up", "enter")
	wantAll(t, "inspector", view(m), "Inspect request")
	m, _ = press(m, "esc", "i")
	wantAll(t, "card", view(m), "presence card of worker")
	m, _ = press(m, "esc", "esc", "esc")
	if c.agents.detail != "" {
		t.Error("esc did not leave the detail")
	}

	m, _ = press(m, "f")
	if c.tab != TabTraffic || !strings.Contains(view(m), "only involving worker") {
		t.Errorf("f did not filter traffic:\n%s", view(m))
	}
	m, _ = press(m, "2", "t")
	if c.tab != TabCompose || c.compose.kind != kindTask || c.compose.target.Value() != "textstats" || !c.compose.editing {
		t.Errorf("t: tab %d kind %d target %q", c.tab, c.compose.kind, c.compose.target.Value())
	}
	m, _ = press(m, "esc", "2", "e")
	if v := view(m); c.tab != TabAgents || !strings.Contains(v, "isn't on this machine") {
		t.Errorf("e without a config should say why:\n%s", v)
	}
}

func TestConsoleTrafficFilters(t *testing.T) {
	var m tea.Model = sizeTo(newTestConsole(t, &fakeBus{}), 140, 30)
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "worker", "ping"))))
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "weather", "forecast"))))
	ev, _ := a2a.New(a2a.TypeEvent, "worker", map[string]string{"job": "done"})
	m, _ = m.Update(ItemMsg(ItemFromEnvelope(a2a.EventTopic("worker", "jobs"), ev)))
	m, _ = press(m, "3", "/")
	m = typeText(m, "forecast")
	if v := view(m); !strings.Contains(v, "/ forecast") {
		t.Errorf("filter input not shown:\n%s", v)
	}
	m, _ = press(m, "enter")
	v := view(m)
	wantAll(t, "text filter", v, `matching "forecast"`, "coordinator → weather", "1 shown")
	if strings.Contains(v, "coordinator → worker") {
		t.Error("text filter let other messages through")
	}
	m, _ = press(m, "esc", "t", "t", "t") // clear text; request → response → event
	v = view(m)
	if !strings.Contains(v, "only events") || strings.Contains(v, "coordinator → weather") || !strings.Contains(v, "worker → events/worker/jobs") {
		t.Errorf("type filter:\n%s", v)
	}
	m, _ = press(m, "esc")
	if c := m.(*Console); c.traffic.feed.filter != nil {
		t.Error("esc did not clear the type filter")
	}
	m, _ = press(m, "up", "R")
	c := m.(*Console)
	if c.tab != TabCompose || c.compose.kind != kindRequest || c.compose.target.Value() != "weather" ||
		!strings.Contains(c.compose.body.Value(), `"op": "forecast"`) {
		t.Errorf("R: tab %d kind %d target %q body %q", c.tab, c.compose.kind, c.compose.target.Value(), c.compose.body.Value())
	}
}

func TestConsoleCompose(t *testing.T) {
	bus := &fakeBus{}
	c := newTestConsole(t, bus)
	var m tea.Model = sizeTo(c, 140, 36)
	m, _ = press(m, "4")
	// The form has focus on arrival: digits are text, not tab switches.
	m = typeText(m, "wo1")
	if c.tab != TabCompose || c.compose.target.Value() != "wo1" {
		t.Fatalf("typing went to the console: tab %d target %q", c.tab, c.compose.target.Value())
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m, _ = press(m, "tab") // completes "wo" to "worker"
	if got := c.compose.target.Value(); got != "worker" {
		t.Errorf("completion = %q", got)
	}
	m, _ = press(m, "tab") // body
	m = typeText(m, `{"op": `)
	m, cmd := m.Update(ctrl('s'))
	if cmd != nil || !strings.Contains(view(m), "not valid JSON") {
		t.Errorf("invalid body sent, or no error shown:\n%s", view(m))
	}
	m = typeText(m, `"ping"}`)
	m, cmd = m.Update(ctrl('s'))
	if c.compose.inFlight() != 1 || !strings.Contains(view(m), "4 Compose (1)") {
		t.Errorf("no in-flight count:\n%s", view(m))
	}
	m = run(m, cmd)
	if e := bus.last(); e == nil || e.To != "worker" || string(e.Body) != `{"op":"ping"}` {
		t.Fatalf("sent %+v", e)
	}
	m, _ = press(m, "esc")
	wantAll(t, "history", view(m), "request worker", "✓ response", "from worker", `"op": "pong"`)

	// A refusal from the far end.
	bus.reply = func(e *a2a.Envelope) (*a2a.Envelope, error) {
		r, _ := e.Reply(a2a.TypeError, "worker", client.ErrorBody{Code: "not_found", Reason: "no such op"})
		return r, &client.RemoteError{Code: "not_found", Reason: "no such op", From: "worker"}
	}
	m, _ = press(m, "e")
	m, cmd = m.Update(ctrl('s'))
	m = run(m, cmd)
	m, _ = press(m, "esc")
	wantAll(t, "error reply", view(m), "✗ not_found", "no such op")

	// A send the broker refuses arrives in the inbox as an error; the
	// history matches it to the send.
	m, _ = press(m, "e")
	c.compose.field = fieldKind
	m, _ = press(m, "right", "right") // request → task → send
	m, cmd = m.Update(ctrl('s'))
	m = run(m, cmd)
	sentEnv := bus.last()
	if sentEnv.Type != a2a.TypeEvent || sentEnv.To != "worker" {
		t.Fatalf("send: %+v", sentEnv)
	}
	rej, _ := sentEnv.Reply(a2a.TypeError, a2a.BrokerID, client.ErrorBody{Code: "no_route", Reason: "you may not send to worker"})
	m, _ = m.Update(InboxMsg{Entry: &InboxEntry{Item: ItemFromEnvelope("t", rej), At: time.Now()}})
	m, _ = press(m, "esc")
	wantAll(t, "send refused", view(m), "send worker", "✗ no_route", "$broker")

	// R puts a past message back in the form.
	m, _ = press(m, "down", "down", "R")
	if c.compose.kind != kindRequest || !c.compose.editing || !strings.Contains(c.compose.body.Value(), "ping") {
		t.Errorf("R: kind %d editing %v body %q", c.compose.kind, c.compose.editing, c.compose.body.Value())
	}
}

func TestConsoleInboxAnswers(t *testing.T) {
	c := newTestConsole(t, &fakeBus{})
	a := c.opt.Inbox
	var m tea.Model = sizeTo(c, 140, 36)
	m, _ = press(m, "5")

	ask := func(op string) (chan any, chan error) {
		body, errs := make(chan any, 1), make(chan error, 1)
		req := env(t, "coordinator", "me", op)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		go func() {
			defer cancel()
			b, err := a.Handle(ctx, req)
			body <- b
			errs <- err
		}()
		m, _ = m.Update(InboxMsg{Entry: <-a.C()})
		return body, errs
	}

	body, errs := ask("approve")
	v := view(m)
	wantAll(t, "held request", v, "5 Inbox (1)", "coordinator", "waiting ·", `"op": "approve"`, "holding requests")
	m, _ = press(m, "a")
	m = typeText(m, `{"approved": true}`)
	m, _ = m.Update(ctrl('s'))
	if b := <-body; string(b.(json.RawMessage)) != `{"approved": true}` || <-errs != nil {
		t.Errorf("answer = %s", b)
	}
	wantAll(t, "answered", view(m), "✓ answered")

	_, errs = ask("delete-everything")
	m, _ = press(m, "x")
	m = typeText(m, "not today")
	m, _ = press(m, "enter")
	var re *client.ReplyError
	if err := <-errs; !errors.As(err, &re) || re.Code != "refused" || re.Reason != "not today" {
		t.Errorf("refusal = %v", err)
	}
	if _, _ = press(m, "a"); !strings.Contains(view(m), "waiting for an answer") {
		t.Errorf("answering a finished request should say why not:\n%s", view(m))
	}

	m, _ = press(m, "h")
	if a.Holding() {
		t.Fatal("h did not stop holding")
	}
	_, errs = ask("again")
	if err := <-errs; !errors.As(err, &re) || re.Code != "not_answering" {
		t.Errorf("not holding: %v", err)
	}
	wantAll(t, "inbox", view(m), "refusing requests at once", "refused at once")
}

func TestAnswererExpires(t *testing.T) {
	a := NewAnswerer("me")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Handle(ctx, env(t, "coordinator", "me", "slow"))
		done <- err
	}()
	e := <-a.C()
	var re *client.ReplyError
	if err := <-done; !errors.As(err, &re) || re.Code != "timeout" {
		t.Errorf("expiry = %v", err)
	}
	if e.pending() || e.resolve(answer{}, "answered") {
		t.Error("an expired request can still be answered")
	}
}

func TestConsoleConfigTab(t *testing.T) {
	_, loc := editorAt(t)
	tx, err := confstore.Begin(loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AddAgent(broker.AgentConfig{ID: "worker", Capabilities: []string{"textstats"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	c := newTestConsole(t, &fakeBus{})
	c.opt.Config = func() (*ConfigModel, error) { return NewConfigEditor(loc, "") }
	var m tea.Model = sizeTo(c, 140, 36)
	m, _ = press(m, "2", "down", "down", "e") // worker (card) → edit in config
	if c.tab != TabConfig || c.config == nil {
		t.Fatalf("e did not open the config tab")
	}
	if sel, _ := c.config.selectedAgent(); sel.ID != "worker" {
		t.Errorf("config selected %q", sel.ID)
	}
	v := view(m)
	wantAll(t, "config tab", v, "Config", "worker", "serves textstats", "1-7 other tabs")
	if strings.Contains(v, "agentbus · config") {
		t.Error("the tab repeats the editor's title bar")
	}
	m, _ = press(m, "esc")
	if c.tab != TabConfig {
		t.Error("esc in the editor list left the tab")
	}
	m, _ = press(m, "a") // add form: keys are text now
	m = typeText(m, "1")
	if c.tab != TabConfig || c.config.mode != modeForm {
		t.Errorf("typing in the form switched tabs (tab %d)", c.tab)
	}
	m, _ = press(m, "esc")
	if c.config.mode != modeList {
		t.Error("esc did not cancel the form")
	}
	m, _ = press(m, "1")
	if c.tab != TabOverview {
		t.Error("1 did not switch tabs from the editor list")
	}
	checkFits(t, "config tab", m.View().Content, 140, 36)
}

func TestConsoleFitsWindow(t *testing.T) {
	long := envelope(t)
	long.Body = []byte(`{"text":"` + strings.Repeat("very long body text ", 30) + `"}`)
	var many []a2a.Presence
	for i := range 60 {
		many = append(many, a2a.Presence{Agent: strings.Repeat("a", i%20+1) + "-agent", Status: a2a.StatusOnline,
			Description: strings.Repeat("d", 200)})
	}
	for _, size := range [][2]int{{80, 24}, {120, 30}, {200, 50}} {
		w, h := size[0], size[1]
		c := newTestConsole(t, &fakeBus{cards: many})
		var m tea.Model = sizeTo(c, w, h)
		st := sampleStatus(time.Now(), 5)
		st.DataDir = "/very/long/path/" + strings.Repeat("nested/", 20) + "data"
		for i := range 30 {
			st.Requests = append(st.Requests, broker.RequestStats{Target: "task:" + strings.Repeat("x", i+1), Answered: int64(i)})
		}
		m, _ = m.Update(StatusMsg(st))
		m, _ = m.Update(consoleTickMsg(time.Now()))
		for range 100 {
			m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", long)))
		}
		for _, tab := range []string{"1", "2", "3", "4", "5", "6", "7"} {
			m, _ = press(m, "esc")
			m, _ = press(m, tab)
			checkFits(t, "tab "+tab, m.View().Content, w, h)
		}
		m, _ = press(m, "esc", "2")
		for range 40 {
			m, _ = press(m, "down")
		}
		if !strings.Contains(view(m), "▌") {
			t.Errorf("%dx%d: cursor row scrolled out of view", w, h)
		}
		m, _ = press(m, "enter")
		checkFits(t, "agent detail", m.View().Content, w, h)
		m, _ = press(m, "?")
		checkFits(t, "help", m.View().Content, w, h)
	}
}

// The Map tab draws each message on its way through the broker: requests
// and replies between agents, a task tagged at the broker, an event's
// ripple, and a refusal stamped with its code.
func TestConsoleMap(t *testing.T) {
	var m tea.Model = sizeTo(newTestConsole(t, &fakeBus{}), 120, 32)
	m, _ = press(m, "7")
	c := m.(*Console)
	if c.tab != TabMap {
		t.Fatal("7 did not open the Map tab")
	}
	v := view(m)
	wantAll(t, "empty map", v, "7 Map", "broker", "coordinator", "worker", "last 10s: 0 req")

	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "weather", "forecast"))))
	task, _ := a2a.New(a2a.TypeRequest, "coordinator", map[string]string{"text": "a b"})
	task.Capability = "textstats"
	m, _ = m.Update(ItemMsg(ItemFromEnvelope(a2a.TaskTopic("textstats"), task)))
	ev, _ := a2a.New(a2a.TypeEvent, "worker", map[string]string{"job": "done"})
	m, _ = m.Update(ItemMsg(ItemFromEnvelope(a2a.EventTopic("worker", "jobs"), ev)))
	refused, _ := a2a.New(a2a.TypeError, a2a.BrokerID, map[string]string{"code": "not_authorized", "reason": "not allowed to send to x"})
	refused.To = "coordinator"
	m, _ = m.Update(ItemMsg(ItemFromEnvelope(a2a.InboxTopic("coordinator"), refused)))
	if n := len(c.bus.packets); n != 4 {
		t.Fatalf("%d packets in flight, want 4", n)
	}

	// Halfway along the first leg the packets are between agents and the
	// broker; drawn in braille.
	for i := range c.bus.packets {
		c.bus.packets[i].born = c.bus.packets[i].born.Add(-mapLeg / 2)
	}
	v = view(m)
	wantAll(t, "in flight", v, "last 10s: 2 req", "weather", "1 event", "1 refused", "✗ not_authorized")
	if !strings.ContainsFunc(v, func(r rune) bool { return r > 0x2800 && r <= 0x28FF }) {
		t.Error("no braille in the map")
	}

	// Past the broker: the task is tagged and stops; the event ripples.
	for i := range c.bus.packets {
		c.bus.packets[i].born = c.bus.packets[i].born.Add(-mapLeg)
	}
	v = view(m)
	wantAll(t, "past the broker", v, "⇢ textstats")
	if len(c.bus.ripples) != 1 {
		t.Errorf("%d ripples, want 1 for the event", len(c.bus.ripples))
	}
	// The refusal started at the broker, so it has already reached the
	// sender; the request is on its way to weather.
	if n := len(c.bus.packets); n != 1 || c.bus.packets[0].to != "weather" {
		t.Errorf("packets left: %+v, want only the request on its second leg", c.bus.packets)
	}

	// p freezes the map; new messages aren't added while paused, and the
	// frame ticks stop.
	m, _ = press(m, "p")
	wantAll(t, "paused", view(m), "paused", "p resume")
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "weather", "again"))))
	if n := len(c.bus.packets); n != 1 {
		t.Errorf("a message was added while paused: %d packets", n)
	}
	if _, cmd := m.Update(mapTickMsg(time.Now())); cmd != nil {
		t.Error("frames kept ticking while paused")
	}
	if _, cmd := press(m, "p"); cmd == nil {
		t.Error("resuming did not restart the frame ticks")
	}
	m, _ = press(m, "1")
	if _, cmd := m.Update(mapTickMsg(time.Now())); cmd != nil {
		t.Error("frames kept ticking on another tab")
	}
	checkFits(t, "map", view(m), 120, 32)
}
