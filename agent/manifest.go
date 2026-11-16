// Package agent runs a domain-specific agentbus agent described by a
// manifest: which bus topics it serves, and the shell command, script
// (sh, bash, python3, ...) or Go function that handles each.
//
// The same runtime backs `agentbus agent run` (a manifest on disk) and the
// standalone binaries `agentbus agent build` produces (manifest and scripts
// embedded, Go handlers compiled in). See docs/AGENTS.md.
package agent

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/agent/llm"
)

// Manifest describes an agent. Connection settings are defaults that the
// environment and flags override on each host; the token is never in it.
type Manifest struct {
	Agent         string            `yaml:"agent"`          // agent ID; AGENTBUS_AGENT overrides
	Description   string            `yaml:"description"`    // first part of the presence description
	Broker        string            `yaml:"broker"`         // default broker URL; AGENTBUS_URL overrides
	TokenFile     string            `yaml:"token_file"`     // default token file; AGENTBUS_TOKEN_FILE overrides
	MaxConcurrent int               `yaml:"max_concurrent"` // handlers running at once; default 4
	Timeout       Duration          `yaml:"timeout"`        // default per-handler timeout; default 60s
	Drain         Duration          `yaml:"drain"`          // wait for running handlers on shutdown; default 30s
	AllowFrom     []string          `yaml:"allow_from"`     // senders allowed by default; empty means all
	Workdir       string            `yaml:"workdir"`        // default working directory for commands
	Env           map[string]string `yaml:"env"`            // extra environment for commands
	LLM           *LLM              `yaml:"llm"`            // defaults for llm handlers: provider, model, key, limits
	MaxHops       int               `yaml:"max_hops"`       // bus calls an llm handler may chain; default 3
	Handlers      []Handler         `yaml:"handlers"`
}

// Handler binds one trigger to one action.
//
// Trigger, at most one of:
//
//	task: CAP          consume the capability's task queue
//	inbox: true        answer this agent's inbox; with op, only body.op == op
//	event: FILTER      watch events matching an a2a/v1/events/... filter
//
// A handler with no trigger is a tool: it runs only when an llm handler's
// model calls it.
//
// Action, exactly one of:
//
//	run: COMMAND       a shell command (sh -c)
//	script: PATH       a script shipped with the agent, run by interpreter
//	go: NAME           a Go function registered with agent.Register
//	llm: {...}         a model, with other handlers as its tools
type Handler struct {
	Name        string            `yaml:"name"`
	Task        string            `yaml:"task"`
	Inbox       bool              `yaml:"inbox"`
	Op          string            `yaml:"op"`
	Event       string            `yaml:"event"`
	Run         string            `yaml:"run"`
	Script      string            `yaml:"script"`
	Interpreter string            `yaml:"interpreter"` // default from the extension: .sh sh, .bash bash, .py python3
	Go          string            `yaml:"go"`
	LLM         *LLM              `yaml:"llm"`
	Params      map[string]any    `yaml:"params"` // JSON Schema of the body, shown to models that call this as a tool
	Timeout     Duration          `yaml:"timeout"`
	AllowFrom   []string          `yaml:"allow_from"` // overrides the manifest's
	Description string            `yaml:"description"`
	Workdir     string            `yaml:"workdir"`
	Env         map[string]string `yaml:"env"`
}

// LLM configures a model-backed handler. In the manifest's top-level llm:
// block only the connection and limit fields are allowed; a handler's
// llm: block inherits them and adds the rest.
type LLM struct {
	Provider   string `yaml:"provider"`     // anthropic or openai (any OpenAI-compatible server)
	Model      string `yaml:"model"`        // required, here or in the defaults
	BaseURL    string `yaml:"base_url"`     // default: the provider's public API
	APIKeyEnv  string `yaml:"api_key_env"`  // default ANTHROPIC_API_KEY or OPENAI_API_KEY
	APIKeyFile string `yaml:"api_key_file"` // read instead of the environment when set
	MaxTurns   int    `yaml:"max_turns"`    // model calls per message; default 8
	MaxTokens  int    `yaml:"max_tokens"`   // per model call; default 4096

	System string   `yaml:"system"` // the system prompt
	Tools  []string `yaml:"tools"`  // names of this manifest's handlers the model may call
	Bus    *BusTool `yaml:"bus"`    // bus calls the model may make
	Reply  string   `yaml:"reply"`  // json: the answer must be JSON; text: always a string; default: JSON if it parses
}

// BusTool lists what an llm handler's model may do on the bus. The
// broker's grants for this agent still apply on top.
type BusTool struct {
	Agents  bool     `yaml:"agents"`  // list agents and their presence cards
	Request []string `yaml:"request"` // agent IDs it may ask; "*" for any
	Task    []string `yaml:"task"`    // capabilities it may hand work to; "*" for any
}

// Duration reads "30s", "5m" and so on from YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration like 30s or 5m", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Defaults.
const (
	DefaultMaxConcurrent = 4
	DefaultTimeout       = 60 * time.Second
	DefaultDrain         = 30 * time.Second
	DefaultMaxHops       = 3
	DefaultMaxTurns      = 8
	DefaultMaxTokens     = 4096
)

// ParseManifest reads a manifest. Unknown keys are errors, so a typo
// doesn't silently drop a setting.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		if strings.Contains(err.Error(), "field token not found") {
			return nil, errors.New("manifest: tokens don't belong in the manifest (it is embedded in the binary); " +
				"use token_file or AGENTBUS_TOKEN_FILE on each host")
		}
		return nil, fmt.Errorf("manifest: %w", err)
	}
	m.applyDefaults()
	return &m, m.Validate()
}

// LoadManifest reads and parses name from fsys.
func LoadManifest(fsys fs.FS, name string) (*Manifest, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return ParseManifest(data)
}

func (m *Manifest) applyDefaults() {
	if m.MaxConcurrent == 0 {
		m.MaxConcurrent = DefaultMaxConcurrent
	}
	if m.Timeout == 0 {
		m.Timeout = Duration(DefaultTimeout)
	}
	if m.Drain == 0 {
		m.Drain = Duration(DefaultDrain)
	}
	if m.MaxHops == 0 {
		m.MaxHops = DefaultMaxHops
	}
	for i := range m.Handlers {
		h := &m.Handlers[i]
		if h.Name == "" {
			h.Name = h.defaultName(i)
		}
		if h.LLM != nil {
			h.LLM = h.LLM.withDefaults(m.LLM)
		}
	}
}

// withDefaults returns a copy of l with unset connection and limit fields
// taken from d, then from the built-in defaults.
func (l *LLM) withDefaults(d *LLM) *LLM {
	out := *l
	if d == nil {
		d = &LLM{}
	}
	out.Provider = first(out.Provider, d.Provider)
	out.Model = first(out.Model, d.Model)
	out.BaseURL = first(out.BaseURL, d.BaseURL)
	out.APIKeyFile = first(out.APIKeyFile, d.APIKeyFile)
	out.APIKeyEnv = first(out.APIKeyEnv, d.APIKeyEnv)
	if out.APIKeyEnv == "" && out.APIKeyFile == "" {
		switch out.Provider {
		case llm.Anthropic:
			out.APIKeyEnv = llm.AnthropicKey
		case llm.OpenAI:
			out.APIKeyEnv = llm.OpenAIKey
		}
	}
	out.MaxTurns = cmp.Or(out.MaxTurns, d.MaxTurns, DefaultMaxTurns)
	out.MaxTokens = cmp.Or(out.MaxTokens, d.MaxTokens, DefaultMaxTokens)
	return &out
}

func (h Handler) defaultName(i int) string {
	switch {
	case h.Task != "":
		return h.Task
	case h.Inbox && h.Op != "":
		return "inbox:" + h.Op
	case h.Inbox:
		return "inbox"
	case h.Event != "":
		return "event:" + h.Event
	}
	return fmt.Sprintf("handler %d", i+1)
}

// Validate checks the manifest on its own. Whether Go handlers are
// registered, scripts exist and interpreters are installed is checked by
// Check, against the binary and host that will run it.
func (m *Manifest) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if m.Agent != "" && !a2a.ValidName(m.Agent) {
		bad("agent %q: IDs are 1-64 chars of a-z 0-9 . _ - and start with a letter or digit", m.Agent)
	}
	if m.MaxConcurrent < 1 {
		bad("max_concurrent must be at least 1")
	}
	if m.Timeout < 0 || m.Drain < 0 {
		bad("timeout and drain can't be negative")
	}
	for _, id := range m.AllowFrom {
		if !a2a.ValidName(id) {
			bad("allow_from: %q is not an agent ID", id)
		}
	}
	if len(m.Handlers) == 0 {
		bad("no handlers: add at least one under handlers:")
	}
	if m.MaxHops < 1 {
		bad("max_hops must be at least 1")
	}
	if d := m.LLM; d != nil {
		if d.System != "" || d.Tools != nil || d.Bus != nil || d.Reply != "" {
			bad("llm: the top-level block only sets defaults (provider, model, base_url, api_key_env, " +
				"api_key_file, max_turns, max_tokens); put system, tools, bus and reply on the handler")
		}
		if d.Provider != "" && d.Provider != llm.Anthropic && d.Provider != llm.OpenAI {
			bad("llm: provider %q: want %s or %s", d.Provider, llm.Anthropic, llm.OpenAI)
		}
	}
	byName := map[string][]Handler{}
	for _, h := range m.Handlers {
		byName[h.Name] = append(byName[h.Name], h)
	}
	usedAsTool := map[string]bool{}
	tasks, ops := map[string]bool{}, map[string]bool{}
	defaultInbox := false
	for _, h := range m.Handlers {
		where := "handler " + h.Name
		triggers := 0
		if h.Task != "" {
			triggers++
			if !a2a.ValidName(h.Task) {
				bad("%s: task %q is not a valid capability name", where, h.Task)
			}
			if tasks[h.Task] {
				bad("%s: task %q is handled twice", where, h.Task)
			}
			tasks[h.Task] = true
		}
		if h.Inbox {
			triggers++
			switch {
			case h.Op == "" && defaultInbox:
				bad("%s: only one inbox handler can have no op", where)
			case h.Op == "":
				defaultInbox = true
			case ops[h.Op]:
				bad("%s: op %q is handled twice", where, h.Op)
			default:
				ops[h.Op] = true
			}
		} else if h.Op != "" {
			bad("%s: op only applies to inbox handlers", where)
		}
		if h.Event != "" {
			triggers++
			if t, err := a2a.ParseTopic(h.Event); err != nil || t.Kind != a2a.KindEvent {
				bad("%s: event %q is not an a2a/v1/events/... filter", where, h.Event)
			}
		}
		if triggers > 1 {
			bad("%s: needs at most one of task, inbox or event", where)
		}
		actions := 0
		for _, set := range []bool{h.Run != "", h.Script != "", h.Go != "", h.LLM != nil} {
			if set {
				actions++
			}
		}
		if actions != 1 {
			bad("%s: needs exactly one of run, script, go or llm", where)
		}
		if triggers == 0 && h.LLM != nil {
			bad("%s: an llm handler needs a task, inbox or event trigger; only run, script and go handlers can be tools", where)
		}
		if h.LLM != nil {
			for _, err := range validateLLM(h, byName) {
				bad("%s: llm: %v", where, err)
			}
			for _, t := range h.LLM.Tools {
				usedAsTool[t] = true
			}
		}
		if h.Params != nil {
			if _, err := json.Marshal(h.Params); err != nil {
				bad("%s: params: %v", where, err)
			}
		}
		if h.Script != "" && !scriptPathOK(h.Script) {
			bad("%s: script %q must be a relative path inside the agent directory", where, h.Script)
		}
		if h.Interpreter != "" && h.Script == "" {
			bad("%s: interpreter only applies to script handlers", where)
		}
		if h.Timeout < 0 {
			bad("%s: timeout can't be negative", where)
		}
		for _, id := range h.AllowFrom {
			if !a2a.ValidName(id) {
				bad("%s: allow_from: %q is not an agent ID", where, id)
			}
		}
	}
	for _, h := range m.Handlers {
		if h.trigger() == "" && h.LLM == nil && !usedAsTool[h.Name] {
			bad("handler %s: has no task, inbox or event trigger, and no llm handler lists it in tools, so it never runs", h.Name)
		}
	}
	return errors.Join(errs...)
}

// trigger names what starts h, or "" for a tool-only handler.
func (h Handler) trigger() string {
	switch {
	case h.Task != "":
		return "task"
	case h.Inbox:
		return "inbox"
	case h.Event != "":
		return "event"
	}
	return ""
}

// Names reserved for the bus tools an llm handler can be given.
const (
	toolAgents  = "bus_agents"
	toolRequest = "bus_request"
	toolTask    = "bus_task"
)

var toolNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validateLLM(h Handler, byName map[string][]Handler) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	l := h.LLM
	switch l.Provider {
	case llm.Anthropic, llm.OpenAI:
	case "":
		bad("no provider: set provider (%s or %s) here or in the top-level llm: block", llm.Anthropic, llm.OpenAI)
	default:
		bad("provider %q: want %s or %s", l.Provider, llm.Anthropic, llm.OpenAI)
	}
	if l.Model == "" {
		bad("no model: set model here or in the top-level llm: block")
	}
	if l.MaxTurns < 1 || l.MaxTurns > 50 {
		bad("max_turns must be 1-50")
	}
	if l.MaxTokens < 1 {
		bad("max_tokens must be at least 1")
	}
	switch l.Reply {
	case "", "json", "text":
	default:
		bad("reply %q: want json or text", l.Reply)
	}
	if l.APIKeyEnv != "" && strings.HasPrefix(l.APIKeyEnv, "AGENTBUS_") {
		bad("api_key_env can't be an AGENTBUS_ variable")
	}
	seen := map[string]bool{}
	for _, t := range l.Tools {
		if seen[t] {
			bad("tools: %q is listed twice", t)
		}
		seen[t] = true
		switch hs := byName[t]; {
		case t == toolAgents || t == toolRequest || t == toolTask:
			bad("tools: %q is reserved for the bus tools; use bus: instead", t)
		case !toolNameRE.MatchString(t):
			bad("tools: %q: a tool's handler name must be 1-64 chars of a-z A-Z 0-9 _ - (set name: on it)", t)
		case len(hs) == 0:
			bad("tools: no handler named %q", t)
		case len(hs) > 1:
			bad("tools: %d handlers are named %q", len(hs), t)
		case hs[0].LLM != nil:
			bad("tools: %q is an llm handler; tools must be run, script or go handlers", t)
		}
	}
	if b := l.Bus; b != nil {
		for _, id := range b.Request {
			if id != "*" && !a2a.ValidName(id) {
				bad("bus: request: %q is not an agent ID", id)
			}
		}
		for _, c := range b.Task {
			if c != "*" && !a2a.ValidName(c) {
				bad("bus: task: %q is not a capability name", c)
			}
		}
	}
	return errs
}

func scriptPathOK(p string) bool {
	return p != "" && !path.IsAbs(p) && !strings.HasPrefix(p, "\\") && fs.ValidPath(path.Clean(p)) &&
		!slices.Contains(strings.Split(path.Clean(p), "/"), "..")
}

// timeout is the handler's timeout, or the manifest default.
func (m *Manifest) timeout(h Handler) time.Duration {
	if h.Timeout > 0 {
		return time.Duration(h.Timeout)
	}
	return time.Duration(m.Timeout)
}

// allowFrom is the handler's sender list, or the manifest default.
func (m *Manifest) allowFrom(h Handler) []string {
	if h.AllowFrom != nil {
		return h.AllowFrom
	}
	return m.AllowFrom
}

// interpreterFor returns the argv prefix that runs script.
func interpreterFor(h Handler) []string {
	if h.Interpreter != "" {
		return strings.Fields(h.Interpreter)
	}
	switch path.Ext(h.Script) {
	case ".sh":
		return []string{"sh"}
	case ".bash":
		return []string{"bash"}
	case ".py":
		return []string{"python3"}
	}
	return nil // executed directly: needs a shebang and the executable bit
}

// Capabilities lists the task queues the manifest serves.
func (m *Manifest) Capabilities() []string {
	var out []string
	for _, h := range m.Handlers {
		if h.Task != "" {
			out = append(out, h.Task)
		}
	}
	return out
}

// PresenceDescription composes the description published on the agent's
// presence card: the manifest's, then one entry per handler that has one,
// so callers (and the models driving them) can see each body shape.
func (m *Manifest) PresenceDescription() string {
	parts := []string{}
	if m.Description != "" {
		parts = append(parts, m.Description)
	}
	for _, h := range m.Handlers {
		if h.Description == "" || h.Event != "" || h.trigger() == "" {
			continue
		}
		switch {
		case h.Task != "":
			parts = append(parts, "task "+h.Task+": "+h.Description)
		case h.Op != "":
			parts = append(parts, `inbox op "`+h.Op+`": `+h.Description)
		default:
			parts = append(parts, "inbox: "+h.Description)
		}
	}
	s := strings.Join(parts, " | ")
	if len(s) > a2a.MaxDescription {
		s = strings.ToValidUTF8(s[:a2a.MaxDescription-3], "") + "..."
	}
	return s
}
