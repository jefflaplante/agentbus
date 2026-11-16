package confstore

import (
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
	if loc := Resolve("x.json"); loc.Source != FromFlag || loc.Path != "x.json" {
		t.Errorf("flag: %+v", loc)
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

func TestAgentLifecycle(t *testing.T) {
	isolate(t)
	loc := Resolve("")
	cfg, _ := Bootstrap(loc)

	file, err := AddAgent(cfg, loc, broker.AgentConfig{ID: "worker", Capabilities: []string{"stats"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddAgent(cfg, loc, broker.AgentConfig{ID: "worker"}); err == nil {
		t.Error("duplicate agent accepted")
	}
	if _, err := AddAgent(cfg, loc, broker.AgentConfig{ID: "Bad Name"}); err == nil {
		t.Error("invalid agent id accepted")
	}
	if err := Save(loc.Path, cfg); err != nil {
		t.Fatal(err)
	}
	old, _ := ReadToken(loc, "worker")
	if _, err := RotateToken(cfg, loc, "worker"); err != nil {
		t.Fatal(err)
	}
	tok, _ := ReadToken(loc, "worker")
	if tok == old || broker.HashToken(tok) != cfg.Agents[Find(cfg, "worker")].TokenSHA256 {
		t.Error("rotate did not replace the token consistently")
	}
	if err := RemoveAgent(cfg, loc, "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("token file left behind")
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
