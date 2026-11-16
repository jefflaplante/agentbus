package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain text · ✓":      "plain text · ✓",
		"a\nb\rc\td":          `a\x0ab\x0dc\x09d`,
		"\x1b]0;pwned\x07":    `\x1b]0;pwned\x07`,
		"del\x7f c1\u009b31m": `del\x7f c1\u009b31m`,
		"bad\xffutf8":         `bad\xffutf8`,
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanBlock("a\n\tb\x1b[2J\r"); got != "a\n\tb\\x1b[2J\\x0d" {
		t.Errorf("CleanBlock = %q", got)
	}
}

// hasRawControl reports control characters other than the SGR color
// sequences lipgloss itself emits, by stripping those first.
func hasRawControl(s string, allowNewline bool) bool {
	for _, r := range plain(s) {
		if (r < 0x20 && !(allowNewline && (r == '\n' || r == '\t'))) || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

func TestUntrustedTextIsEscaped(t *testing.T) {
	evil := "x\nAGENT forged\x1b]0;title\x07\x1b[2J\u009b"
	rows := AgentRows([]a2a.Presence{{Agent: "w", Status: a2a.StatusOnline, Description: evil,
		Capabilities: []string{"a\nb"}, Meta: map[string]string{"k\x1b": evil}}})
	for _, cell := range rows[0] {
		if hasRawControl(cell, false) {
			t.Errorf("agent row cell %q has control characters", cell)
		}
	}
	if !strings.Contains(rows[0][5], `\x1b]0;title`) {
		t.Errorf("description not shown escaped: %q", rows[0][5])
	}

	e := envelope(t)
	e.From, e.Event, e.Headers = "a\x1bb", evil, map[string]string{"h\n": evil}
	e.Body = []byte("{\"s\":\"del\u007f c1\u0085 esc\\u001b\"}")
	line := FormatItem(ItemFromEnvelope("t", e), 0)
	if hasRawControl(line, false) || !strings.Contains(plain(line), `\x1b[2J`) {
		t.Errorf("feed line not escaped: %q", plain(line))
	}

	in := NewInspector(ItemFromEnvelope("a2a/v1/x\x1b", e), 120, 40)
	if v := in.View(); hasRawControl(v, true) || strings.Contains(plain(v), "forged\n") {
		t.Errorf("inspector not escaped:\n%q", plain(v))
	}
	raw := Item{Topic: "t", Raw: []byte("line1\nline2\x1b[2J")}
	if v := NewInspector(raw, 120, 40).View(); hasRawControl(v, true) || !strings.Contains(plain(v), "line2\\x1b[2J") {
		t.Errorf("raw payload not escaped:\n%q", plain(v))
	}

	p := a2a.Presence{Agent: "w", Status: a2a.StatusOnline, Description: evil, Meta: map[string]string{"k": evil}}
	if v := NewInspector(Item{Topic: "t", Presence: &p}, 120, 40).View(); hasRawControl(v, true) {
		t.Errorf("presence inspector not escaped:\n%q", plain(v))
	}

	st := sampleStatus(time.Now(), 1)
	st.Agents[0].Remote, st.Decisions.Rejected = "1.2.3.4\x1b[2J", map[string]int64{"code\x1b": 1}
	sm := NewStatus(make(chan broker.Status))
	sm.Agents = func() []a2a.Presence {
		return []a2a.Presence{{Agent: "worker", Status: a2a.StatusOnline, Description: evil}}
	}
	var m tea.Model = sizeTo(sm, 160, 40)
	m, _ = m.Update(StatusMsg(st))
	if v := m.View().Content; hasRawControl(v, true) {
		t.Errorf("status view not escaped:\n%q", plain(v))
	}
	m, _ = press(m, "enter")
	if v := m.View().Content; hasRawControl(v, true) {
		t.Errorf("status detail not escaped:\n%q", plain(v))
	}

	d := NewDashboard("me", "all", func() []a2a.Presence { return []a2a.Presence{{Agent: "a\x1bb", Capabilities: []string{evil}}} }, make(chan Item))
	if v := d.View().Content; hasRawControl(v, true) {
		t.Errorf("dashboard not escaped:\n%q", plain(v))
	}
}

// Pause in the status detail is shown there and doesn't outlive it; c
// doesn't clear the traffic every other agent's detail shares.
func TestStatusDetailPauseAndClear(t *testing.T) {
	sm := NewStatus(make(chan broker.Status))
	sm.Agents = cards
	var m tea.Model = sizeTo(sm, 140, 40)
	m, _ = m.Update(StatusMsg(sampleStatus(time.Now(), 5)))
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "worker", "ping"))))
	m, _ = press(m, "enter", "p")
	m, _ = m.Update(ItemMsg(ItemFromEnvelope("t", env(t, "coordinator", "worker", "again"))))
	if v := plain(m.View().Content); !strings.Contains(v, "paused (1 not shown)") {
		t.Errorf("pause not shown in the detail:\n%s", v)
	}
	m, _ = press(m, "c")
	if v := plain(m.View().Content); !strings.Contains(v, "coordinator → worker") {
		t.Error("c cleared the shared traffic")
	}
	m, _ = press(m, "esc")
	if sm := m.(StatusModel); sm.detail != "" || sm.feed.paused {
		t.Errorf("leaving the detail left it paused (detail %q)", sm.detail)
	}
}

// Selections follow the agent, not the row number, as rows change.
func TestSelectionFollowsAgent(t *testing.T) {
	sm := NewStatus(make(chan broker.Status))
	sm.Agents = cards
	var m tea.Model = sizeTo(sm, 140, 40)
	st := sampleStatus(time.Now(), 5) // worker, explorer, you
	m, _ = m.Update(StatusMsg(st))
	m, _ = press(m, "down") // explorer
	st.Agents = append([]broker.AgentStatus{{ID: "aaa", State: "online"}}, st.Agents...)
	m, _ = m.Update(StatusMsg(st))
	m, _ = press(m, "enter")
	if v := plain(m.View().Content); !strings.Contains(v, "agent explorer") {
		t.Errorf("status: cursor moved off explorer:\n%s", v)
	}

	list := cards()
	src := func() []a2a.Presence { return list }
	var am tea.Model = sizeTo(NewAgents(src), 140, 20)
	am, _ = press(am, "down") // coordinator
	list = append([]a2a.Presence{{Agent: "aaa"}}, list...)
	am, _ = am.Update(tickMsg(time.Now()))
	am, _ = press(am, "enter")
	if v := plain(am.View().Content); !strings.Contains(v, "presence card of coordinator") {
		t.Errorf("agents: cursor moved off coordinator:\n%s", v)
	}

	list = cards()
	var dm tea.Model = sizeTo(NewDashboard("me", "all", src, make(chan Item)), 140, 30)
	dm, _ = press(dm, "tab", "down") // coordinator
	list = append([]a2a.Presence{{Agent: "aaa"}}, list...)
	dm, _ = dm.Update(tickMsg(time.Now()))
	dm, _ = press(dm, "i")
	if v := plain(dm.View().Content); !strings.Contains(v, "presence card of coordinator") {
		t.Errorf("dashboard: cursor moved off coordinator:\n%s", v)
	}
}

func TestDashboardAgentsScroll(t *testing.T) {
	var many []a2a.Presence
	for i := range 60 {
		many = append(many, a2a.Presence{Agent: fmt.Sprintf("agent-%02d", i), Status: a2a.StatusOnline})
	}
	var m tea.Model = sizeTo(NewDashboard("me", "all", func() []a2a.Presence { return many }, make(chan Item)), 120, 24)
	m, _ = press(m, "tab")
	for range 40 {
		m, _ = press(m, "down")
	}
	v := plain(m.View().Content)
	if !strings.Contains(v, "▌ ● agent-40") {
		t.Errorf("cursor row not visible:\n%s", v)
	}
	checkFits(t, "dashboard scrolled", m.View().Content, 120, 24)
}

// The editor never saves over changes made to the file after it loaded.
func TestConfigEditorSeesConcurrentChanges(t *testing.T) {
	m, loc := editorAt(t)
	m.agentForm(nil)
	m.fID = "worker"
	if err := m.onForm(); err != nil {
		t.Fatal(err)
	}

	// The CLI removes worker while the editor is open.
	tx, err := confstore.Begin(loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.RemoveAgent("worker"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	setCursor := func(key string) {
		for i, k := range m.keys {
			if k == key {
				m.settings.SetCursor(i)
			}
		}
	}
	m.tab = 1
	setCursor("rate_per_second")
	m.editSetting()
	m.fValue = "33"
	if err := m.onForm(); err == nil || !strings.Contains(err.Error(), "changed outside this editor") {
		t.Fatalf("stale save: %v", err)
	}
	cfg, _ := confstore.Load(loc.Path)
	if confstore.Find(cfg, "worker") >= 0 || cfg.RatePerSecond == 33 {
		t.Fatalf("stale editor overwrote the file: %+v", cfg.Agents)
	}
	if confstore.Find(m.cfg, "worker") >= 0 {
		t.Error("editor did not reload")
	}
	m.editSetting()
	m.fValue = "33"
	if err := m.onForm(); err != nil {
		t.Fatalf("retry after reload: %v", err)
	}
	if cfg, _ := confstore.Load(loc.Path); confstore.Find(cfg, "worker") >= 0 || cfg.RatePerSecond != 33 {
		t.Errorf("after retry: rate %v agents %+v", cfg.RatePerSecond, cfg.Agents)
	}

	// The CLI rotates admin; the editor's stale hash must not come back.
	tx, _ = confstore.Begin(loc)
	if _, err := tx.RotateToken(confstore.AdminID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	m.tab = 0
	sel, _ := m.selectedAgent()
	m.agentForm(&sel)
	m.fObserver = false
	if err := m.onForm(); err == nil {
		t.Fatal("edit over a rotation saved")
	}
	tok, _ := confstore.ReadToken(loc, confstore.AdminID)
	cfg, _ = confstore.Load(loc.Path)
	if broker.HashToken(tok) != cfg.Agents[confstore.Find(cfg, confstore.AdminID)].TokenSHA256 {
		t.Error("rotation reverted")
	}
}

func TestConfigEditorTLSForm(t *testing.T) {
	m, loc := editorAt(t)
	m.tab = 1
	for i, k := range m.keys {
		if k == "tls_key" {
			m.settings.SetCursor(i)
		}
	}
	m.editSetting()
	if m.mode != modeForm || !strings.Contains(plain(m.View().Content), "tls_cert") {
		t.Fatalf("tls form not opened:\n%s", plain(m.View().Content))
	}
	m.fCert, m.fKey = "/etc/agentbus/cert.pem", "/etc/agentbus/key.pem"
	if err := m.onForm(); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := confstore.Load(loc.Path); cfg.TLSCert != m.fCert || cfg.TLSKey != m.fKey {
		t.Errorf("tls = %q %q", cfg.TLSCert, cfg.TLSKey)
	}
}

func TestRateAndBurstValidation(t *testing.T) {
	for _, s := range []string{"NaN", "-1", "Inf", "x"} {
		if validRate(s) == nil {
			t.Errorf("rate %q accepted", s)
		}
	}
	for _, s := range []string{"", "0", "2.5"} {
		if err := validRate(s); err != nil {
			t.Errorf("rate %q: %v", s, err)
		}
	}
	for _, s := range []string{"1.5", "-2", "NaN"} {
		if validBurst(s) == nil {
			t.Errorf("burst %q accepted", s)
		}
	}
	if err := validBurst("10"); err != nil {
		t.Error(err)
	}
	m, _ := editorAt(t)
	m.agentForm(nil)
	m.fID, m.fRate, m.fBurst = "w", "NaN", "1.5"
	if err := m.onForm(); err == nil {
		t.Error("bad rate and burst saved")
	}
}

func TestRequestSummary(t *testing.T) {
	got := plain(requestSummary([]broker.RequestStats{
		{Target: "task:work", Open: 2, Answered: 5, Errored: 1, Expired: 3},
		{Target: "agent:bob", Answered: 1},
	}))
	if got != "2 open · 6 ok · 1 err · 3 expired" {
		t.Errorf("summary = %q", got)
	}
}
