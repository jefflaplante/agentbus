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
//
// $AGENTBUS_CONFIG_DIR replaces ~/.config/agentbus, for a server
// (/etc/agentbus) or a second broker on one machine. Its state then goes
// in a "state" directory beside the config, unless $AGENTBUS_STATE_DIR
// names another (/var/lib/agentbus).
package confstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
	FromDir     Source = "AGENTBUS_CONFIG_DIR"
	FromDefault Source = "default location"
)

// Location is a resolved config path.
type Location struct {
	Path   string
	Source Source
}

// Explicit reports whether the user named this file (flag or env). A
// directory from $AGENTBUS_CONFIG_DIR isn't explicit: serve and config
// create the config there when it's missing, as in the default location.
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
// broker.json in $AGENTBUS_CONFIG_DIR, then the default location. It never looks in the current directory, so every
// command finds the same file wherever it runs. The path is made
// absolute, so the data_dir and token paths derived from it are too.
func Resolve(flag string) Location {
	switch {
	case flag != "":
		return Location{absPath(flag), FromFlag}
	case os.Getenv("AGENTBUS_CONFIG") != "":
		return Location{absPath(os.Getenv("AGENTBUS_CONFIG")), FromEnv}
	}
	if d := os.Getenv("AGENTBUS_CONFIG_DIR"); d != "" {
		return Location{filepath.Join(absPath(d), "broker.json"), FromDir}
	}
	return Location{filepath.Join(DefaultConfigDir(), "broker.json"), FromDefault}
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// ConfigDir holds broker.json and the tokens directory:
// $AGENTBUS_CONFIG_DIR, else DefaultConfigDir.
func ConfigDir() string {
	if d := os.Getenv("AGENTBUS_CONFIG_DIR"); d != "" {
		return absPath(d)
	}
	return DefaultConfigDir()
}

// DefaultConfigDir is $XDG_CONFIG_HOME/agentbus, or ~/.config/agentbus.
func DefaultConfigDir() string { return xdg("XDG_CONFIG_HOME", ".config") }

// StateDir is $AGENTBUS_STATE_DIR, else $XDG_STATE_HOME/agentbus, else
// ~/.local/state/agentbus.
func StateDir() string {
	if d := os.Getenv("AGENTBUS_STATE_DIR"); d != "" {
		return absPath(d)
	}
	return xdg("XDG_STATE_HOME", filepath.Join(".local", "state"))
}

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
	cfg, _, err := load(path)
	return cfg, err
}

func load(path string) (*broker.Config, string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("no config at %s; \"agentbus config init\" (or \"agentbus serve\") creates one: %w", path, err)
	}
	if err != nil {
		return nil, "", err
	}
	var c broker.Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, stampOf(data), nil
}

// Stamp identifies the config file's current contents ("" if it can't be
// read). An editor compares stamps to notice changes made by others.
func Stamp(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return stampOf(data)
}

func stampOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Save validates cfg and writes it atomically with mode 0600. Defaults
// are written out, so the file shows every effective setting. If path is
// a symlink, the file it points to is replaced.
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
	return writeAtomic(path, append(data, '\n'))
}

// writeAtomic replaces path with data, mode 0600: a private temp file in
// the same directory, synced, then renamed over the target (the target
// of a symlink, if path is one).
func writeAtomic(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	tmp, err := stageFile(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// stageFile writes data to a new 0600 temp file beside path and returns
// its name; the caller renames or removes it.
func stageFile(path string, data []byte) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// DefaultStateDir is where Bootstrap puts data_dir and the audit log for
// a config at loc: $AGENTBUS_STATE_DIR when set; else StateDir for the
// default location, and a "state" directory beside any other config, so
// two brokers never share a data_dir.
func DefaultStateDir(loc Location) string {
	if os.Getenv("AGENTBUS_STATE_DIR") == "" && loc.Source != FromDefault {
		return filepath.Join(filepath.Dir(absPath(loc.Path)), "state")
	}
	return StateDir()
}

// Bootstrap writes a working config at loc: a loopback listener, state
// under DefaultStateDir, and one observer identity, "admin", whose token
// goes in the tokens directory. It refuses to overwrite an existing config.
func Bootstrap(loc Location) (*broker.Config, error) {
	loc.Path = absPath(loc.Path)
	if loc.Exists() {
		return nil, fmt.Errorf("%s already exists", loc.Path)
	}
	state := DefaultStateDir(loc)
	tx := &Tx{Config: &broker.Config{
		Listen:    DefaultListen,
		DataDir:   filepath.Join(state, "data"),
		AuditFile: filepath.Join(state, "audit.jsonl"),
	}, loc: loc, fresh: true}
	if _, err := tx.AddAgent(broker.AgentConfig{ID: AdminID, Observer: true}); err != nil {
		tx.Abort()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tx.Config, nil
}

// ErrChanged means the config file changed on disk after it was read, so
// saving would overwrite changes the editor never saw.
var ErrChanged = errors.New("the config file changed on disk since it was read")

// Tx is one change to the config file. Begin reads the file; the methods
// change Config and stage token files; Commit saves the config and only
// then puts the token files in place (or deletes them), so a config that
// fails to save never leaves the token files out of step with it. Abort
// discards a Tx that won't be committed.
type Tx struct {
	Config *broker.Config
	loc    Location
	stamp  string
	fresh  bool              // creating the file: it must not exist
	staged map[string]string // agent id -> temp file holding its new token
	remove []string          // token files to delete after the save
	done   bool
}

// Begin reads the config at loc for a change.
func Begin(loc Location) (*Tx, error) {
	cfg, stamp, err := load(loc.Path)
	if err != nil {
		return nil, err
	}
	return &Tx{Config: cfg, loc: loc, stamp: stamp}, nil
}

// Stamp is the stamp of the file contents the Tx started from.
func (tx *Tx) Stamp() string { return tx.stamp }

// AddAgent adds an agent with a fresh token and returns the path its
// token file will have once the Tx commits.
func (tx *Tx) AddAgent(a broker.AgentConfig) (string, error) {
	if !a2a.ValidName(a.ID) {
		return "", fmt.Errorf("invalid agent id %q: use 1-64 of a-z 0-9 . _ -", a.ID)
	}
	if Find(tx.Config, a.ID) >= 0 {
		return "", fmt.Errorf("agent %q already exists", a.ID)
	}
	hash, err := tx.stageToken(a.ID)
	if err != nil {
		return "", err
	}
	a.TokenSHA256 = hash
	tx.Config.Agents = append(tx.Config.Agents, a)
	return tx.loc.TokenFile(a.ID), nil
}

// RotateToken gives an agent a new token, written to its token file when
// the Tx commits. A running broker disconnects the agent once it reloads
// the saved config.
func (tx *Tx) RotateToken(id string) (string, error) {
	i := Find(tx.Config, id)
	if i < 0 {
		return "", fmt.Errorf("no agent %q", id)
	}
	hash, err := tx.stageToken(id)
	if err != nil {
		return "", err
	}
	tx.Config.Agents[i].TokenSHA256 = hash
	return tx.loc.TokenFile(id), nil
}

// RemoveAgent deletes an agent; its token file is deleted when the Tx
// commits.
func (tx *Tx) RemoveAgent(id string) error {
	i := Find(tx.Config, id)
	if i < 0 {
		return fmt.Errorf("no agent %q", id)
	}
	tx.Config.Agents = slices.Delete(tx.Config.Agents, i, i+1)
	tx.discard(id)
	tx.remove = append(tx.remove, tx.loc.TokenFile(id))
	return nil
}

// Set changes one top-level setting; see Set.
func (tx *Tx) Set(key, value string) error { return Set(tx.Config, key, value) }

func (tx *Tx) stageToken(id string) (string, error) {
	tok, hash := broker.NewToken()
	tmp, err := stageFile(tx.loc.TokenFile(id), []byte(tok+"\n"))
	if err != nil {
		return "", err
	}
	tx.discard(id)
	if tx.staged == nil {
		tx.staged = map[string]string{}
	}
	tx.staged[id] = tmp
	return hash, nil
}

func (tx *Tx) discard(id string) {
	if tmp, ok := tx.staged[id]; ok {
		_ = os.Remove(tmp)
		delete(tx.staged, id)
	}
}

// Commit saves the config, refusing with ErrChanged if the file changed
// since Begin, then moves staged token files into place and deletes the
// token files of removed agents. If the save fails, nothing on disk
// changes.
func (tx *Tx) Commit() error {
	if tx.done {
		return errors.New("config change already committed or aborted")
	}
	tx.done = true
	if tx.fresh && tx.loc.Exists() {
		tx.abortFiles()
		return fmt.Errorf("%s already exists", tx.loc.Path)
	}
	if !tx.fresh && Stamp(tx.loc.Path) != tx.stamp {
		tx.abortFiles()
		return ErrChanged
	}
	if err := Save(tx.loc.Path, tx.Config); err != nil {
		tx.abortFiles()
		return err
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(tx.staged)) {
		if err := os.Rename(tx.staged[id], tx.loc.TokenFile(id)); err != nil {
			_ = os.Remove(tx.staged[id])
			errs = append(errs, fmt.Errorf("config saved but writing the token file for %s failed (rotate it again): %w", id, err))
		}
	}
	for _, f := range tx.remove {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("config saved but removing %s failed: %w", f, err))
		}
	}
	tx.staged, tx.remove = nil, nil
	tx.stamp = Stamp(tx.loc.Path)
	return errors.Join(errs...)
}

// Abort discards the staged token files. It's safe after Commit.
func (tx *Tx) Abort() {
	if !tx.done {
		tx.done = true
		tx.abortFiles()
	}
}

func (tx *Tx) abortFiles() {
	for id := range tx.staged {
		tx.discard(id)
	}
	tx.remove = nil
}

// Find returns the index of agent id, or -1.
func Find(cfg *broker.Config, id string) int {
	return slices.IndexFunc(cfg.Agents, func(a broker.AgentConfig) bool { return a.ID == id })
}

// WriteToken writes a token file, mode 0600 even if the file already
// existed with another mode, creating its directory.
func WriteToken(path, token string) error {
	return writeAtomic(path, []byte(token+"\n"))
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
	var field reflect.Type
	t := reflect.TypeFor[broker.Config]()
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name == key {
			field = t.Field(i).Type
		}
	}
	if field == nil {
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
		return fmt.Errorf("%s: %q is not %s", key, value, kindOf(field))
	}
	*cfg = next
	return nil
}

// kindOf describes what a setting of type t accepts, for error messages.
func kindOf(t reflect.Type) string {
	if t == reflect.TypeFor[broker.Duration]() {
		return "a duration (like 90s, 10m or 72h)"
	}
	switch t.Kind() {
	case reflect.String:
		return `a string (quote it as JSON, '"123"', if it looks like a number)`
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int64, reflect.Int32:
		return "a whole number"
	case reflect.Float64, reflect.Float32:
		return "a number"
	}
	return "a valid " + t.String()
}
