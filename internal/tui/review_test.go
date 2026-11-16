package tui

import (
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
	c := newTestConsole(t, &fakeBus{cards: []a2a.Presence{{Agent: "worker", Status: a2a.StatusOnline, Description: evil},
		{Agent: "a\x1bb", Capabilities: []string{evil}}}})
	var m tea.Model = sizeTo(c, 160, 40)
	m, _ = m.Update(StatusMsg(st))
	for _, tab := range []string{"1", "2", "3"} {
		m, _ = press(m, tab)
		if v := m.View().Content; hasRawControl(v, true) {
			t.Errorf("tab %s not escaped:\n%q", tab, plain(v))
		}
	}
	m, _ = press(m, "2", "enter")
	if v := m.View().Content; hasRawControl(v, true) {
		t.Errorf("agent detail not escaped:\n%q", plain(v))
	}
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
