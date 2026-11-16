package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type bus struct {
	url    string
	tokens map[string]string
}

func startBus(t *testing.T) *bus {
	t.Helper()
	b := &bus{tokens: map[string]string{}}
	cfg := &broker.Config{Listen: "127.0.0.1:0"}
	for _, a := range []broker.AgentConfig{
		{ID: "probe", Capabilities: []string{"probe.count"}, SendTo: []string{"nobody"}},
		{ID: "alice"}, {ID: "mallory"}, {ID: "nobody"},
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256, b.tokens[a.ID] = hash, tok
		cfg.Agents = append(cfg.Agents, a)
	}
	srv, err := broker.New(cfg, quiet)
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

func (b *bus) connect(t *testing.T, id string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: id, Token: b.tokens[id], CleanSession: true, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// slowGate holds the channels test.slow uses; each test run sets fresh ones.
var slowGate struct {
	sync.Mutex
	started chan struct{}
	release chan struct{}
}

func newSlowGate() (started, release chan struct{}) {
	slowGate.Lock()
	defer slowGate.Unlock()
	slowGate.started, slowGate.release = make(chan struct{}, 1), make(chan struct{})
	return slowGate.started, slowGate.release
}

func init() {
	Register("test.echo", func(_ context.Context, e *a2a.Envelope) (any, error) {
		return map[string]any{"from": e.From, "body": e.Body}, nil
	})
	Register("test.slow", func(context.Context, *a2a.Envelope) (any, error) {
		slowGate.Lock()
		started, release := slowGate.started, slowGate.release
		slowGate.Unlock()
		started <- struct{}{}
		<-release
		return "finished", nil
	})
}

// startAgent runs manifest as "probe" and returns a stop function that
// cancels it and waits for Run to return.
func startAgent(t *testing.T, b *bus, manifest string, files fstest.MapFS) func() error {
	t.Helper()
	m, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Manifest: m, Files: files, URL: b.url, AgentID: "probe", Token: b.tokens["probe"],
			Logger: quiet, Ready: func(*client.Client) { close(ready) }})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("agent exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("agent didn't start")
	}
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return errors.New("Run didn't return")
		}
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func remoteCode(err error) string {
	var re *client.RemoteError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

func TestAgentServesEveryHandlerKind(t *testing.T) {
	b := startBus(t)
	marker := filepath.Join(t.TempDir(), "event")
	files := fstest.MapFS{
		"scripts/count.sh": {Data: []byte(`#!/bin/sh
body=$(cat)
case $body in *'"text"'*) ;; *) printf '{"code":"invalid","reason":"need text"}'; exit 3 ;; esac
printf '{"from":"%s","op":"%s"}' "$AGENTBUS_FROM" "${AGENTBUS_OP:-}"
`)},
		"scripts/lib/helper.txt": {Data: []byte("sibling files are extracted too")},
	}
	startAgent(t, b, `
agent: probe
handlers:
  - task: probe.count
    script: scripts/count.sh
  - inbox: true
    op: echo
    go: test.echo
  - inbox: true
    op: who
    run: 'printf "%s" "$AGENTBUS_SELF/$AGENTBUS_HANDLER"'
  - inbox: true
    op: secret
    run: echo classified
    allow_from: [alice]
  - event: a2a/v1/events/alice/deploy/#
    run: 'touch `+marker+`'
`, files)

	alice, mallory := b.connect(t, "alice"), b.connect(t, "mallory")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := alice.RequestTask(ctx, "probe.count", map[string]string{"text": "x", "op": "count"})
	if err != nil || !strings.Contains(string(r.Body), `"from":"alice"`) || !strings.Contains(string(r.Body), `"op":"count"`) {
		t.Errorf("script task: %v %s", err, bodyOf(r))
	}
	if _, err := alice.RequestTask(ctx, "probe.count", 42); remoteCode(err) != "invalid" {
		t.Errorf("exit 3 code: %v", err)
	}
	if r, err := alice.Request(ctx, "probe", map[string]string{"op": "echo"}); err != nil || !strings.Contains(string(r.Body), `"from":"alice"`) {
		t.Errorf("go handler: %v %s", err, bodyOf(r))
	}
	if r, err := alice.Request(ctx, "probe", map[string]string{"op": "who"}); err != nil || string(r.Body) != `"probe/inbox:who"` {
		t.Errorf("run handler env: %v %s", err, bodyOf(r))
	}
	if _, err := alice.Request(ctx, "probe", map[string]string{"op": "nope"}); remoteCode(err) != "unknown_op" {
		t.Errorf("unknown op: %v", err)
	}
	if _, err := mallory.Request(ctx, "probe", map[string]string{"op": "secret"}); remoteCode(err) != "forbidden" {
		t.Errorf("allow_from: %v", err)
	}
	if _, err := alice.Request(ctx, "probe", map[string]string{"op": "secret"}); err != nil {
		t.Errorf("allowed sender refused: %v", err)
	}
	if err := alice.Emit(ctx, "deploy/web", map[string]string{"v": "1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event handler didn't run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func bodyOf(e *a2a.Envelope) string {
	if e == nil {
		return "<nil>"
	}
	return string(e.Body)
}

// On shutdown the agent refuses new work, lets running handlers finish,
// then disconnects.
func TestAgentDrainsOnShutdown(t *testing.T) {
	slowStarted, slowRelease := newSlowGate()
	b := startBus(t)
	stop := startAgent(t, b, `
agent: probe
drain: 5s
handlers:
  - inbox: true
    op: slow
    go: test.slow
  - inbox: true
    op: quick
    run: echo quick
`, nil)
	alice := b.connect(t, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	slowReply := make(chan error, 1)
	go func() {
		r, err := alice.Request(ctx, "probe", map[string]string{"op": "slow"})
		if err == nil && string(r.Body) != `"finished"` {
			err = errors.New("wrong body " + string(r.Body))
		}
		slowReply <- err
	}()
	<-slowStarted
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	time.Sleep(200 * time.Millisecond) // shutdown has begun; the slow handler is still running
	if _, err := alice.Request(ctx, "probe", map[string]string{"op": "quick"}); remoteCode(err) != "busy" {
		t.Errorf("new work during drain: %v", err)
	}
	close(slowRelease)
	if err := <-slowReply; err != nil {
		t.Errorf("running request lost in shutdown: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestRunRefusesMissingGoHandlerUnlessSkipping(t *testing.T) {
	b := startBus(t)
	m, err := ParseManifest([]byte("agent: probe\nhandlers:\n  - {inbox: true, op: a, go: not-built}\n  - {inbox: true, op: b, run: echo b}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Manifest: m, URL: b.url, AgentID: "probe", Token: b.tokens["probe"], Logger: quiet}
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "not-built") {
		t.Errorf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg.SkipMissingGo = true
	cfg.Ready = func(*client.Client) { cancel() }
	if err := Run(ctx, cfg); err != nil {
		t.Errorf("with SkipMissingGo: %v", err)
	}
}

func TestExtractKeepsLayoutPrivate(t *testing.T) {
	dir, err := extract(fstest.MapFS{"scripts/a.sh": {Data: []byte("x")}, "scripts/lib/b.py": {Data: []byte("y")}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("temp dir mode %v %v", fi.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scripts", "lib", "b.py")); err != nil {
		t.Error("nested file not extracted")
	}
}
