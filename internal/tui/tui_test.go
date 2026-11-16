package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
)

// plain strips colors so tests can compare text.
func plain(s string) string { return ansi.Strip(s) }

func envelope(t *testing.T) *a2a.Envelope {
	t.Helper()
	e, err := a2a.New(a2a.TypeRequest, "coordinator", map[string]string{"op": "ping"})
	if err != nil {
		t.Fatal(err)
	}
	e.To, e.Priority, e.Event = "worker", a2a.PriorityHigh, "doorbell_ring"
	return e
}

func TestFormatItem(t *testing.T) {
	env := envelope(t)
	line := plain(FormatItem(ItemFromEnvelope(a2a.InboxTopic("worker"), env), 0))
	for _, want := range []string{"request", "coordinator → worker", "high", "doorbell_ring", `{"op":"ping"}`} {
		if !strings.Contains(line, want) {
			t.Errorf("envelope line %q missing %q", line, want)
		}
	}

	card := `{"v":1,"agent":"worker","status":"online","capabilities":["textstats"]}`
	line = plain(FormatItem(ItemFromMessage(a2a.PresenceTopic("worker"), []byte(card)), 0))
	for _, want := range []string{"presence", "worker", "online", "[textstats]"} {
		if !strings.Contains(line, want) {
			t.Errorf("presence line %q missing %q", line, want)
		}
	}

	line = plain(FormatItem(ItemFromMessage("$SYS/broker/uptime", []byte("42")), 0))
	if !strings.Contains(line, "raw") || !strings.Contains(line, "$SYS/broker/uptime") {
		t.Errorf("raw line %q", line)
	}

	if w := ansi.StringWidth(FormatItem(ItemFromEnvelope("t", env), 30)); w > 30 {
		t.Errorf("line is %d cells wide, want <= 30", w)
	}
}

func TestFeedModel(t *testing.T) {
	ch := make(chan Item, 4)
	var m tea.Model = NewFeed("watch", ch)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	m, cmd := m.Update(ItemMsg(ItemFromEnvelope("t", envelope(t))))
	if cmd == nil {
		t.Error("feed stopped reading after an item")
	}
	if v := plain(m.View().Content); !strings.Contains(v, "1 messages") || !strings.Contains(v, "coordinator → worker") {
		t.Errorf("view after one item:\n%s", v)
	}

	m, _ = m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", envelope(t))))
	if v := plain(m.View().Content); !strings.Contains(v, "paused (1 not shown)") {
		t.Errorf("pause not shown:\n%s", v)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if v := plain(m.View().Content); !strings.Contains(v, "waiting for messages") {
		t.Errorf("clear did not empty the feed:\n%s", v)
	}
	m, _ = m.Update(sourceClosedMsg{})
	if v := plain(m.View().Content); !strings.Contains(v, "disconnected") {
		t.Errorf("closed source not shown:\n%s", v)
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); !isQuit(cmd) {
		t.Error("q did not quit")
	}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func cards() []a2a.Presence {
	return []a2a.Presence{
		{Agent: "worker", Status: a2a.StatusOnline, Capabilities: []string{"textstats"}, Description: "Demo worker",
			Keys: &a2a.Keys{Sign: "x"}, KeysPinned: true},
		{Agent: "coordinator", Status: a2a.StatusOffline, Description: "Demo coordinator"},
	}
}

func TestAgentRowsAndModel(t *testing.T) {
	rows := AgentRows(cards())
	if rows[0][0] != "worker" || rows[0][1] != "online" || rows[0][3] != "textstats" || rows[0][4] != "pinned" {
		t.Errorf("row = %v", rows[0])
	}

	var m tea.Model = NewAgents(cards)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 20})
	v := plain(m.View().Content)
	for _, want := range []string{"1 online", "1 offline", "worker", "coordinator", "Demo worker"} {
		if !strings.Contains(v, want) {
			t.Errorf("agents view missing %q:\n%s", want, v)
		}
	}
	if _, cmd := m.Update(tickMsg(time.Now())); cmd == nil {
		t.Error("tick did not schedule the next refresh")
	}
}

func TestDashboard(t *testing.T) {
	ch := make(chan Item, 1)
	var m tea.Model = NewDashboard("explorer", "all agent traffic (observer)", cards, ch)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", envelope(t))))
	v := plain(m.View().Content)
	for _, want := range []string{"as explorer", "observer", "1 up", "worker", "1 msgs", "req 1", "coordinator → worker"} {
		if !strings.Contains(v, want) {
			t.Errorf("dashboard missing %q:\n%s", want, v)
		}
	}
}

func TestWaitModel(t *testing.T) {
	var m tea.Model = newWait("waiting for worker")
	if v := plain(m.View().Content); !strings.Contains(v, "waiting for worker") {
		t.Errorf("view = %q", v)
	}
	boom := errors.New("boom")
	m, cmd := m.Update(doneMsg{err: boom})
	if !isQuit(cmd) || m.(WaitModel).Err != boom || m.View().Content != "" {
		t.Errorf("done did not quit with the error: %+v", m)
	}
	m, _ = newWait("x").Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.(WaitModel).Err != ErrCanceled {
		t.Error("ctrl+c did not cancel")
	}
}

// Wait runs a real program (no terminal) and returns fn's result.
func TestWaitRuns(t *testing.T) {
	var out strings.Builder
	called := false
	err := Wait(context.Background(), &out, strings.NewReader(""), "working", func(context.Context) error {
		called = true
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	if err != nil || !called {
		t.Fatalf("err %v called %v", err, called)
	}
}

func sampleStatus(at time.Time, in int64) broker.Status {
	return broker.Status{
		Version: "dev", Mochi: "2.7.9", Started: at.Add(-time.Hour), Time: at, Uptime: 3600,
		Listen: "127.0.0.1:18830", DataDir: "/var/lib/agentbus",
		Limits:    broker.StatusLimits{MaxEnvelopeBytes: 262144, RatePerSecond: 20, RateBurst: 50, MaxQueued: 1024, SessionExpiry: "168h0m0s", MaxClockSkew: "2m0s", MaxConnections: 1024, MaxConnectionsPerIP: 64},
		Traffic:   broker.StatusTraffic{ClientsConnected: 3, Sessions: 4, MessagesReceived: in, MessagesSent: in * 2},
		Decisions: broker.Decisions{Accepted: 40, Duplicates: 1, Rejected: map[string]int64{"no_route": 2, "invalid": 1}},
		Agents: []broker.AgentStatus{
			{ID: "worker", State: "online", Remote: "127.0.0.1:5000", Capabilities: []string{"textstats"}, Accepted: 10},
			{ID: "explorer", State: "online", Observer: true},
			{ID: "you", State: "offline", Queued: 2, SendTo: []string{"worker"}},
		},
	}
}

func TestStatusModel(t *testing.T) {
	ch := make(chan broker.Status, 2)
	var m tea.Model = NewStatus(ch)
	if v := plain(m.View().Content); !strings.Contains(v, "observer identity") {
		t.Errorf("empty view should explain the requirement:\n%s", v)
	}
	now := time.Now()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m, _ = m.Update(StatusMsg(sampleStatus(now.Add(-2*time.Second), 100)))
	m, _ = m.Update(StatusMsg(sampleStatus(now, 120)))
	v := plain(m.View().Content)
	for _, want := range []string{"127.0.0.1:18830", "up 1h0m0s", "/var/lib/agentbus", "3 connected · 4 sessions",
		"10.0/s in", "accepted   40", "3  invalid 1 · no_route 2", "worker", "textstats", "observer", "you", "→ worker", "256.0 KiB/envelope"} {
		if !strings.Contains(v, want) {
			t.Errorf("status view missing %q:\n%s", want, v)
		}
	}
	if s := BrokerSummary(sampleStatus(now, 1)); !strings.Contains(s, "3 clients · 40 accepted · 3 rejected") {
		t.Errorf("summary = %q", s)
	}
}

func TestTailTrunc(t *testing.T) {
	if got := tailTrunc("/home/user/agentbus/demo/.run/data", 16); got != "…/demo/.run/data" || len([]rune(got)) != 16 {
		t.Errorf("got %q", got)
	}
	if got := tailTrunc("/short", 16); got != "/short" {
		t.Errorf("got %q", got)
	}
}

func TestBackgroundColorSwitchesPalette(t *testing.T) {
	defer SetDark(true)
	var m tea.Model = NewFeed("watch", make(chan Item))
	m, _ = m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#FFFFFF")})
	if pal != lightPalette {
		t.Error("light background did not select the light palette")
	}
	m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#000000")})
	if pal != darkPalette {
		t.Error("dark background did not select the dark palette")
	}
}

// Layout must fit the window: no line wider than it, and the dashboard
// no taller. (lipgloss v2 counts borders and padding in Width/Height.)
func TestViewsFitWindow(t *testing.T) {
	long := envelope(t)
	long.Body = []byte(`{"text":"` + strings.Repeat("very long body text ", 30) + `"}`)
	for _, size := range [][2]int{{80, 24}, {120, 30}, {200, 50}} {
		w, h := size[0], size[1]
		ch := make(chan Item, 200)
		var d tea.Model = NewDashboard("explorer", "all agent traffic (observer)", cards, ch)
		d, _ = d.Update(tea.WindowSizeMsg{Width: w, Height: h})
		d, _ = d.Update(StatusMsg(sampleStatus(time.Now(), 5)))
		for range 100 {
			d, _ = d.Update(ItemMsg(ItemFromEnvelope("t", long)))
		}
		checkFits(t, "dashboard", d.View().Content, w, h)

		sc := make(chan broker.Status, 1)
		var s tea.Model = NewStatus(sc)
		s, _ = s.Update(tea.WindowSizeMsg{Width: w, Height: h})
		st := sampleStatus(time.Now(), 5)
		st.DataDir = "/very/long/path/" + strings.Repeat("nested/", 20) + "data"
		s, _ = s.Update(StatusMsg(st))
		checkFits(t, "status", s.View().Content, w, 0)

		var a tea.Model = NewAgents(cards)
		a, _ = a.Update(tea.WindowSizeMsg{Width: w, Height: h})
		checkFits(t, "agents", a.View().Content, w, h)
	}
}

func checkFits(t *testing.T, name, view string, w, h int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	for i, l := range lines {
		if lw := ansi.StringWidth(l); lw > w {
			t.Errorf("%s at %dx%d: line %d is %d wide: %q", name, w, h, i, lw, plain(l))
			return
		}
	}
	if h > 0 && len(lines) > h {
		t.Errorf("%s at %dx%d: %d lines tall", name, w, h, len(lines))
	}
}
