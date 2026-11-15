package broker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

type bus struct {
	t      *testing.T
	url    string
	tokens map[string]string
	logs   *syncBuffer // broker log output
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startBus runs a broker on a random port with agents alice, bob, carol.
// bob and carol may consume "summarize"; carol may only send to alice.
func startBus(t *testing.T, mutate func(*broker.Config)) *bus {
	t.Helper()
	b := &bus{t: t, tokens: map[string]string{}, logs: &syncBuffer{}}
	cfg := &broker.Config{Listen: "127.0.0.1:0"}
	for _, a := range []broker.AgentConfig{
		{ID: "alice"},
		{ID: "bob", Capabilities: []string{"summarize"}},
		{ID: "carol", Capabilities: []string{"summarize"}, SendTo: []string{"alice"}},
		{ID: "ops", Observer: true},
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		b.tokens[a.ID] = tok
		cfg.Agents = append(cfg.Agents, a)
	}
	if mutate != nil {
		mutate(cfg)
	}
	srv, err := broker.New(cfg, slog.New(slog.NewTextHandler(b.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	b.url = "tcp://" + srv.Addr()
	return b
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (b *bus) connect(id string, inbox client.Handler) *client.Client {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: b.url, AgentID: id, Token: b.tokens[id], Inbox: inbox,
		CleanSession: true, Logger: quietLog(), Description: id + " test agent",
	})
	if err != nil {
		b.t.Fatalf("connect %s: %v", id, err)
	}
	b.t.Cleanup(c.Close)
	return c
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// inboxRecorder collects envelopes delivered to an Inbox handler.
type inboxRecorder struct {
	mu  sync.Mutex
	got []*a2a.Envelope
	ch  chan *a2a.Envelope
}

func newRecorder() *inboxRecorder { return &inboxRecorder{ch: make(chan *a2a.Envelope, 64)} }

func (r *inboxRecorder) handler(_ context.Context, e *a2a.Envelope) (any, error) {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
	r.ch <- e
	return nil, nil
}

func (r *inboxRecorder) next(t *testing.T) *a2a.Envelope {
	t.Helper()
	select {
	case e := <-r.ch:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for inbox message")
		return nil
	}
}

func (r *inboxRecorder) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case e := <-r.ch:
		t.Fatalf("unexpected inbox message: %+v", e)
	case <-time.After(d):
	}
}

func TestRequestResponse(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", func(_ context.Context, e *a2a.Envelope) (any, error) {
		var in map[string]string
		if err := e.DecodeBody(&in); err != nil {
			return nil, err
		}
		return map[string]string{"echo": in["msg"], "from": e.From}, nil
	})
	alice := b.connect("alice", nil)

	reply, err := alice.Request(ctxT(t), "bob", map[string]string{"msg": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := reply.DecodeBody(&out); err != nil {
		t.Fatal(err)
	}
	if out["echo"] != "hello" || out["from"] != "alice" || reply.From != "bob" {
		t.Errorf("reply = %+v body %v", reply, out)
	}
}

func TestHandlerErrorBecomesRemoteError(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", func(context.Context, *a2a.Envelope) (any, error) { return nil, errors.New("model overloaded") })
	alice := b.connect("alice", nil)

	_, err := alice.Request(ctxT(t), "bob", "hi")
	var re *client.RemoteError
	if !errors.As(err, &re) || re.From != "bob" || re.Reason != "model overloaded" {
		t.Fatalf("err = %v", err)
	}
}

func TestBadTokenRejected(t *testing.T) {
	b := startBus(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: "alice", Token: "wrong", CleanSession: true, Logger: quietLog()})
	if err == nil {
		t.Fatal("connected with a bad token")
	}
}

func TestSendToNotAllowed(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", func(context.Context, *a2a.Envelope) (any, error) { return "should not run", nil })
	b.connect("alice", func(context.Context, *a2a.Envelope) (any, error) { return "hi carol", nil })
	carol := b.connect("carol", nil)

	_, err := carol.Request(ctxT(t), "bob", "hi")
	var re *client.RemoteError
	if !errors.As(err, &re) || re.From != a2a.BrokerID {
		t.Fatalf("want broker rejection, got %v", err)
	}
	// carol must still be connected and able to reach agents she may send to.
	if _, err := carol.Request(ctxT(t), "alice", "hi"); err != nil {
		t.Fatalf("follow-up request: %v", err)
	}
}

func TestSpoofedSenderRejected(t *testing.T) {
	b := startBus(t, nil)
	bobInbox := newRecorder()
	b.connect("bob", bobInbox.handler)
	aliceInbox := newRecorder()
	alice := b.connect("alice", aliceInbox.handler)

	env, _ := a2a.New(a2a.TypeEvent, "carol", "trust me") // alice claims to be carol
	env.To = "bob"
	if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
		t.Fatal(err)
	}
	rej := aliceInbox.next(t)
	if rej.Type != a2a.TypeError || rej.From != a2a.BrokerID || rej.CorrelationID != env.ID {
		t.Fatalf("rejection = %+v", rej)
	}
	bobInbox.none(t, 200*time.Millisecond)
}

func TestDuplicateDropped(t *testing.T) {
	b := startBus(t, nil)
	bobInbox := newRecorder()
	b.connect("bob", bobInbox.handler)
	alice := b.connect("alice", nil)

	env, _ := a2a.New(a2a.TypeEvent, "alice", "once")
	env.To = "bob"
	for range 2 {
		if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
			t.Fatal(err)
		}
	}
	bobInbox.next(t)
	bobInbox.none(t, 200*time.Millisecond)
}

func TestExpiredRejected(t *testing.T) {
	b := startBus(t, nil)
	aliceInbox := newRecorder()
	alice := b.connect("alice", aliceInbox.handler)

	env, _ := a2a.New(a2a.TypeEvent, "alice", "stale")
	env.To, env.TS, env.TTLMillis = "bob", time.Now().Add(-time.Minute), 1000
	if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
		t.Fatal(err)
	}
	if rej := aliceInbox.next(t); rej.Type != a2a.TypeError || rej.CorrelationID != env.ID {
		t.Fatalf("rejection = %+v", rej)
	}
}

func TestTaskQueueDeliversOnce(t *testing.T) {
	b := startBus(t, nil)
	var bobN, carolN atomic.Int32
	worker := func(n *atomic.Int32, name string) client.Handler {
		return func(context.Context, *a2a.Envelope) (any, error) {
			n.Add(1)
			return name, nil
		}
	}
	bob := b.connect("bob", nil)
	carol := b.connect("carol", nil)
	if err := bob.ServeTasks(ctxT(t), "summarize", worker(&bobN, "bob")); err != nil {
		t.Fatal(err)
	}
	if err := carol.ServeTasks(ctxT(t), "summarize", worker(&carolN, "carol")); err != nil {
		t.Fatal(err)
	}
	alice := b.connect("alice", nil)

	const n = 20
	for i := range n {
		if _, err := alice.RequestTask(ctxT(t), "summarize", i); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
	}
	if got := bobN.Load() + carolN.Load(); got != n {
		t.Errorf("handled %d tasks (bob %d, carol %d), want %d", got, bobN.Load(), carolN.Load(), n)
	}
}

func TestConsumeCapabilityDenied(t *testing.T) {
	b := startBus(t, nil)
	alice := b.connect("alice", nil)
	err := alice.ServeTasks(ctxT(t), "summarize", func(context.Context, *a2a.Envelope) (any, error) { return nil, nil })
	if err == nil {
		t.Fatal("alice consumed a capability she is not granted")
	}
}

func TestPresence(t *testing.T) {
	b := startBus(t, nil)
	alice := b.connect("alice", nil)
	bob := b.connect("bob", nil)
	if err := bob.ServeTasks(ctxT(t), "summarize", func(context.Context, *a2a.Envelope) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		p, ok := find(alice.Agents(), "bob")
		return ok && p.Status == a2a.StatusOnline && len(p.Capabilities) == 1 && p.Capabilities[0] == "summarize"
	}, "bob online with summarize")

	bob.Close()
	waitFor(t, func() bool {
		p, ok := find(alice.Agents(), "bob")
		return ok && p.Status == a2a.StatusOffline && p.Description == "bob test agent"
	}, "bob offline, card kept")
}

func TestRateLimit(t *testing.T) {
	b := startBus(t, func(c *broker.Config) { c.RatePerSecond, c.RateBurst = 0.01, 3 })
	aliceInbox := newRecorder()
	alice := b.connect("alice", aliceInbox.handler) // the presence publish takes 1 token
	b.connect("bob", nil)

	for range 3 {
		if err := alice.Send(ctxT(t), "bob", "x"); err != nil {
			t.Fatal(err)
		}
	}
	var rb broker.RejectBody
	rej := aliceInbox.next(t)
	if err := rej.DecodeBody(&rb); err != nil || rej.From != a2a.BrokerID || rb.Reason != "rate limit exceeded" {
		t.Fatalf("rejection = %+v body %+v", rej, rb)
	}
}

func find(ps []a2a.Presence, id string) (a2a.Presence, bool) {
	for _, p := range ps {
		if p.Agent == id {
			return p, true
		}
	}
	return a2a.Presence{}, false
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// rawConnect connects a plain MQTT client, the way a desktop tool would.
func (b *bus) rawConnect(id string, onMsg paho.MessageHandler) paho.Client {
	b.t.Helper()
	opts := paho.NewClientOptions().AddBroker(b.url).SetClientID(id).
		SetUsername(id).SetPassword(b.tokens[id]).SetDefaultPublishHandler(onMsg)
	c := paho.NewClient(opts)
	if tok := c.Connect(); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		b.t.Fatalf("raw connect %s: %v", id, tok.Error())
	}
	b.t.Cleanup(func() { c.Disconnect(100) })
	return c
}

// suback subscribes and returns the broker's code for filter.
func suback(t *testing.T, c paho.Client, filter string) byte {
	t.Helper()
	tok := c.Subscribe(filter, 1, nil)
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", filter, tok.Error())
	}
	// paho keys the result by the filter without any $share/<group>/ prefix.
	for _, code := range tok.(*paho.SubscribeToken).Result() {
		return code
	}
	t.Fatalf("no SUBACK result for %s", filter)
	return 0
}

func TestObserverSeesAllButConsumesNothing(t *testing.T) {
	b := startBus(t, nil)
	bob := b.connect("bob", nil)
	if err := bob.ServeTasks(ctxT(t), "summarize", func(context.Context, *a2a.Envelope) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	alice := b.connect("alice", nil)

	seen := make(chan string, 64)
	ops := b.rawConnect("ops", func(_ paho.Client, m paho.Message) { seen <- m.Topic() })
	for _, f := range []string{"#", "$SYS/#"} {
		if code := suback(t, ops, f); code >= 0x80 {
			t.Fatalf("observer refused %q: 0x%02x", f, code)
		}
	}
	if code := suback(t, ops, a2a.TaskShareFilter("summarize")); code < 0x80 {
		t.Error("observer allowed to consume tasks through a shared subscription")
	}

	if err := alice.Send(ctxT(t), "bob", "private"); err != nil {
		t.Fatal(err)
	}
	task, _ := a2a.New(a2a.TypeRequest, "alice", "work")
	task.Capability = "summarize"
	if err := alice.Publish(ctxT(t), a2a.TaskTopic("summarize"), task); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{a2a.InboxTopic("bob"): true, a2a.TaskTopic("summarize"): true}
	deadline := time.After(3 * time.Second)
	for len(want) > 0 {
		select {
		case topic := <-seen:
			delete(want, topic)
		case <-deadline:
			t.Fatalf("observer never saw %v", want)
		}
	}
}

func TestNonObserverCannotWiretap(t *testing.T) {
	b := startBus(t, nil)
	raw := b.rawConnect("alice", func(paho.Client, paho.Message) {})
	for _, f := range []string{"#", "a2a/v1/#", a2a.InboxTopic("bob"), "a2a/v1/agents/+/inbox"} {
		if code := suback(t, raw, f); code < 0x80 {
			t.Errorf("alice allowed to subscribe to %q", f)
		}
	}
	for _, f := range []string{a2a.PresenceFilter, a2a.Root + "/events/#", a2a.InboxTopic("alice")} {
		if code := suback(t, raw, f); code >= 0x80 {
			t.Errorf("alice refused %q: 0x%02x", f, code)
		}
	}
}

func TestSessionTakeoverWarns(t *testing.T) {
	b := startBus(t, nil)
	b.connect("alice", nil)
	b.rawConnect("alice", func(paho.Client, paho.Message) {}) // second process, same ID

	warn := regexp.MustCompile(`level=WARN msg="agent session taken over[^\n]*agent=alice new_remote=\S+ old_remote=\S+`)
	waitFor(t, func() bool {
		return warn.MatchString(b.logs.String()) &&
			strings.Contains(b.logs.String(), `msg="agent connection replaced" agent=alice`)
	}, "takeover warning naming the agent and both remotes")
}

func TestReconnectAfterCleanDisconnectDoesNotWarn(t *testing.T) {
	b := startBus(t, nil)
	b.connect("alice", nil).Close()
	waitFor(t, func() bool {
		return strings.Contains(b.logs.String(), `msg="agent disconnected" action=disconnect agent=alice`)
	}, "disconnect")
	b.connect("alice", nil)
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(b.logs.String(), "taken over") {
		t.Errorf("warned on a normal reconnect:\n%s", b.logs.String())
	}
}
