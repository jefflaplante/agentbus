// Package e2e is the functional test suite. It builds the real binaries,
// runs the agentbus broker and the demo agents as separate processes, and
// checks what they say to each other over TCP. Skipped with -short.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/demo/agents"
)

// bins holds the paths of the built binaries.
type bins struct{ agentbus, worker, coordinator string }

func build(t *testing.T) bins {
	t.Helper()
	dir := t.TempDir()
	b := bins{
		agentbus:    filepath.Join(dir, "agentbus"),
		worker:      filepath.Join(dir, "worker"),
		coordinator: filepath.Join(dir, "coordinator"),
	}
	for out, pkg := range map[string]string{
		b.agentbus: "./cmd/agentbus", b.worker: "./demo/cmd/worker", b.coordinator: "./demo/cmd/coordinator",
	} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, msg)
		}
	}
	return b
}

// logBuf is a goroutine-safe buffer for process output.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitFor polls the buffer until re matches, returning the first submatch.
func (l *logBuf) waitFor(t *testing.T, re *regexp.Regexp, what string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(l.String()); m != nil {
			if len(m) > 1 {
				return m[1]
			}
			return m[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; output so far:\n%s", what, l.String())
	return ""
}

// start runs a long-lived process and stops it at cleanup, dumping its
// output if the test failed.
func start(t *testing.T, name string, env []string, bin string, args ...string) (*exec.Cmd, *logBuf) {
	t.Helper()
	out := &logBuf{}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		if t.Failed() {
			t.Logf("---- %s output ----\n%s", name, out.String())
		}
	})
	return cmd, out
}

// run runs a short-lived command and returns stdout, stderr and the error.
func run(t *testing.T, env []string, bin string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type identity struct{ id, token, hash string }

func newIdentity(t *testing.T, b bins, id string) identity {
	t.Helper()
	out, _, err := run(t, nil, b.agentbus, "token")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`token:\s+(\S+)\s+token_sha256:\s+(\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("unexpected token output: %q", out)
	}
	return identity{id: id, token: m[1], hash: m[2]}
}

func (i identity) env(url string) []string {
	return []string{"AGENTBUS_URL=" + url, "AGENTBUS_AGENT=" + i.id, "AGENTBUS_TOKEN=" + i.token}
}

var addrRE = regexp.MustCompile(`agentbus broker listening" addr=(\S+)`)

// TestAgentsOverRunningBroker is the main functional test: broker, worker
// and coordinator as three processes, then CLI checks against the live bus.
func TestAgentsOverRunningBroker(t *testing.T) {
	if testing.Short() {
		t.Skip("functional test builds binaries and runs processes")
	}
	b := build(t)
	coord := newIdentity(t, b, "coordinator")
	work := newIdentity(t, b, "worker")
	cli := newIdentity(t, b, "cli")

	cfg := map[string]any{
		"listen": "127.0.0.1:0",
		"agents": []map[string]any{
			{"id": coord.id, "token_sha256": coord.hash},
			{"id": work.id, "token_sha256": work.hash, "capabilities": []string{"textstats"}},
			{"id": cli.id, "token_sha256": cli.hash, "send_to": []string{"worker"}},
		},
	}
	cfgPath := filepath.Join(t.TempDir(), "broker.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, brokerLog := start(t, "broker", nil, b.agentbus, "serve", "--config", cfgPath)
	url := "tcp://" + brokerLog.waitFor(t, addrRE, "broker to listen")

	workerCmd, workerLog := start(t, "worker", work.env(url), b.worker)
	workerLog.waitFor(t, regexp.MustCompile(`worker ready`), "worker to be ready")

	t.Run("coordinator script", func(t *testing.T) {
		stdout, stderr, err := run(t, coord.env(url), b.coordinator, "-timeout", "30s")
		t.Logf("coordinator:\n%s", stderr)
		if err != nil {
			t.Fatalf("coordinator failed: %v\n%s", err, stdout)
		}
		var sum agents.Summary
		if err := json.Unmarshal([]byte(stdout), &sum); err != nil {
			t.Fatalf("summary: %v\n%s", err, stdout)
		}
		var names []string
		for _, s := range sum.Steps {
			names = append(names, s.Name)
			if !s.OK {
				t.Errorf("step %s failed: %s", s.Name, s.Detail)
			}
		}
		if got, want := strings.Join(names, ","), "presence,hello,ping,reverse,tasks,acl"; got != want {
			t.Errorf("steps = %s, want %s", got, want)
		}
		// The worker saw both sides of the conversation.
		for _, want := range []string{"kind=hello", "op=ping", "op=reverse", "msg=task"} {
			if !strings.Contains(workerLog.String(), want) {
				t.Errorf("worker log has no %q", want)
			}
		}
	})

	t.Run("cli task to worker", func(t *testing.T) {
		stdout, stderr, err := run(t, cli.env(url), b.agentbus, "task", "--capability", "textstats",
			"--body", `{"text":"one two two three three three"}`, "--timeout", "10s")
		if err != nil {
			t.Fatalf("%v\n%s", err, stderr)
		}
		var reply struct {
			Type string             `json:"type"`
			Body agents.StatsResult `json:"body"`
		}
		if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
			t.Fatalf("%v: %s", err, stdout)
		}
		if reply.Type != "response" || reply.Body.Words != 6 || reply.Body.TopWords[0].Word != "three" {
			t.Errorf("reply = %+v", reply)
		}
	})

	t.Run("cli request to worker", func(t *testing.T) {
		stdout, stderr, err := run(t, cli.env(url), b.agentbus, "request", "--to", "worker", "--body", `{"op":"ping"}`, "--timeout", "10s")
		if err != nil || !strings.Contains(stdout, `"op":"pong"`) {
			t.Errorf("err %v stdout %s stderr %s", err, stdout, stderr)
		}
	})

	t.Run("broker enforces send_to", func(t *testing.T) {
		_, stderr, err := run(t, cli.env(url), b.agentbus, "request", "--to", "coordinator", "--body", `{}`, "--timeout", "10s")
		if err == nil || !strings.Contains(stderr, `not allowed to send to "coordinator"`) {
			t.Errorf("want broker rejection; err %v stderr %s", err, stderr)
		}
	})

	t.Run("bad token rejected", func(t *testing.T) {
		env := []string{"AGENTBUS_URL=" + url, "AGENTBUS_AGENT=cli", "AGENTBUS_TOKEN=wrong"}
		if _, _, err := run(t, env, b.agentbus, "agents"); err == nil {
			t.Error("connected with a bad token")
		}
	})

	t.Run("presence tracks the worker", func(t *testing.T) {
		agentsTable := func() string {
			out, stderr, err := run(t, cli.env(url), b.agentbus, "agents")
			if err != nil {
				t.Fatalf("%v\n%s", err, stderr)
			}
			return out
		}
		online := regexp.MustCompile(`(?m)^worker\s+online\s.*textstats`)
		if out := agentsTable(); !online.MatchString(out) {
			t.Fatalf("worker not listed online:\n%s", out)
		}
		_ = workerCmd.Process.Signal(syscall.SIGTERM)
		offline := regexp.MustCompile(`(?m)^worker\s+offline\s`)
		deadline := time.Now().Add(10 * time.Second)
		for {
			out := agentsTable()
			if offline.MatchString(out) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("worker never went offline:\n%s", out)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

}
