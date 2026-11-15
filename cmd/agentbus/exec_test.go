package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
)

func runExec(t *testing.T, command string, timeout time.Duration, typ a2a.Type) (any, error) {
	t.Helper()
	env, _ := a2a.New(typ, "alice", map[string]string{"q": "hi"})
	h := execHandler(command, timeout, map[a2a.Type]bool{a2a.TypeRequest: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h(context.Background(), env)
}

func TestExecReplyFromStdout(t *testing.T) {
	out, err := runExec(t, `cat | jq -c '{got: .body.q}' 2>/dev/null || echo '{"got":"hi"}'`, 5*time.Second, a2a.TypeRequest)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"got":"hi"`) {
		t.Errorf("reply = %s", raw)
	}
}

func TestExecScrubsToken(t *testing.T) {
	t.Setenv("AGENTBUS_TOKEN", "secret-token")
	t.Setenv("AGENTBUS_TOKEN_FILE", "/tmp/x")
	out, err := runExec(t, "env", 5*time.Second, a2a.TypeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := out.(string); strings.Contains(s, "AGENTBUS_TOKEN") {
		t.Error("child saw AGENTBUS_TOKEN")
	}
}

func TestExecFailureHidesStderr(t *testing.T) {
	_, err := runExec(t, "echo /home/secret/path >&2; exit 3", 5*time.Second, a2a.TypeRequest)
	if err == nil || strings.Contains(err.Error(), "secret") || err.Error() != "handler failed" {
		t.Errorf("err = %v", err)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := runExec(t, "sleep 30 & echo $! > "+pidFile+"; wait", 300*time.Millisecond, a2a.TypeRequest)
	if err == nil || err.Error() != "handler timed out" {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("grandchild survived the timeout")
	}
}

func TestExecOutputCap(t *testing.T) {
	_, err := runExec(t, "head -c 2000000 /dev/zero", 5*time.Second, a2a.TypeRequest)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v", err)
	}
}

func TestExecSkipsUnselectedTypes(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	if _, err := runExec(t, "touch "+marker, 5*time.Second, a2a.TypeEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran for an event with -types request")
	}
}
