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
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/jefflaplante/agentbus/a2a"
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
	Handlers      []Handler         `yaml:"handlers"`
}

// Handler binds one trigger to one action.
//
// Trigger, exactly one of:
//
//	task: CAP          consume the capability's task queue
//	inbox: true        answer this agent's inbox; with op, only body.op == op
//	event: FILTER      watch events matching an a2a/v1/events/... filter
//
// Action, exactly one of:
//
//	run: COMMAND       a shell command (sh -c)
//	script: PATH       a script shipped with the agent, run by interpreter
//	go: NAME           a Go function registered with agent.Register
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
	Timeout     Duration          `yaml:"timeout"`
	AllowFrom   []string          `yaml:"allow_from"` // overrides the manifest's
	Description string            `yaml:"description"`
	Workdir     string            `yaml:"workdir"`
	Env         map[string]string `yaml:"env"`
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
	for i := range m.Handlers {
		h := &m.Handlers[i]
		if h.Name == "" {
			h.Name = h.defaultName(i)
		}
	}
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
		if triggers != 1 {
			bad("%s: needs exactly one of task, inbox or event", where)
		}
		actions := 0
		for _, set := range []bool{h.Run != "", h.Script != "", h.Go != ""} {
			if set {
				actions++
			}
		}
		if actions != 1 {
			bad("%s: needs exactly one of run, script or go", where)
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
	return errors.Join(errs...)
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
		if h.Description == "" || h.Event != "" {
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
