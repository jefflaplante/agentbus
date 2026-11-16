package broker_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

// hbus is a broker built from a config the test controls.
type hbus struct {
	srv    *broker.Broker
	url    string
	cfg    broker.Config
	tokens map[string]string
	path   string
}

func startH(t *testing.T, mutate func(*broker.Config), agents ...broker.AgentConfig) *hbus {
	t.Helper()
	b := &hbus{tokens: map[string]string{}, path: filepath.Join(t.TempDir(), "broker.json")}
	b.cfg = broker.Config{Listen: "127.0.0.1:0"}
	for _, a := range agents {
		tok, hash := broker.NewToken()
		a.TokenSHA256, b.tokens[a.ID] = hash, tok
		b.cfg.Agents = append(b.cfg.Agents, a)
	}
	if mutate != nil {
		mutate(&b.cfg)
	}
	b.write(t)
	cfg, err := broker.LoadConfig(b.path)
	if err != nil {
		t.Fatal(err)
	}
	if b.srv, err = broker.New(cfg, quietLog()); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.srv.Close() })
	b.url = "tcp://" + b.srv.Addr()
	return b
}

func (b *hbus) write(t *testing.T) {
	t.Helper()
	data, _ := json.Marshal(b.cfg)
	if err := os.WriteFile(b.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (b *hbus) connect(t *testing.T, id string, clean bool, inbox client.Handler) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: id, Token: b.tokens[id],
		CleanSession: clean, Logger: quietLog(), Inbox: inbox})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func collectErrors(ch chan<- string) client.Handler {
	return func(_ context.Context, e *a2a.Envelope) (any, error) {
		if e.Type == a2a.TypeError {
			var b client.ErrorBody
			_ = e.DecodeBody(&b)
			ch <- b.Code
		}
		return nil, nil
	}
}

func nextCode(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatalf("no refusal for %s", what)
		return ""
	}
}

// A full offline queue is refused with queue_full instead of mochi
// dropping the message without telling anyone.
func TestQueueFullIsRefused(t *testing.T) {
	b := startH(t, func(c *broker.Config) { c.MaxQueued = 3 },
		broker.AgentConfig{ID: "alice"}, broker.AgentConfig{ID: "bob"})
	bob := b.connect(t, "bob", false, nil)
	bob.Close() // offline, session kept
	errs := make(chan string, 4)
	alice := b.connect(t, "alice", true, collectErrors(errs))
	for i := range 4 {
		if err := alice.Send(ctxT(t), "bob", i); err != nil {
			t.Fatal(err)
		}
	}
	if code := nextCode(t, errs, "the fourth queued message"); code != "queue_full" {
		t.Errorf("code = %s", code)
	}
}

// Event topics are capped, so one publish can't put kilobytes into every
// log line and audit record.
func TestLongTopicIsRefused(t *testing.T) {
	b := startH(t, nil, broker.AgentConfig{ID: "alice"})
	if _, err := a2a.ParseTopic(a2a.EventTopic("alice", strings.Repeat("x", 600))); err == nil {
		t.Fatal("ParseTopic accepted a 600-byte topic")
	}
	codes := make(chan string, 2)
	raw := paho.NewClient(paho.NewClientOptions().AddBroker(b.url).SetClientID("alice").SetUsername("alice").
		SetPassword(b.tokens["alice"]).SetCleanSession(true).SetAutoReconnect(false))
	if tok := raw.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer raw.Disconnect(0)
	raw.Subscribe(a2a.InboxTopic("alice"), 1, func(_ paho.Client, m paho.Message) {
		var e struct {
			Body client.ErrorBody `json:"body"`
		}
		_ = json.Unmarshal(m.Payload(), &e)
		codes <- e.Body.Code
	}).Wait()
	env, _ := a2a.New(a2a.TypeEvent, "alice", 1)
	data, _ := env.Marshal()
	raw.Publish(a2a.EventTopic("alice", strings.Repeat("x", 600)), 1, false, data).Wait()
	if code := nextCode(t, codes, "a 600-byte topic"); code != "not_authorized" && code != "invalid" {
		t.Errorf("code = %s", code)
	}
}

// Removing an agent clears its presence card; revoking a capability
// re-stamps the card without it.
func TestReloadUpdatesPresence(t *testing.T) {
	b := startH(t, nil,
		broker.AgentConfig{ID: "ops", Observer: true},
		broker.AgentConfig{ID: "bob", Capabilities: []string{"work"}},
		broker.AgentConfig{ID: "carol", Capabilities: []string{"work"}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, id := range []string{"bob", "carol"} {
		c, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: id, Token: b.tokens[id], CleanSession: true,
			Logger: quietLog(), Tasks: map[string]client.Handler{"work": func(context.Context, *a2a.Envelope) (any, error) { return 1, nil }}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
	}
	var mu sync.Mutex
	cards := map[string][]byte{}
	ops := b.connect(t, "ops", true, nil)
	if err := ops.Observe(ctx, a2a.PresenceFilter, func(topic string, payload []byte) {
		mu.Lock()
		cards[topic] = payload
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	card := func(id string) []byte {
		mu.Lock()
		defer mu.Unlock()
		return cards[a2a.PresenceTopic(id)]
	}
	waitFor(t, func() bool { return strings.Contains(string(card("bob")), `"work"`) && len(card("carol")) > 0 }, "both cards")

	b.cfg.Agents = []broker.AgentConfig{b.cfg.Agents[0], b.cfg.Agents[2]} // remove bob
	b.cfg.Agents[1].Capabilities = nil                                    // carol loses work
	b.write(t)
	if _, err := b.srv.ReloadFrom(b.path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(card("bob")) == 0 }, "bob's card to be cleared")
	waitFor(t, func() bool {
		c := card("carol")
		return len(c) > 0 && !strings.Contains(string(c), `"work"`)
	}, "carol's card to drop the revoked capability")
}

// A socket that never sends CONNECT must not hold up shutdown.
func TestCloseDoesNotWaitForHandshakes(t *testing.T) {
	b := startH(t, nil, broker.AgentConfig{ID: "alice"})
	conn, err := net.Dial("tcp", b.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = b.srv.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close took %s with a half-open connection", d)
	}
}

// Idle sockets that never log in can't keep a real agent out: once the
// pre-auth budget is full, the oldest of them is closed instead.
func TestIdleSocketsDoNotLockOutAgents(t *testing.T) {
	b := startH(t, func(c *broker.Config) { c.MaxConnections, c.MaxConnectionsPerIP = 16, 8 },
		broker.AgentConfig{ID: "alice"})
	for range 6 {
		c, err := net.Dial("tcp", b.srv.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	time.Sleep(100 * time.Millisecond)
	b.connect(t, "alice", true, nil) // fails the test if refused
}

// Logins racing a reload that removes the agent never crash the broker
// and never leave the removed agent connected.
func TestLoginRacingRemovalReload(t *testing.T) {
	b := startH(t, nil, broker.AgentConfig{ID: "ops"}, broker.AgentConfig{ID: "victim"})
	keep, gone := b.cfg, b.cfg
	gone.Agents = gone.Agents[:1]
	for i := range 40 {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			opts := paho.NewClientOptions().AddBroker(b.url).SetClientID("victim").SetUsername("victim").
				SetPassword(b.tokens["victim"]).SetAutoReconnect(false).SetConnectTimeout(time.Second)
			c := paho.NewClient(opts)
			if tok := c.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() == nil {
				time.Sleep(20 * time.Millisecond)
				c.Disconnect(0)
			}
		}()
		go func() {
			defer wg.Done()
			cfg := gone
			cfg.Agents = append([]broker.AgentConfig(nil), gone.Agents...)
			if _, err := b.srv.Reload(&cfg); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		for _, a := range b.srv.Status().Agents {
			if a.ID == "victim" {
				t.Fatalf("iteration %d: removed agent still listed", i)
			}
		}
		cfg := keep
		cfg.Agents = append([]broker.AgentConfig(nil), keep.Agents...)
		if _, err := b.srv.Reload(&cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	b := startH(t, func(c *broker.Config) { c.MetricsListen = "127.0.0.1:0" },
		broker.AgentConfig{ID: "alice"}, broker.AgentConfig{ID: "bob", Capabilities: []string{"work"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bob, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: "bob", Token: b.tokens["bob"], CleanSession: true,
		Logger: quietLog(), Tasks: map[string]client.Handler{"work": func(context.Context, *a2a.Envelope) (any, error) { return "done", nil }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bob.Close)
	alice := b.connect(t, "alice", true, nil)
	for range 3 {
		if _, err := alice.RequestTask(ctx, "work", 1); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + b.srv.MetricsAddr() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("/healthz = %d", code)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("/readyz = %d", code)
	}
	code, body := get("/metrics")
	for _, want := range []string{
		`agentbus_requests_total{target="task:work",outcome="answered"} 3`,
		`agentbus_requests_open{target="task:work"} 0`,
		`agentbus_request_duration_seconds_count{target="task:work"} 3`,
		`agentbus_agent_online{agent="bob"} 1`,
		`# TYPE agentbus_request_duration_seconds histogram`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %s", want)
		}
	}
	if code != 200 {
		t.Errorf("/metrics = %d", code)
	}
	if st := b.srv.Status(); len(st.Requests) != 1 || st.Requests[0].Answered != 3 {
		t.Errorf("status requests = %+v", st.Requests)
	}

	bad := broker.Config{Listen: "127.0.0.1:0", MetricsListen: "0.0.0.0:9464"}
	bad.ApplyDefaults()
	if err := bad.Validate(); err == nil {
		t.Error("non-loopback metrics_listen accepted without allow_insecure")
	}
}
