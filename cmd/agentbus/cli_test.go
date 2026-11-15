package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

type cliBus struct {
	url    string
	tokens map[string]string
	files  map[string]string // token files
}

func startCLIBus(t *testing.T) *cliBus {
	t.Helper()
	for _, k := range []string{"AGENTBUS_URL", "AGENTBUS_AGENT", "AGENTBUS_TOKEN", "AGENTBUS_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	b := &cliBus{tokens: map[string]string{}, files: map[string]string{}}
	cfg := &broker.Config{Listen: "127.0.0.1:0"}
	dir := t.TempDir()
	for _, a := range []broker.AgentConfig{
		{ID: "cli"}, {ID: "bob", Capabilities: []string{"stats"}}, {ID: "ops", Observer: true},
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		b.tokens[a.ID] = tok
		b.files[a.ID] = filepath.Join(dir, a.ID+".token")
		if err := os.WriteFile(b.files[a.ID], []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Agents = append(cfg.Agents, a)
	}
	srv, err := broker.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	b.url = "tcp://" + srv.Addr()
	return b
}

// bob connects as a Go agent that echoes requests and serves "stats".
func (b *cliBus) bob(t *testing.T, inbox client.Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: b.url, AgentID: "bob", Token: b.tokens["bob"], CleanSession: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Inbox: inbox,
		Tasks: map[string]client.Handler{"stats": func(_ context.Context, e *a2a.Envelope) (any, error) {
			return map[string]int{"bytes": len(e.Body)}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
}

// cli runs the agentbus CLI in-process as agent "cli", not on a terminal.
func (b *cliBus) cli(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	return runCLI(t, stdin, append([]string{"--url", b.url, "--agent", "cli", "--token-file", b.files["cli"]}, args...)...)
}

func runCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	// Client goroutines can still log briefly after a command returns.
	out, errb := &syncWriter{w: &bytes.Buffer{}}, &syncWriter{w: &bytes.Buffer{}}
	a := &app{stdin: strings.NewReader(stdin), stdout: out, stderr: errb}
	root := newRootCmd(a)
	root.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := root.ExecuteContext(ctx)
	return out.String(), errb.String(), err
}

func TestTokenJSON(t *testing.T) {
	out, _, err := runCLI(t, "", "token", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var tok map[string]string
	if err := json.Unmarshal([]byte(out), &tok); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok["token"]))
	if hex.EncodeToString(sum[:]) != tok["token_sha256"] {
		t.Error("token_sha256 does not match token")
	}
}

func TestRequestPlainPrintsReplyEnvelope(t *testing.T) {
	b := startCLIBus(t)
	b.bob(t, func(_ context.Context, e *a2a.Envelope) (any, error) {
		return map[string]any{"echo": json.RawMessage(e.Body), "event": e.Event, "session": e.Headers["session"]}, nil
	})
	out, stderr, err := b.cli(t, "", "request", "--to", "bob", "--body", `{"op":"ping"}`,
		"--event", "greeting", "-H", "session=tg:1", "--timeout", "5s")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var reply a2a.Envelope
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("stdout is not one envelope: %q", out)
	}
	var body map[string]any
	_ = reply.DecodeBody(&body)
	if reply.Type != a2a.TypeResponse || reply.From != "bob" || body["event"] != "greeting" || body["session"] != "tg:1" {
		t.Errorf("reply = %+v body %v", reply, body)
	}
}

func TestRequestErrorReplyFails(t *testing.T) {
	b := startCLIBus(t)
	out, _, err := b.cli(t, "", "request", "--to", "nobody", "--body", `{}`, "--timeout", "5s")
	if err == nil || !strings.Contains(err.Error(), "no_route") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, `"type":"error"`) {
		t.Errorf("error reply not printed: %q", out)
	}
}

func TestTaskBodyFromStdinAndFile(t *testing.T) {
	b := startCLIBus(t)
	b.bob(t, nil)
	out, stderr, err := b.cli(t, `{"text":"from stdin"}`, "task", "-c", "stats", "--body", "-", "--timeout", "5s")
	if err != nil || !strings.Contains(out, `"bytes":21`) {
		t.Fatalf("stdin body: %v %q %s", err, out, stderr)
	}
	f := filepath.Join(t.TempDir(), "body.json")
	_ = os.WriteFile(f, []byte(`{"a":1}`), 0o600)
	out, _, err = b.cli(t, "", "task", "-c", "stats", "--body", "@"+f, "--timeout", "5s")
	if err != nil || !strings.Contains(out, `"bytes":7`) {
		t.Fatalf("file body: %v %q", err, out)
	}
}

func TestBadInput(t *testing.T) {
	b := startCLIBus(t)
	for name, args := range map[string][]string{
		"bad json":   {"send", "--to", "bob", "--body", "{nope"},
		"bad header": {"send", "--to", "bob", "--body", "{}", "-H", "novalue"},
		"no to":      {"send", "--body", "{}"},
	} {
		if _, _, err := b.cli(t, "", args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := runCLI(t, "", "--url", b.url, "agents"); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing credentials: %v", err)
	}
}

func TestAgentsPlainTable(t *testing.T) {
	b := startCLIBus(t)
	b.bob(t, nil)
	out, _, err := b.cli(t, "", "agents")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "AGENT") || !strings.Contains(lines[0], "KEYS") {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(out, "bob") || !strings.Contains(out, "stats") {
		t.Errorf("table:\n%s", out)
	}
}

func TestWatchNonEventFilterNeedsObserver(t *testing.T) {
	b := startCLIBus(t)
	_, _, err := b.cli(t, "", "watch", "--filter", "a2a/v1/#")
	if err == nil || !strings.Contains(err.Error(), "observer") {
		t.Fatalf("err = %v", err)
	}
}

func TestWatchPlainAsObserver(t *testing.T) {
	b := startCLIBus(t)
	b.bob(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	sw := &syncWriter{w: &out}
	a := &app{stdin: strings.NewReader(""), stdout: sw, stderr: io.Discard}
	root := newRootCmd(a)
	root.SetArgs([]string{"--url", b.url, "--agent", "ops", "--token-file", b.files["ops"], "watch", "--filter", "a2a/v1/#"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	time.Sleep(300 * time.Millisecond)
	if _, _, err := b.cli(t, "", "task", "-c", "stats", "--body", `{"x":1}`, "--timeout", "5s"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sw.mu.Lock()
	s := out.String()
	sw.mu.Unlock()
	if !strings.Contains(s, `"topic":"a2a/v1/tasks/stats"`) || !strings.Contains(s, `"topic":"a2a/v1/agents/cli/inbox"`) {
		t.Errorf("observer feed:\n%s", s)
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.(*bytes.Buffer).String()
}

func TestDashboardNeedsTerminal(t *testing.T) {
	b := startCLIBus(t)
	if _, _, err := b.cli(t, "", "dashboard"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusPlain(t *testing.T) {
	b := startCLIBus(t)
	b.bob(t, nil)
	if _, _, err := b.cli(t, "", "status", "--plain"); err == nil || !strings.Contains(err.Error(), "observer") {
		t.Errorf("non-observer: %v", err)
	}
	out, stderr, err := runCLI(t, "", "--url", b.url, "--agent", "ops", "--token-file", b.files["ops"], "top", "--plain")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var st broker.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("not a status document: %q", out)
	}
	if len(st.Agents) != 3 || st.Listen == "" || st.Traffic.ClientsConnected < 2 {
		t.Errorf("status = %+v", st)
	}
}
