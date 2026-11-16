package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const goodManifest = `
agent: probe
description: test agent
timeout: 5s
allow_from: [alice]
handlers:
  - task: probe.count
    script: scripts/count.py
    description: 'body {"text": string}'
  - inbox: true
    op: ping
    run: echo pong
  - inbox: true
    go: echo
    allow_from: []
  - event: a2a/v1/events/+/deploy/#
    run: "true"
`

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxConcurrent != DefaultMaxConcurrent || time.Duration(m.Drain) != DefaultDrain || time.Duration(m.Timeout) != 5*time.Second {
		t.Errorf("defaults: %+v", m)
	}
	if got := m.Capabilities(); len(got) != 1 || got[0] != "probe.count" {
		t.Errorf("capabilities = %v", got)
	}
	if m.Handlers[1].Name != "inbox:ping" || m.Handlers[3].Name != "event:a2a/v1/events/+/deploy/#" {
		t.Errorf("default names: %q %q", m.Handlers[1].Name, m.Handlers[3].Name)
	}
	// An explicit empty list overrides the manifest default (everyone may use it).
	if a := m.allowFrom(m.Handlers[2]); a == nil || len(a) != 0 {
		t.Errorf("allow_from override = %#v", a)
	}
	if a := m.allowFrom(m.Handlers[0]); len(a) != 1 || a[0] != "alice" {
		t.Errorf("allow_from default = %v", a)
	}
	if d := m.PresenceDescription(); !strings.Contains(d, "test agent | task probe.count: body") {
		t.Errorf("description = %q", d)
	}
}

func TestManifestRejects(t *testing.T) {
	for want, doc := range map[string]string{
		"field colour not found":                        "agent: a\ncolour: red\nhandlers: [{task: x, run: y}]",
		"tokens don't belong":                           "agent: a\ntoken: secret\nhandlers: [{task: x, run: y}]",
		"at most one of task, inbox or event":           "handlers: [{task: x, inbox: true, run: y}]",
		"exactly one of run, script, go or llm":         "handlers: [{task: x, run: y, go: z}]",
		"so it never runs":                              "handlers: [{task: x, run: y}, {name: t, run: z}]",
		"no provider":                                   "handlers: [{inbox: true, llm: {model: m}}]",
		"no model":                                      "handlers: [{inbox: true, llm: {provider: openai}}]",
		"provider \"bard\"":                             "handlers: [{inbox: true, llm: {provider: bard, model: m}}]",
		"only sets defaults":                            "llm: {system: hi}\nhandlers: [{task: x, run: y}]",
		"no handler named \"nope\"":                     "llm: {provider: openai, model: m}\nhandlers: [{inbox: true, llm: {tools: [nope]}}]",
		"set name: on it":                               "llm: {provider: openai, model: m}\nhandlers: [{inbox: true, llm: {tools: [\"inbox:a\"]}}, {inbox: true, op: a, run: y}]",
		"is an llm handler":                             "llm: {provider: openai, model: m}\nhandlers: [{inbox: true, llm: {tools: [b]}}, {name: b, task: b, llm: {}}]",
		"only run, script and go handlers can be tools": "llm: {provider: openai, model: m}\nhandlers: [{name: b, llm: {}}]",
		"reserved for the bus tools":                    "llm: {provider: openai, model: m}\nhandlers: [{inbox: true, llm: {tools: [bus_request]}}, {name: bus_request, run: y}]",
		"reply \"yaml\"":                                "handlers: [{inbox: true, llm: {provider: openai, model: m, reply: yaml}}]",
		"max_turns must be 1-50":                        "handlers: [{inbox: true, llm: {provider: openai, model: m, max_turns: 99}}]",
		"is not an agent ID":                            "handlers: [{inbox: true, llm: {provider: openai, model: m, bus: {request: [\"Bad Id\"]}}}]",
		"max_hops must be at least 1":                   "max_hops: -1\nhandlers: [{task: x, run: y}]",
		"op \"a\" is handled twice":                     "handlers: [{inbox: true, op: a, run: y}, {inbox: true, op: a, run: z}]",
		"only one inbox handler can have no op":         "handlers: [{inbox: true, run: y}, {inbox: true, run: z}]",
		"must be a relative path":                       "handlers: [{task: x, script: ../etc/passwd}]",
		"not an a2a/v1/events":                          "handlers: [{event: a2a/v1/tasks/x, run: y}]",
		"op only applies to inbox":                      "handlers: [{task: x, op: a, run: y}]",
		"no handlers":                                   "agent: a",
		"not a duration":                                "timeout: soon\nhandlers: [{task: x, run: y}]",
		"task \"x\" is handled twice":                   "handlers: [{task: x, run: y}, {task: x, run: z}]",
		"interpreter only applies":                      "handlers: [{task: x, run: y, interpreter: python3}]",
		"is not a valid capability name":                "handlers: [{task: X Y, run: y}]",
	} {
		_, err := ParseManifest([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", doc, err, want)
		}
	}
}

func TestLLMDefaults(t *testing.T) {
	m, err := ParseManifest([]byte(`
llm: {provider: anthropic, model: claude-haiku-5-5, max_tokens: 1000}
handlers:
  - inbox: true
    llm: {model: claude-sonnet-5-5, tools: [disk], system: be brief}
  - task: t
    llm: {provider: openai, base_url: "http://localhost:11434/v1"}
  - name: disk
    run: df -h
`))
	if err != nil {
		t.Fatal(err)
	}
	a, o := m.Handlers[0].LLM, m.Handlers[1].LLM
	if a.Provider != "anthropic" || a.Model != "claude-sonnet-5-5" || a.MaxTokens != 1000 || a.MaxTurns != DefaultMaxTurns ||
		a.APIKeyEnv != "ANTHROPIC_API_KEY" || a.System != "be brief" {
		t.Errorf("handler 1 = %+v", a)
	}
	if o.Provider != "openai" || o.Model != "claude-haiku-5-5" || o.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("handler 2 = %+v", o)
	}
	if m.MaxHops != DefaultMaxHops {
		t.Errorf("max_hops = %d", m.MaxHops)
	}
	if d := m.PresenceDescription(); strings.Contains(d, "disk") {
		t.Errorf("tool-only handlers belong off the presence card: %q", d)
	}
}

func TestInterpreterFor(t *testing.T) {
	for script, want := range map[string]string{"a.sh": "sh", "a.bash": "bash", "a.py": "python3", "a": ""} {
		got := strings.Join(interpreterFor(Handler{Script: script}), " ")
		if got != want {
			t.Errorf("%s: %q", script, got)
		}
	}
	if got := interpreterFor(Handler{Script: "a.py", Interpreter: "/opt/venv/bin/python -u"}); len(got) != 2 {
		t.Errorf("explicit interpreter = %v", got)
	}
}

func TestCheckFindsMissingPieces(t *testing.T) {
	m, err := ParseManifest([]byte("handlers:\n  - {task: a, script: scripts/missing.sh}\n  - {task: b, go: not-registered}\n"))
	if err != nil {
		t.Fatal(err)
	}
	problems := strings.Join(Check(m, fstest.MapFS{}, false), "\n")
	if !strings.Contains(problems, "script scripts/missing.sh not found") || !strings.Contains(problems, `Go handler "not-registered"`) {
		t.Errorf("problems:\n%s", problems)
	}
}

func TestResolveTokenOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENTBUS_URL", "")
	t.Setenv("AGENTBUS_AGENT", "")
	t.Setenv("AGENTBUS_TOKEN", "")
	t.Setenv("AGENTBUS_TOKEN_FILE", "")
	t.Setenv("AGENTBUS_CONFIG_DIR", "")
	m := &Manifest{Agent: "probe", Broker: "ssl://bus:8883"}
	if _, err := Resolve(m, "", "", ""); err == nil || !strings.Contains(err.Error(), "/etc/agentbus/probe.token") {
		t.Errorf("no token: %v", err)
	}
	t.Setenv("AGENTBUS_TOKEN", "from-env")
	s, err := Resolve(m, "", "", "")
	if err != nil || s.Token != "from-env" || s.URL != "ssl://bus:8883" || s.AgentID != "probe" {
		t.Errorf("env token: %+v %v", s, err)
	}
	if _, err := Resolve(m, "", "", dir+"/missing"); err == nil {
		t.Error("an explicit token file that doesn't exist was ignored")
	}
	t.Setenv("AGENTBUS_URL", "tcp://other:1")
	if s, _ := Resolve(m, "", "", ""); s.URL != "tcp://other:1" {
		t.Errorf("AGENTBUS_URL didn't override the manifest: %s", s.URL)
	}
	t.Setenv("AGENTBUS_TOKEN", "")
	cfgDir := filepath.Join(dir, "etc")
	if err := os.MkdirAll(filepath.Join(cfgDir, "tokens"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "tokens", "probe.token"), []byte("from-dir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTBUS_CONFIG_DIR", cfgDir)
	if s, err := Resolve(m, "", "", ""); err != nil || s.Token != "from-dir" {
		t.Errorf("token in $AGENTBUS_CONFIG_DIR/tokens not found: %+v %v", s, err)
	}
}
