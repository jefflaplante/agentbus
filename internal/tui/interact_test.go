package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func press(m tea.Model, keys ...string) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = m.Update(key(k))
	}
	return m, cmd
}

func env(t *testing.T, from, to, op string) *a2a.Envelope {
	t.Helper()
	e, err := a2a.New(a2a.TypeRequest, from, map[string]string{"op": op})
	if err != nil {
		t.Fatal(err)
	}
	e.To = to
	return e
}

func TestFeedSelectAndInspect(t *testing.T) {
	var m tea.Model = NewFeed("watch", make(chan Item))
	m = sizeTo(m, 120, 30)
	for i, op := range []string{"one", "two", "three"} {
		m, _ = m.Update(ItemMsg(ItemFromEnvelope("a2a/v1/agents/bob/inbox", env(t, "alice", "bob", op))))
		_ = i
	}
	m, _ = press(m, "up") // select "two"
	if v := plain(m.View().Content); !strings.Contains(v, "▌") || !strings.Contains(v, "back to live") {
		t.Fatalf("no selection shown:\n%s", v)
	}
	m, _ = press(m, "enter")
	v := plain(m.View().Content)
	for _, want := range []string{"Inspect request", "from", "alice", "to", "bob", `"op": "two"`, "esc back"} {
		if !strings.Contains(v, want) {
			t.Errorf("inspector missing %q:\n%s", want, v)
		}
	}
	m, _ = press(m, "esc") // close inspector
	m, _ = press(m, "esc") // back to following
	if f := m.(FeedModel); !f.following() || f.inspect != nil {
		t.Error("esc did not step back")
	}
	if _, cmd := press(m, "esc"); !isQuit(cmd) {
		t.Error("esc while following should quit")
	}
}

func sizeTo(m tea.Model, w, h int) tea.Model {
	m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func TestInspectorShowsPresenceTrust(t *testing.T) {
	p := a2a.Presence{Agent: "worker", Status: a2a.StatusOnline, TS: time.Now(), Description: "Demo worker",
		Keys: &a2a.Keys{Sign: "c2lnbg=="}, KeysPinned: true, Meta: map[string]string{"version": "1.4"}}
	in := NewInspector(Item{Topic: a2a.PresenceTopic("worker"), Presence: &p}, 100, 30)
	v := plain(in.View())
	for _, want := range []string{"presence card of worker", "pinned in the broker config", "version = 1.4", "Demo worker"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q:\n%s", want, v)
		}
	}
	if done, _ := in.Update(key("esc")); !done {
		t.Error("esc did not close the inspector")
	}
}

func editorAt(t *testing.T) (*ConfigModel, confstore.Location) {
	t.Helper()
	loc := confstore.Location{Path: filepath.Join(t.TempDir(), "broker.json"), Source: confstore.FromFlag}
	if _, err := confstore.Bootstrap(loc); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigEditor(loc, "")
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	return m, loc
}

func TestConfigEditorAgentLifecycle(t *testing.T) {
	m, loc := editorAt(t)

	m.agentForm(nil)
	m.fID, m.fCaps, m.fSendTo, m.fRate, m.fBurst = "worker", "stats, summarize", "admin", "5", "10"
	if err := m.onForm(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := confstore.Load(loc.Path)
	a := cfg.Agents[confstore.Find(cfg, "worker")]
	if len(a.Capabilities) != 2 || a.SendTo[0] != "admin" || a.RatePerSecond != 5 || a.RateBurst != 10 {
		t.Errorf("added agent = %+v", a)
	}
	if _, ok := confstore.ReadToken(loc, "worker"); !ok {
		t.Error("no token file for the new agent")
	}

	sel, _ := m.selectedAgent()
	m.agentForm(&sel)
	m.fObserver, m.fCaps = true, ""
	if err := m.onForm(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = confstore.Load(loc.Path)
	if a := cfg.Agents[confstore.Find(cfg, "worker")]; !a.Observer || len(a.Capabilities) != 0 || a.TokenSHA256 == "" {
		t.Errorf("edited agent = %+v", a)
	}

	before, _ := confstore.ReadToken(loc, "worker")
	m.askRotate("worker")
	if err := m.onYes(); err != nil {
		t.Fatal(err)
	}
	after, _ := confstore.ReadToken(loc, "worker")
	cfg, _ = confstore.Load(loc.Path)
	if after == before || broker.HashToken(after) != cfg.Agents[confstore.Find(cfg, "worker")].TokenSHA256 {
		t.Error("rotate left the token file and config out of step")
	}

	m.askDelete("worker")
	if err := m.onYes(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = confstore.Load(loc.Path)
	if confstore.Find(cfg, "worker") >= 0 {
		t.Error("agent not removed")
	}
	if _, ok := confstore.ReadToken(loc, "worker"); ok {
		t.Error("token file not removed")
	}
}

func TestConfigEditorSettingsAndClient(t *testing.T) {
	m, loc := editorAt(t)
	m.tab = 1
	for i, k := range m.keys {
		if k == "rate_per_second" {
			m.settings.SetCursor(i)
		}
	}
	m.editSetting()
	m.fValue = "33"
	if err := m.onForm(); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := confstore.Load(loc.Path); cfg.RatePerSecond != 33 {
		t.Errorf("rate = %v", cfg.RatePerSecond)
	}

	for i, k := range m.keys {
		if k == "listen" {
			m.settings.SetCursor(i)
		}
	}
	m.editSetting()
	m.fValue = "0.0.0.0:1883" // plaintext beyond loopback: rejected
	if err := m.onForm(); err == nil {
		t.Error("invalid setting saved")
	}
	if cfg, _ := confstore.Load(loc.Path); cfg.Listen != confstore.DefaultListen {
		t.Errorf("config changed despite the error: %s", cfg.Listen)
	}

	m.tab = 0
	m.openClient("admin")
	v := plain(m.View().Content)
	for _, want := range []string{"Set up admin", "token in", "Token file", loc.TokenFile("admin"),
		"Start it on this machine", "--agent admin console", "↑/↓ scroll"} {
		if !strings.Contains(v, want) {
			t.Errorf("start view missing %q:\n%s", want, v)
		}
	}
	for range 40 {
		m.Update(key("down"))
	}
	if v := plain(m.View().Content); !strings.Contains(v, "Start it on another host") || strings.Contains(v, "Start it on this machine") {
		t.Errorf("start view did not scroll to the end:\n%s", v)
	}
	m.Update(key("w"))
	if b, err := os.ReadFile(filepath.Join(loc.TokensDir(), "admin.env")); err != nil || !strings.Contains(string(b), "AGENTBUS_TOKEN_FILE") {
		t.Errorf("env file: %v %q", err, b)
	}
	m.Update(key("tab"))
	if v := plain(m.View().Content); !strings.Contains(v, "AGENTBUS_AGENT=admin") {
		t.Errorf("Shell tab:\n%s", v)
	}
	m.Update(key("3"))
	if v := plain(m.View().Content); !strings.Contains(v, "client.Connect") {
		t.Errorf("Go tab:\n%s", v)
	}
	if _, cmd := m.Update(key("esc")); m.mode != modeList || cmd != nil {
		t.Error("esc did not leave the client view")
	}
	checkFits(t, "config", m.View().Content, 130, 30)
}

func TestConfigEditorEscCancelsForm(t *testing.T) {
	m, loc := editorAt(t)
	before, _ := os.ReadFile(loc.Path)
	for _, open := range []string{"e", "a"} {
		m.Update(key(open))
		if m.mode != modeForm {
			t.Fatalf("%s did not open the form", open)
		}
		m.Update(key("x")) // type into the first field
		_, cmd := m.Update(key("esc"))
		if m.mode != modeList || cmd != nil {
			t.Errorf("esc after %s: mode %v, cmd %v", open, m.mode, cmd != nil)
		}
		if !strings.Contains(plain(m.View().Content), "canceled") {
			t.Errorf("no cancel notice after %s", open)
		}
	}
	if after, _ := os.ReadFile(loc.Path); string(after) != string(before) {
		t.Error("canceled form changed the config")
	}
}

func TestConfigEditorEmbeddedCloses(t *testing.T) {
	m, _ := editorAt(t)
	m.Embedded = true
	_, cmd := m.Update(key("q"))
	if cmd == nil {
		t.Fatal("no command")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Error("embedded editor should send CloseMsg, not quit")
	}
}

func TestNewViewsFitWindow(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {200, 50}} {
		w, h := size[0], size[1]
		cm, _ := editorAt(t)
		cm.Update(tea.WindowSizeMsg{Width: w, Height: h})
		checkFits(t, "config", cm.View().Content, w, h)
	}
}
