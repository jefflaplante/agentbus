package confstore

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/agentbus/broker"
)

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("AGENTBUS_CONFIG", "")
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(home); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestResolveOrder(t *testing.T) {
	home := isolate(t)
	if loc := Resolve(""); loc.Source != FromDefault || loc.Path != filepath.Join(home, "config", "agentbus", "broker.json") {
		t.Errorf("default: %+v", loc)
	}
	if err := os.WriteFile("broker.json", []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loc := Resolve(""); loc.Source != FromCwd {
		t.Errorf("cwd: %+v", loc)
	}
	t.Setenv("AGENTBUS_CONFIG", "/etc/agentbus.json")
	if loc := Resolve(""); loc.Source != FromEnv || !loc.Explicit() {
		t.Errorf("env: %+v", loc)
	}
	wd, _ := os.Getwd()
	if loc := Resolve("x.json"); loc.Source != FromFlag || loc.Path != filepath.Join(wd, "x.json") {
		t.Errorf("flag: %+v", loc)
	}
	t.Setenv("AGENTBUS_CONFIG", "rel/env.json")
	if loc := Resolve(""); loc.Path != filepath.Join(wd, "rel", "env.json") || !filepath.IsAbs(loc.TokensDir()) {
		t.Errorf("relative env path not made absolute: %+v", loc)
	}
}

// A relative --config gives absolute data_dir, audit_file and token paths.
func TestBootstrapRelativeConfig(t *testing.T) {
	isolate(t)
	home, _ := os.Getwd()
	loc := Resolve("sub/broker.json")
	cfg, err := Bootstrap(loc)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.DataDir) || !filepath.IsAbs(cfg.AuditFile) || cfg.DataDir != filepath.Join(home, "sub", "state", "data") {
		t.Errorf("data_dir %q audit_file %q", cfg.DataDir, cfg.AuditFile)
	}
	if DefaultStateDir(loc) != filepath.Join(home, "sub", "state") {
		t.Errorf("state dir %s", DefaultStateDir(loc))
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestBootstrap(t *testing.T) {
	isolate(t)
	loc := Resolve("")
	cfg, err := Bootstrap(loc)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen || len(cfg.Agents) != 1 || !cfg.Agents[0].Observer || cfg.Agents[0].ID != AdminID {
		t.Errorf("cfg = %+v", cfg)
	}
	if m := mode(t, loc.Path); m != 0o600 {
		t.Errorf("config mode %o", m)
	}
	if m := mode(t, loc.TokenFile(AdminID)); m != 0o600 {
		t.Errorf("token mode %o", m)
	}
	tok, ok := ReadToken(loc, AdminID)
	if !ok || broker.HashToken(tok) != cfg.Agents[0].TokenSHA256 {
		t.Error("token file does not match the configured hash")
	}
	// It loads and validates as a broker config.
	if _, err := broker.LoadConfig(loc.Path); err != nil {
		t.Errorf("bootstrapped config does not load: %v", err)
	}
	if _, err := Bootstrap(loc); err == nil {
		t.Error("Bootstrap overwrote an existing config")
	}
}

func commit(t *testing.T, loc Location, fn func(tx *Tx) error) {
	t.Helper()
	tx, err := Begin(loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		tx.Abort()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLifecycle(t *testing.T) {
	isolate(t)
	loc := Resolve("")
	if _, err := Bootstrap(loc); err != nil {
		t.Fatal(err)
	}
	var file string
	commit(t, loc, func(tx *Tx) (err error) {
		file, err = tx.AddAgent(broker.AgentConfig{ID: "worker", Capabilities: []string{"stats"}})
		if _, err := tx.AddAgent(broker.AgentConfig{ID: "worker"}); err == nil {
			t.Error("duplicate agent accepted")
		}
		if _, err := tx.AddAgent(broker.AgentConfig{ID: "Bad Name"}); err == nil {
			t.Error("invalid agent id accepted")
		}
		return err
	})
	if m := mode(t, file); m != 0o600 {
		t.Errorf("token mode %o", m)
	}
	old, _ := ReadToken(loc, "worker")
	commit(t, loc, func(tx *Tx) error { _, err := tx.RotateToken("worker"); return err })
	tok, _ := ReadToken(loc, "worker")
	cfg, _ := Load(loc.Path)
	if tok == old || broker.HashToken(tok) != cfg.Agents[Find(cfg, "worker")].TokenSHA256 {
		t.Error("rotate did not replace the token consistently")
	}
	commit(t, loc, func(tx *Tx) error { return tx.RemoveAgent("worker") })
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("token file left behind")
	}
	if left := tmpFiles(t, loc); len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func tmpFiles(t *testing.T, loc Location) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{filepath.Dir(loc.Path), loc.TokensDir()} {
		m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
		out = append(out, m...)
	}
	return out
}

// A change whose save fails leaves the token files as they were.
func TestFailedSaveKeepsTokenFiles(t *testing.T) {
	isolate(t)
	loc := Resolve("")
	if _, err := Bootstrap(loc); err != nil {
		t.Fatal(err)
	}
	admin, _ := ReadToken(loc, AdminID)

	// Removing the only agent fails validation: admin.token must survive.
	tx, _ := Begin(loc)
	if err := tx.RemoveAgent(AdminID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("config with no agents saved")
	}
	if tok, ok := ReadToken(loc, AdminID); !ok || tok != admin {
		t.Error("failed rm deleted or changed the token file")
	}

	// A rotation whose save fails keeps the old, still-valid token.
	raw, _ := os.ReadFile(loc.Path)
	bad := strings.Replace(string(raw), DefaultListen, "0.0.0.0:1883", 1) // invalid: plaintext beyond loopback
	if err := os.WriteFile(loc.Path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	tx, _ = Begin(loc)
	if _, err := tx.RotateToken(AdminID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("invalid config saved")
	}
	if tok, _ := ReadToken(loc, AdminID); tok != admin {
		t.Error("failed rotate replaced the token file")
	}
	if left := tmpFiles(t, loc); len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// Commit refuses to overwrite changes made since Begin.
func TestCommitDetectsConcurrentChange(t *testing.T) {
	isolate(t)
	loc := Resolve("")
	if _, err := Bootstrap(loc); err != nil {
		t.Fatal(err)
	}
	tx, _ := Begin(loc)
	if _, err := tx.AddAgent(broker.AgentConfig{ID: "late"}); err != nil {
		t.Fatal(err)
	}
	commit(t, loc, func(other *Tx) error { return other.Set("rate_per_second", "7") })
	if err := tx.Commit(); !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	cfg, _ := Load(loc.Path)
	if cfg.RatePerSecond != 7 || Find(cfg, "late") >= 0 {
		t.Errorf("concurrent change lost: %+v", cfg)
	}
	if _, ok := ReadToken(loc, "late"); ok {
		t.Error("token file written for an agent that was never saved")
	}
}

// Save writes 0600 even over a stale temp file, follows a symlinked
// config, and WriteToken tightens an existing token file's mode.
func TestSaveModesAndSymlink(t *testing.T) {
	home := isolate(t)
	real := filepath.Join(home, "real", "broker.json")
	cfg, err := Bootstrap(Location{Path: real, Source: FromFlag})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg.RatePerSecond = 9
	if err := Save(link, cfg); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("Save replaced the symlink with a regular file")
	}
	if got, _ := Load(real); got.RatePerSecond != 9 {
		t.Error("Save did not write through the symlink")
	}
	if m := mode(t, real); m != 0o600 {
		t.Errorf("config mode %o", m)
	}

	tok := filepath.Join(home, "loose.token")
	if err := os.WriteFile(tok, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteToken(tok, "new"); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, tok); m != 0o600 {
		t.Errorf("WriteToken left mode %o", m)
	}
}

func TestLoadMissingHintsInit(t *testing.T) {
	isolate(t)
	_, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err == nil || !strings.Contains(err.Error(), "config init") || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v", err)
	}
}

func TestSaveValidatesAndWritesDefaults(t *testing.T) {
	isolate(t)
	loc := Resolve("")
	cfg, _ := Bootstrap(loc)
	bad := *cfg
	bad.Listen = "0.0.0.0:1883" // plaintext beyond loopback
	if err := Save(loc.Path, &bad); err == nil {
		t.Error("invalid config saved")
	}
	raw, _ := os.ReadFile(loc.Path)
	for _, want := range []string{`"rate_per_second": 20`, `"session_expiry": "168h0m0s"`, `"listen": "127.0.0.1:1883"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved file missing %s", want)
		}
	}
}

func TestSet(t *testing.T) {
	cfg := &broker.Config{Listen: "127.0.0.1:1"}
	for k, v := range map[string]string{"rate_per_second": "50", "session_expiry": "72h", "allow_insecure": "true", "listen": "127.0.0.1:9"} {
		if err := Set(cfg, k, v); err != nil {
			t.Fatalf("%s=%s: %v", k, v, err)
		}
	}
	if cfg.RatePerSecond != 50 || dur(cfg.SessionExpiry) != "72h0m0s" || !cfg.AllowInsecure || cfg.Listen != "127.0.0.1:9" {
		t.Errorf("cfg = %+v", cfg)
	}
	for k, v := range map[string]string{"nope": "1", "agents": "[]", "max_queued": "lots", "session_expiry": "soon"} {
		if err := Set(cfg, k, v); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	for k, want := range map[string]string{"data_dir": "a string", "max_queued": "a whole number", "session_expiry": "a duration", "allow_insecure": "true or false"} {
		v := "123"
		if k != "data_dir" {
			v = "nope"
		}
		if err := Set(cfg, k, v); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "json") {
			t.Errorf("%s=%s: %v, want %q", k, v, err, want)
		}
	}
}

func dur(d broker.Duration) string { b, _ := d.MarshalJSON(); return strings.Trim(string(b), `"`) }

func TestURLAndSnippets(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:1883": "tcp://127.0.0.1:1883", ":1883": "tcp://localhost:1883", "0.0.0.0:9": "tcp://localhost:9",
	} {
		if got := URL(&broker.Config{Listen: listen}); got != want {
			t.Errorf("%s: %s", listen, got)
		}
	}
	if got := URL(&broker.Config{Listen: "bus.example:8883", TLSCert: "c"}); got != "ssl://bus.example:8883" {
		t.Errorf("tls: %s", got)
	}
	env := ClientEnv("tcp://h:1", "w", "/a b/w.token")
	if !strings.Contains(env, "AGENTBUS_TOKEN_FILE='/a b/w.token'") {
		t.Errorf("env quoting: %s", env)
	}
	if s := MQTTSettings("ssl://h:8883", "w", "/t"); !strings.Contains(s, "mqtts") || !strings.Contains(s, "client ID  w") {
		t.Errorf("mqtt settings: %s", s)
	}
}

func TestClientEnvUsesHomeAndWorksInShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, ".config", "agentbus", "tokens", "w.token")
	env := ClientEnv("tcp://h:1", "w", file)
	if !strings.Contains(env, `AGENTBUS_TOKEN_FILE="$HOME/.config/agentbus/tokens/w.token"`) {
		t.Fatalf("env = %s", env)
	}
	out, err := exec.Command("sh", "-c", env+`printf %s "$AGENTBUS_TOKEN_FILE"`).Output()
	if err != nil || string(out) != file {
		t.Errorf("sourced value = %q (%v), want %q", out, err, file)
	}
	if Tilde(file) != "~/.config/agentbus/tokens/w.token" {
		t.Errorf("tilde = %s", Tilde(file))
	}
	if s := GoSnippet("tcp://h:1", "w", file); !strings.Contains(s, `filepath.Join(os.Getenv("HOME"), ".config/agentbus/tokens/w.token")`) {
		t.Errorf("go snippet = %s", s)
	}
}
