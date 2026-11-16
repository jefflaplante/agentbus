package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestGeneratedAgent runs the whole generator: agent init, agent build for
// this machine, then the standalone binary against a live broker, with a
// request to each example handler (Go, Python, shell, inline command).
func TestGeneratedAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("functional test builds binaries and runs processes")
	}
	b := build(t)
	src, _ := filepath.Abs("../..")
	work := t.TempDir()
	proj := filepath.Join(work, "probe")

	if out, errOut, err := run(t, nil, b.agentbus, "agent", "init", "probe", "--dir", proj, "--agentbus-src", src); err != nil {
		t.Fatalf("init: %v\n%s%s", err, out, errOut)
	}
	if out, errOut, err := run(t, []string{"GOWORK=off"}, b.agentbus, "agent", "build", proj); err != nil {
		t.Fatalf("build: %v\n%s%s", err, out, errOut)
	}
	bin := filepath.Join(proj, "dist", "probe-"+runtime.GOOS+"-"+runtime.GOARCH)
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}

	agent := newIdentity(t, b, "probe")
	asker := newIdentity(t, b, "asker")
	cfg := map[string]any{
		"listen": "127.0.0.1:0",
		"agents": []map[string]any{
			{"id": agent.id, "token_sha256": agent.hash, "capabilities": []string{"probe.wordcount", "probe.disk"}},
			{"id": asker.id, "token_sha256": asker.hash},
		},
	}
	cfgPath := filepath.Join(work, "broker.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, brokerLog := start(t, "broker", nil, b.agentbus, "serve", "--config", cfgPath)
	url := "tcp://" + brokerLog.waitFor(t, addrRE, "broker to listen")

	// Run from an empty directory: scripts must come from inside the binary.
	host := t.TempDir()
	hostBin := filepath.Join(host, "probe")
	raw, _ := os.ReadFile(bin)
	if err := os.WriteFile(hostBin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(hostBin, "-v")
	cmd.Dir = host
	agentLog := &logBuf{}
	cmd.Env = append(os.Environ(), agent.env(url)...)
	cmd.Stdout, cmd.Stderr = agentLog, agentLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	agentLog.waitFor(t, regexp.MustCompile(`agent serving`), "the built agent to start")

	ask := func(args ...string) (string, error) {
		out, errOut, err := run(t, asker.env(url), b.agentbus, append(args, "--plain")...)
		return out + errOut, err
	}
	if out, err := ask("request", "--to", "probe", "--body", `{"op":"greet","name":"e2e"}`); err != nil || !strings.Contains(out, "hello e2e, from probe") {
		t.Errorf("Go handler: %v %s", err, out)
	}
	if out, err := ask("request", "--to", "probe", "--body", `{"op":"uptime"}`); err != nil || !strings.Contains(out, "load") {
		t.Errorf("inline command: %v %s", err, out)
	}
	if out, err := ask("task", "-c", "probe.disk", "--body", `{"path":"/"}`); err != nil || !strings.Contains(out, `"use_pct"`) {
		t.Errorf("shell script: %v %s", err, out)
	}
	if out, _ := ask("task", "-c", "probe.disk", "--body", `{"path":"/; id"}`); !strings.Contains(out, "invalid") {
		t.Errorf("shell script accepted an unsafe path: %s", out)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		if out, err := ask("task", "-c", "probe.wordcount", "--body", `{"text":"a b c"}`); err != nil || !strings.Contains(out, `"words":3`) {
			t.Errorf("python script: %v %s", err, out)
		}
	}

	_ = cmd.Process.Signal(os.Interrupt)
	agentLog.waitFor(t, regexp.MustCompile(`agent stopped`), "the agent to drain and stop")
}
