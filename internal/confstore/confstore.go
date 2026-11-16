// Package confstore finds, creates and edits the broker config file and
// the agent token files beside it. The serve, config and client commands
// share it, so `agentbus serve` with no flags bootstraps a working setup
// and `agentbus status` finds the local admin token by itself.
//
// Layout (XDG):
//
//	~/.config/agentbus/broker.json        config (0600)
//	~/.config/agentbus/tokens/<id>.token  one token per agent (0600)
//	~/.local/state/agentbus/              data_dir and audit log
package confstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	neturl "net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
)

// AdminID is the observer identity created by Bootstrap.
const AdminID = "admin"

// DefaultListen is where a bootstrapped broker listens: loopback only, so
// it's safe without TLS.
const DefaultListen = "127.0.0.1:1883"

// Where a config came from.
type Source string

const (
	FromFlag    Source = "--config"
	FromEnv     Source = "AGENTBUS_CONFIG"
	FromCwd     Source = "./broker.json"
	FromDefault Source = "default location"
)

// Location is a resolved config path.
type Location struct {
	Path   string
	Source Source
}

// Explicit reports whether the user named this path (flag or env).
func (l Location) Explicit() bool { return l.Source == FromFlag || l.Source == FromEnv }

// TokensDir is the token directory beside the config file.
func (l Location) TokensDir() string { return filepath.Join(filepath.Dir(l.Path), "tokens") }

// TokenFile is where an agent's token lives.
func (l Location) TokenFile(id string) string { return filepath.Join(l.TokensDir(), id+".token") }

// Exists reports whether the config file exists.
func (l Location) Exists() bool {
	_, err := os.Stat(l.Path)
	return err == nil
}

// Resolve picks the config file: --config, then $AGENTBUS_CONFIG, then
// ./broker.json if it exists, then the default location.
func Resolve(flag string) Location {
	switch {
	case flag != "":
		return Location{flag, FromFlag}
	case os.Getenv("AGENTBUS_CONFIG") != "":
		return Location{os.Getenv("AGENTBUS_CONFIG"), FromEnv}
	}
	if _, err := os.Stat("broker.json"); err == nil {
		abs, _ := filepath.Abs("broker.json")
		return Location{abs, FromCwd}
	}
	return Location{filepath.Join(ConfigDir(), "broker.json"), FromDefault}
}

// ConfigDir is $XDG_CONFIG_HOME/agentbus, or ~/.config/agentbus.
func ConfigDir() string { return xdg("XDG_CONFIG_HOME", ".config") }

// StateDir is $XDG_STATE_HOME/agentbus, or ~/.local/state/agentbus.
func StateDir() string { return xdg("XDG_STATE_HOME", filepath.Join(".local", "state")) }

func xdg(env, fallback string) string {
	if d := os.Getenv(env); d != "" {
		return filepath.Join(d, "agentbus")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, fallback, "agentbus")
}

// Load reads a config for editing: no defaults applied and no validation,
// so a config that doesn't validate can still be opened and fixed.
func Load(path string) (*broker.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c broker.Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// Save validates cfg and writes it atomically with mode 0600. Defaults
// are written out, so the file shows every effective setting.
func Save(path string, cfg *broker.Config) error {
	c := *cfg
	c.Agents = slices.Clone(cfg.Agents)
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Bootstrap writes a working config at loc: a loopback listener, state
// under StateDir, and one observer identity, "admin", whose token goes in
// the tokens directory. It refuses to overwrite an existing config.
func Bootstrap(loc Location) (*broker.Config, error) {
	if loc.Exists() {
		return nil, fmt.Errorf("%s already exists", loc.Path)
	}
	state := StateDir()
	if loc.Source == FromCwd || loc.Explicit() {
		state = filepath.Join(filepath.Dir(loc.Path), "state")
	}
	cfg := &broker.Config{
		Listen:    DefaultListen,
		DataDir:   filepath.Join(state, "data"),
		AuditFile: filepath.Join(state, "audit.jsonl"),
	}
	if _, err := AddAgent(cfg, loc, broker.AgentConfig{ID: AdminID, Observer: true}); err != nil {
		return nil, err
	}
	if err := Save(loc.Path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// AddAgent adds an agent with a fresh token, written to its token file.
// It returns the token file path.
func AddAgent(cfg *broker.Config, loc Location, a broker.AgentConfig) (string, error) {
	if !a2a.ValidName(a.ID) {
		return "", fmt.Errorf("invalid agent id %q: use 1-64 of a-z 0-9 . _ -", a.ID)
	}
	if Find(cfg, a.ID) >= 0 {
		return "", fmt.Errorf("agent %q already exists", a.ID)
	}
	tok, hash := broker.NewToken()
	a.TokenSHA256 = hash
	if err := WriteToken(loc.TokenFile(a.ID), tok); err != nil {
		return "", err
	}
	cfg.Agents = append(cfg.Agents, a)
	return loc.TokenFile(a.ID), nil
}

// RotateToken gives an agent a new token and rewrites its token file. The
// running broker disconnects the agent once the saved config is reloaded.
func RotateToken(cfg *broker.Config, loc Location, id string) (string, error) {
	i := Find(cfg, id)
	if i < 0 {
		return "", fmt.Errorf("no agent %q", id)
	}
	tok, hash := broker.NewToken()
	if err := WriteToken(loc.TokenFile(id), tok); err != nil {
		return "", err
	}
	cfg.Agents[i].TokenSHA256 = hash
	return loc.TokenFile(id), nil
}

// RemoveAgent deletes an agent and its token file.
func RemoveAgent(cfg *broker.Config, loc Location, id string) error {
	i := Find(cfg, id)
	if i < 0 {
		return fmt.Errorf("no agent %q", id)
	}
	cfg.Agents = slices.Delete(cfg.Agents, i, i+1)
	if err := os.Remove(loc.TokenFile(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Find returns the index of agent id, or -1.
func Find(cfg *broker.Config, id string) int {
	return slices.IndexFunc(cfg.Agents, func(a broker.AgentConfig) bool { return a.ID == id })
}

// WriteToken writes a token file (mode 0600), creating its directory.
func WriteToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

// URL is the client URL for a broker config: tcp:// or ssl:// to its
// listen address, with an unspecified host replaced by localhost.
func URL(cfg *broker.Config) string {
	listen := cfg.Listen
	if listen == "" {
		listen = broker.DefaultListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "tcp://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	scheme := "tcp"
	if cfg.TLSCert != "" {
		scheme = "ssl"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// HomeRel returns path relative to the user's home directory, if it's
// inside it.
func HomeRel(path string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return rel, true
}

// Tilde shows path with the home directory as ~, for display only.
func Tilde(path string) string {
	if rel, ok := HomeRel(path); ok {
		return filepath.Join("~", rel)
	}
	return path
}

// shellPath quotes a path for the shell, as "$HOME/…" when it's under
// the home directory so the snippet works for that user anywhere.
func shellPath(path string) string {
	if rel, ok := HomeRel(path); ok && !strings.ContainsAny(rel, "\"$`\\\n") {
		return `"$HOME/` + rel + `"`
	}
	return shellQuote(path)
}

// ClientEnv is a shell snippet that connects the CLI as agent id.
func ClientEnv(url, id, tokenFile string) string {
	return fmt.Sprintf("export AGENTBUS_URL=%s\nexport AGENTBUS_AGENT=%s\nexport AGENTBUS_TOKEN_FILE=%s\n",
		shellQuote(url), shellQuote(id), shellPath(tokenFile))
}

// GoSnippet is Go code that connects as agent id.
func GoSnippet(url, id, tokenFile string) string {
	file := fmt.Sprintf("%q", tokenFile)
	if rel, ok := HomeRel(tokenFile); ok {
		file = fmt.Sprintf("filepath.Join(os.Getenv(\"HOME\"), %q)", rel)
	}
	return fmt.Sprintf(`tok, _ := os.ReadFile(%s)
c, err := client.Connect(ctx, client.Options{
	BrokerURL: %q,
	AgentID:   %q,
	Token:     strings.TrimSpace(string(tok)),
})`, file, url, id)
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReadToken reads an agent's token file, if it exists.
func ReadToken(loc Location, id string) (string, bool) {
	b, err := os.ReadFile(loc.TokenFile(id))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// MQTTSettings describes how to connect a desktop MQTT client (MQTT
// Explorer, MQTTX) as agent id.
func MQTTSettings(url, id, tokenFile string) string {
	scheme, host, port := "mqtt://", url, ""
	if u, err := neturl.Parse(url); err == nil {
		if u.Scheme == "ssl" {
			scheme = "mqtts:// (TLS on)"
		}
		host, port = u.Hostname(), u.Port()
	}
	return fmt.Sprintf("protocol   %s\nhost       %s\nport       %s\nusername   %s\nclient ID  %s   (must equal the username)\npassword   the contents of %s\n",
		scheme, host, port, id, id, Tilde(tokenFile))
}

// Set changes one top-level setting from a string: JSON if it parses
// (50, true, "10m"), otherwise a bare string. It refuses unknown keys and
// values of the wrong type; Save validates the result.
func Set(cfg *broker.Config, key, value string) error {
	if key == "agents" {
		return errors.New(`use "agentbus config agent" to change agents`)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	known := false
	t := reflect.TypeFor[broker.Config]()
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name == key {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unknown setting %q", key)
	}
	v := json.RawMessage(value)
	if !json.Valid(v) {
		v, _ = json.Marshal(value)
	}
	m[key] = v
	raw, _ = json.Marshal(m)
	var next broker.Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&next); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*cfg = next
	return nil
}
