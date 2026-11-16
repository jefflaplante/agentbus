package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPythonExamples runs examples/python against a live broker: the
// Python worker serves a task queue and its inbox, and both the Python
// client and the Go CLI use it. The worker's send_to excludes its callers,
// so its replies also check that replies don't need send_to.
//
// It needs a Python with paho-mqtt 2.x: AGENTBUS_PYTHON names one, else
// python3. Without paho-mqtt the test is skipped.
func TestPythonExamples(t *testing.T) {
	if testing.Short() {
		t.Skip("functional test builds binaries and runs processes")
	}
	py := os.Getenv("AGENTBUS_PYTHON")
	if py == "" {
		py = "python3"
	}
	if out, err := exec.Command(py, "-c", "import paho.mqtt.enums").CombinedOutput(); err != nil {
		t.Skipf("%s has no paho-mqtt 2.x (pip install -r examples/python/requirements.txt): %s", py, strings.TrimSpace(string(out)))
	}
	dir, _ := filepath.Abs("../../examples/python")

	b := build(t)
	worker := newIdentity(t, b, "counter")
	asker := newIdentity(t, b, "asker")
	gocli := newIdentity(t, b, "gocli")
	cfg := map[string]any{
		"listen": "127.0.0.1:0",
		"agents": []map[string]any{
			{"id": worker.id, "token_sha256": worker.hash, "capabilities": []string{"wordcount"}, "send_to": []string{"nobody"}},
			{"id": asker.id, "token_sha256": asker.hash},
			{"id": gocli.id, "token_sha256": gocli.hash},
			{"id": "nobody", "token_sha256": worker.hash},
		},
	}
	cfgPath := filepath.Join(t.TempDir(), "broker.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, brokerLog := start(t, "broker", nil, b.agentbus, "serve", "--config", cfgPath)
	url := "tcp://" + brokerLog.waitFor(t, addrRE, "broker to listen")

	_, workerLog := start(t, "python worker", worker.env(url), py, filepath.Join(dir, "worker.py"))
	workerLog.waitFor(t, regexp.MustCompile(`serving as counter`), "the Python worker to start")

	client := func(args ...string) (string, string, error) {
		return run(t, asker.env(url), py, append([]string{filepath.Join(dir, "client.py")}, args...)...)
	}
	if out, errOut, err := client("task", "wordcount", `{"text": "one two three"}`); err != nil || strings.TrimSpace(out) != `{"words": 3}` {
		t.Errorf("task: %v %q %s", err, out, errOut)
	}
	if _, errOut, err := client("task", "wordcount", `"not an object"`); err == nil || !strings.Contains(errOut, "invalid: body must be") {
		t.Errorf("bad task body: %v %s", err, errOut)
	}
	if out, errOut, err := client("request", "counter", `{"op": "ping"}`); err != nil || !strings.Contains(out, "pong") {
		t.Errorf("ping: %v %q %s", err, out, errOut)
	}
	if _, errOut, err := client("request", "ghost", `{}`, "--timeout", "3"); err == nil || !strings.Contains(errOut, "no_route") {
		t.Errorf("request to unknown agent: %v %s", err, errOut)
	}
	if out, _, err := client("agents"); err != nil || !strings.Contains(out, `wordcount: body {"text": string}`) {
		t.Errorf("agents: %v %q", err, out)
	}
	if _, errOut, err := client("send", "counter", `{"x": 1}`, "--event", "hello", "--priority", "low"); err != nil {
		t.Errorf("send: %v %s", err, errOut)
	}
	workerLog.waitFor(t, regexp.MustCompile(`note from asker: event=hello priority=low`), "the worker to log the note")
	if _, errOut, err := client("emit", "jobs/finished", `{"job": 42}`); err != nil {
		t.Errorf("emit: %v %s", err, errOut)
	}
	workerLog.waitFor(t, regexp.MustCompile(`job event from asker: jobs/finished`), "the worker to see the event")

	// The Go CLI against the Python worker.
	out, errOut, err := run(t, gocli.env(url), b.agentbus, "task", "--plain", "-c", "wordcount", "--body", `{"text": "from go"}`)
	if err != nil || !strings.Contains(out, `"body":{"words":2}`) {
		t.Errorf("go cli task: %v %q %s", err, out, errOut)
	}
	if !strings.Contains(brokerLog.String(), "reply allowed outside send_to") {
		t.Error("worker replies did not use the open-request exception")
	}
}
