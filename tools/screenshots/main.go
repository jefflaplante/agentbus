// Command screenshots regenerates the terminal screenshots in docs/images.
//
// It runs a real broker in-process with the demo's agents, drives traffic
// through it (including a rejected message and a high-priority event),
// observes it as the "explorer" observer, feeds what it saw into the
// actual Bubble Tea views at fixed terminal sizes, and converts their ANSI
// output to SVG terminal windows. Run it with `make screenshots`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/demo/agents"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/tui"
)

func main() {
	out := flag.String("out", "docs/images", "output directory")
	flag.Parse()
	if err := run(*out); err != nil {
		log.Fatal(err)
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type identity struct{ id, token string }

func run(outDir string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp, err := os.MkdirTemp("", "agentbus-shots")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// Write output relative to where we started, then work inside tmp so
	// the status view shows short relative paths instead of a temp dir.
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	if err := os.Chdir(tmp); err != nil {
		return err
	}
	// Paths under HOME display as ~/… in the config editor, as for a real user.
	if err := os.Setenv("HOME", tmp); err != nil {
		return err
	}

	// The demo's identities.
	ids := map[string]*identity{}
	cfg := &broker.Config{
		Listen: "127.0.0.1:0", DataDir: "data", AuditFile: "audit.jsonl",
		StatusInterval: broker.Duration(250 * time.Millisecond),
	}
	for _, a := range []broker.AgentConfig{
		{ID: "coordinator"},
		{ID: "worker", Capabilities: []string{"textstats"}},
		{ID: "worker-2", Capabilities: []string{"textstats"}},
		{ID: "you", SendTo: []string{"worker", "worker-2"}},
		{ID: "explorer", Observer: true},
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		ids[a.ID] = &identity{a.ID, tok}
		cfg.Agents = append(cfg.Agents, a)
	}
	srv, err := broker.New(cfg, quiet)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	defer srv.Close()
	url := "tcp://" + srv.Addr()
	opts := func(id string) client.Options {
		return client.Options{BrokerURL: url, AgentID: id, Token: ids[id].token, Logger: quiet, CleanSession: true}
	}

	// Two workers, so the task queue spreads work.
	for _, w := range []string{"worker", "worker-2"} {
		go func() {
			_ = agents.RunWorker(ctx, agents.WorkerOptions{Client: opts(w), Delay: 40 * time.Millisecond, Log: quiet})
		}()
	}

	// The observer: everything it sees feeds the screenshots.
	explorer, err := client.Connect(ctx, opts("explorer"))
	if err != nil {
		return err
	}
	defer explorer.Close()
	var mu sync.Mutex
	var items []tui.Item
	var statuses []broker.Status
	if err := explorer.Observe(ctx, a2a.Root+"/#", func(topic string, payload []byte) {
		mu.Lock()
		items = append(items, tui.ItemFromMessage(topic, payload))
		mu.Unlock()
	}); err != nil {
		return err
	}
	if err := explorer.Observe(ctx, broker.StatusTopic, func(_ string, payload []byte) {
		var st broker.Status
		if json.Unmarshal(payload, &st) == nil {
			mu.Lock()
			statuses = append(statuses, st)
			mu.Unlock()
		}
	}); err != nil {
		return err
	}
	waitFor(func() bool { return len(explorer.Agents()) >= 3 })

	// Traffic: the coordinator's scripted conversation, twice.
	for range 2 {
		if _, err := agents.RunCoordinator(ctx, agents.CoordinatorOptions{Client: opts("coordinator")}); err != nil {
			return err
		}
	}
	you, err := client.Connect(ctx, opts("you"))
	if err != nil {
		return err
	}
	defer you.Close()
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	start := time.Now()
	pong, err := you.Request(rctx, "worker", agents.Op{Op: "ping"})
	if err != nil {
		return err
	}
	pongAfter := time.Since(start)
	_ = you.Send(rctx, "worker", agents.Note{Kind: "hello", Text: "ding dong"},
		client.WithPriority(a2a.PriorityHigh), client.WithEvent("doorbell_ring"))
	start = time.Now()
	_, rejected := you.Request(rctx, "coordinator", map[string]string{"q": "hi"}) // not in send_to
	rejectAfter := time.Since(start)
	task, err := you.RequestTask(rctx, "textstats", agents.StatsRequest{Text: "to be or not to be"})
	if err != nil {
		return err
	}
	// A little steady traffic so the status view shows live rates.
	go func() {
		for ctx.Err() == nil {
			pctx, pcancel := context.WithTimeout(ctx, time.Second)
			_, _ = you.Request(pctx, "worker", agents.Op{Op: "ping"})
			pcancel()
			time.Sleep(60 * time.Millisecond)
		}
	}()
	time.Sleep(1200 * time.Millisecond)

	mu.Lock()
	seen := append([]tui.Item(nil), items...)
	snaps := append([]broker.Status(nil), statuses...)
	mu.Unlock()
	if len(snaps) < 2 {
		return fmt.Errorf("only %d status snapshots", len(snaps))
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	write := func(name, title, content string, cols int) error {
		path := filepath.Join(outDir, name)
		fmt.Println("wrote", path)
		return os.WriteFile(path, []byte(toSVG(title, content, cols)), 0o644)
	}

	// agentbus status
	statusCh := make(chan broker.Status)
	var m tea.Model = tui.NewStatus(statusCh)
	m = size(m, 120, 25) // tall enough for the five agents, no empty rows
	for _, st := range snaps[len(snaps)-2:] {
		m, _ = m.Update(tui.StatusMsg(st))
	}
	if err := write("status.svg", "agentbus status", trimView(m.View().Content), 120); err != nil {
		return err
	}

	// agentbus dashboard
	d := tui.NewDashboard("explorer", "all agent traffic (observer)", explorer.Agents, make(chan tui.Item))
	d.Status = statusCh
	m = size(d, 120, 34)
	m, _ = m.Update(tui.StatusMsg(snaps[len(snaps)-1]))
	for _, it := range lastN(withoutRepeats(seen), 60) {
		m, _ = m.Update(tui.ItemMsg(it))
	}
	if err := write("dashboard.svg", "agentbus dashboard", m.View().Content, 120); err != nil {
		return err
	}

	// agentbus agents
	m = size(tui.NewAgents(explorer.Agents), 110, 13)
	if err := write("agents.svg", "agentbus agents", trimView(m.View().Content), 110); err != nil {
		return err
	}

	// agentbus watch --filter 'a2a/v1/#'
	var events []tui.Item
	for _, it := range withoutRepeats(seen) {
		if it.Env != nil && (it.Env.Type == a2a.TypeEvent || it.Env.Priority != "") {
			events = append(events, it)
		}
	}
	m = size(tui.NewFeed("agentbus · watch a2a/v1/#", make(chan tui.Item)), 110, 16)
	for _, it := range lastN(events, 12) {
		m, _ = m.Update(tui.ItemMsg(it))
	}
	if err := write("watch.svg", "agentbus watch --filter 'a2a/v1/#'", trimView(m.View().Content), 110); err != nil {
		return err
	}

	// agentbus status → enter on worker: the agent detail view.
	sm := tui.NewStatus(statusCh)
	sm.Agents = explorer.Agents
	sm.EditAgent = func(string) tea.Model { return nil } // shows the "e edit in config" hint
	m = size(sm, 120, 34)
	for _, st := range snaps[len(snaps)-2:] {
		m, _ = m.Update(tui.StatusMsg(st))
	}
	for _, it := range withoutRepeats(seen) {
		m, _ = m.Update(tui.ItemMsg(it))
	}
	m = keys(m, "down") // coordinator → worker
	m = keys(m, "enter")
	if err := write("status-detail.svg", "agentbus status  →  enter", trimView(m.View().Content), 120); err != nil {
		return err
	}
	// …then up and enter: inspect one of the worker's messages.
	m = keys(m, "up", "up", "enter")
	if err := write("inspect.svg", "agentbus status  →  enter  →  enter", trimView(m.View().Content), 120); err != nil {
		return err
	}

	// agentbus config: the editor, and the client setup for an agent.
	loc := confstore.Location{Path: filepath.Join(tmp, ".config", "agentbus", "broker.json"), Source: confstore.FromDefault}
	for id, idn := range ids {
		if err := confstore.WriteToken(loc.TokenFile(id), idn.token); err != nil {
			return err
		}
	}
	saved := *cfg
	saved.Listen = confstore.DefaultListen // the generator's random port isn't what a reader would see
	saved.DataDir, saved.AuditFile = "/var/lib/agentbus/data", "/var/lib/agentbus/audit.jsonl"
	if err := confstore.Save(loc.Path, &saved); err != nil {
		return err
	}
	ed, err := tui.NewConfigEditor(loc, "worker")
	if err != nil {
		return err
	}
	m = size(ed, 120, 18)
	if err := write("config.svg", "agentbus config", trimView(m.View().Content), 120); err != nil {
		return err
	}
	m = keys(m, "c")
	if err := write("config-client.svg", "agentbus config  →  c", trimView(m.View().Content), 120); err != nil {
		return err
	}
	m = keys(m, "esc", "tab")
	m = size(m, 120, 30)
	if err := write("config-settings.svg", "agentbus config  →  tab", trimView(m.View().Content), 120); err != nil {
		return err
	}

	// A shell session: request, a refused request, a task, plain agents.
	if err := write("cli.svg", "zsh", transcript(pong, pongAfter, rejected, rejectAfter, task, explorer.Agents()), 120); err != nil {
		return err
	}
	return nil
}

// keys sends key presses to a model.
func keys(m tea.Model, names ...string) tea.Model {
	for _, n := range names {
		var k tea.KeyPressMsg
		switch n {
		case "enter":
			k = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			k = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "up":
			k = tea.KeyPressMsg{Code: tea.KeyUp}
		case "down":
			k = tea.KeyPressMsg{Code: tea.KeyDown}
		case "tab":
			k = tea.KeyPressMsg{Code: tea.KeyTab}
		default:
			k = tea.KeyPressMsg{Code: rune(n[0]), Text: n}
		}
		m, _ = m.Update(k)
	}
	return m
}

// size sends a window size to a model.
func size(m tea.Model, w, h int) tea.Model {
	m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// trimView drops trailing blank lines (the alt screen pads to the window).
func trimView(s string) string {
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansiStrip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func ansiStrip(s string) string {
	var b strings.Builder
	for _, r := range parseLine(s) {
		b.WriteString(r.text)
	}
	return b.String()
}

func lastN(items []tui.Item, n int) []tui.Item {
	if len(items) > n {
		return items[len(items)-n:]
	}
	return items
}

// withoutRepeats drops repeated presence cards, which are noise in a still.
func withoutRepeats(items []tui.Item) []tui.Item {
	var out []tui.Item
	last := map[string]a2a.Status{}
	for _, it := range items {
		if it.Presence != nil {
			if s, ok := last[it.Presence.Agent]; ok && s == it.Presence.Status {
				continue
			}
			last[it.Presence.Agent] = it.Presence.Status
		}
		if it.Env != nil && (it.Env.From == "you" && strings.Contains(string(it.Env.Body), `"op":"ping"`) ||
			it.Env.To == "you" && strings.Contains(string(it.Env.Body), `"op":"pong"`)) {
			continue // the background ping loop and its replies
		}
		out = append(out, it)
	}
	return out
}

func waitFor(cond func() bool) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !cond(); {
		time.Sleep(20 * time.Millisecond)
	}
}

// transcript renders a shell session the way the CLI prints it on a
// terminal: prompt, command, status line on stderr, body on stdout.
func transcript(pong *a2a.Envelope, pongAfter time.Duration, rejected error, rejectAfter time.Duration,
	task *a2a.Envelope, cards []a2a.Presence) string {
	prompt := tui.OKStyle.Render("❯") + " "
	cmd := func(s string) string { return prompt + tui.TextStyle.Render(s) }
	pretty := func(b json.RawMessage) string {
		var v any
		_ = json.Unmarshal(b, &v)
		out, _ := json.MarshalIndent(v, "", "  ")
		return string(out)
	}
	var b strings.Builder
	fmt.Fprintln(&b, cmd(`source demo/.run/you.env`))
	fmt.Fprintln(&b, cmd(`agentbus request --to worker --body '{"op":"ping"}'`))
	fmt.Fprintf(&b, "%s %s from %s in %s\n", tui.OKStyle.Render("✓"), pong.Type, tui.AccentStyle.Render(pong.From), pongAfter.Round(time.Millisecond))
	fmt.Fprintln(&b, pretty(pong.Body))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, cmd(`agentbus request --to coordinator --body '{"q":"hi"}'`))
	if re, ok := rejected.(*client.RemoteError); ok {
		fmt.Fprintf(&b, "%s %s from %s after %s\n  %s\n", tui.ErrStyle.Render("✗"), tui.ErrStyle.Render(re.Code),
			tui.AccentStyle.Render(re.From), rejectAfter.Round(time.Millisecond), re.Reason)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, cmd(`agentbus task -c textstats --body '{"text":"to be or not to be"}' --plain | jq -c '.body | {words, top_words}'`))
	var stats agents.StatsResult
	_ = json.Unmarshal(task.Body, &stats)
	short, _ := json.Marshal(map[string]any{"words": stats.Words, "top_words": stats.TopWords})
	fmt.Fprintln(&b, string(short))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, cmd(`agentbus agents --plain`))
	var tw bytes.Buffer
	t := tabwriter.NewWriter(&tw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, "AGENT\tSTATUS\tSINCE\tCAPABILITIES\tKEYS\tDESCRIPTION")
	for _, row := range tui.AgentRows(cards) {
		fmt.Fprintln(t, strings.Join(row, "\t"))
	}
	_ = t.Flush()
	b.WriteString(tw.String())
	return b.String()
}
