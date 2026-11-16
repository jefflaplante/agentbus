package broker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// mochi's teardown of a clean-session connection can run after a
// reconnect under the same ID has registered, and delete the new
// connection's client entry and subscriptions (see guardReconnect). The
// race is too narrow to hit on demand, so this test does what the late
// teardown does, right after a reconnect, and checks the agent is still
// reachable.
func TestGuardReconnectRestoresSession(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &Config{Listen: "127.0.0.1:0"}
	tokens := map[string]string{}
	for _, id := range []string{"asker", "bob"} {
		tok, hash := NewToken()
		tokens[id] = tok
		cfg.Agents = append(cfg.Agents, AgentConfig{ID: id, TokenSHA256: hash})
	}
	b, err := New(cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	connect := func(id string, clean bool, inbox client.Handler) *client.Client {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := client.Connect(ctx, client.Options{BrokerURL: "tcp://" + b.Addr(), AgentID: id, Token: tokens[id],
			CleanSession: clean, Logger: quiet, Inbox: inbox})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	bob := connect("bob", true, nil)
	// A previous session that lingers (here a persistent one), so the next
	// connection counts as a reconnect.
	connect("asker", false, nil).Close()
	got := make(chan struct{}, 1)
	connect("asker", true, func(_ context.Context, e *a2a.Envelope) (any, error) {
		if e.Type == a2a.TypeEvent {
			got <- struct{}{}
		}
		return nil, nil
	})
	// What mochi's late teardown of the old connection does.
	b.server.Topics.Unsubscribe(a2a.InboxTopic("asker"), "asker")
	b.server.Clients.Delete("asker")

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := b.server.Clients.Get("asker"); ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := bob.Send(ctx, "asker", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnected agent unreachable after its session was deleted")
	}
}
