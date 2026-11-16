package broker_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jefflaplante/agentbus/broker"
)

func TestAgentForToken(t *testing.T) {
	aliceTok, aliceHash := broker.NewToken()
	opsTok, opsHash := broker.NewToken()
	cfg := &broker.Config{Listen: "127.0.0.1:0", Agents: []broker.AgentConfig{
		{ID: "alice", TokenSHA256: aliceHash}, {ID: "ops", TokenSHA256: opsHash, Observer: true},
	}}
	b, err := broker.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := b.AgentForToken(aliceTok); !ok || a.ID != "alice" || a.Observer {
		t.Errorf("alice: %+v %v", a, ok)
	}
	if a, ok := b.AgentForToken(opsTok); !ok || a.ID != "ops" || !a.Observer {
		t.Errorf("ops: %+v %v", a, ok)
	}
	for _, bad := range []string{"", "nope", aliceHash} {
		if a, ok := b.AgentForToken(bad); ok {
			t.Errorf("%q matched %s", bad, a.ID)
		}
	}
	// A reload takes effect at once.
	next := *cfg
	newTok, newHash := broker.NewToken()
	next.Agents = []broker.AgentConfig{{ID: "alice", TokenSHA256: newHash}}
	if _, err := b.Reload(&next); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.AgentForToken(aliceTok); ok {
		t.Error("the old token still works after rotation")
	}
	if a, ok := b.AgentForToken(newTok); !ok || a.ID != "alice" {
		t.Error("the new token doesn't work")
	}
	if _, ok := b.AgentForToken(opsTok); ok {
		t.Error("a removed agent's token still works")
	}
}

func TestMCPListenValidation(t *testing.T) {
	_, hash := broker.NewToken()
	base := broker.Config{Listen: "127.0.0.1:0", Agents: []broker.AgentConfig{{ID: "a", TokenSHA256: hash}}}
	for _, c := range []struct {
		name string
		mod  func(*broker.Config)
		want string // "" means valid
	}{
		{"loopback", func(c *broker.Config) { c.MCPListen = "127.0.0.1:8765" }, ""},
		{"public, no TLS", func(c *broker.Config) { c.MCPListen = "0.0.0.0:8765" }, "mcp_listen"},
		{"public, insecure allowed", func(c *broker.Config) { c.MCPListen, c.AllowInsecure = "0.0.0.0:8765", true }, ""},
		{"bad url", func(c *broker.Config) { c.MCPListen, c.MCPURL = "127.0.0.1:8765", "bus.example.com/mcp" }, "mcp_url"},
		{"good url", func(c *broker.Config) { c.MCPListen, c.MCPURL = "127.0.0.1:8765", "https://bus.example.com/mcp" }, ""},
	} {
		cfg := base
		c.mod(&cfg)
		_, err := broker.New(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want one about %s", c.name, err, c.want)
		}
	}
}
