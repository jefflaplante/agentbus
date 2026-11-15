package client_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type testBus struct {
	url    string
	tokens map[string]string
}

func startBus(t *testing.T) *testBus {
	t.Helper()
	b := &testBus{tokens: map[string]string{}}
	cfg := &broker.Config{Listen: "127.0.0.1:0"}
	for _, a := range []broker.AgentConfig{
		{ID: "alice"}, {ID: "bob", Capabilities: []string{"summarize"}}, {ID: "carol"}, {ID: "ops", Observer: true},
	} {
		tok, hash := broker.NewToken()
		b.tokens[a.ID] = tok
		a.TokenSHA256 = hash
		cfg.Agents = append(cfg.Agents, a)
	}
	srv, err := broker.New(cfg, quiet())
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

func (b *testBus) connect(t *testing.T, id string, mutate func(*client.Options)) *client.Client {
	t.Helper()
	o := client.Options{BrokerURL: b.url, AgentID: id, Token: b.tokens[id], CleanSession: true, Logger: quiet()}
	if mutate != nil {
		mutate(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// recv waits for a value, failing the test instead of hanging.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func remoteCode(err error) string {
	var re *client.RemoteError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

func TestHandlerPanicBecomesError(t *testing.T) {
	b := startBus(t)
	b.connect(t, "bob", func(o *client.Options) {
		o.Inbox = func(context.Context, *a2a.Envelope) (any, error) { panic("boom") }
	})
	alice := b.connect(t, "alice", nil)
	if _, err := alice.Request(ctx5(t), "bob", "hi"); remoteCode(err) != "handler_panic" {
		t.Fatalf("want handler_panic, got %v", err)
	}
	// bob's process is still alive and answering.
	if _, err := alice.Request(ctx5(t), "bob", "again"); remoteCode(err) != "handler_panic" {
		t.Fatalf("second request: %v", err)
	}
}

func TestBusyWhenAtHandlerLimit(t *testing.T) {
	b := startBus(t)
	release := make(chan struct{})
	t.Cleanup(func() { safeClose(release) })
	started := make(chan struct{}, 4)
	b.connect(t, "bob", func(o *client.Options) {
		o.MaxConcurrent = 1
		o.Inbox = func(ctx context.Context, e *a2a.Envelope) (any, error) {
			started <- struct{}{}
			<-release
			return "done", nil
		}
	})
	alice := b.connect(t, "alice", nil)

	first := make(chan error, 1)
	go func() { _, err := alice.Request(ctx5(t), "bob", "1"); first <- err }()
	recv(t, started, "first request to start")
	if _, err := alice.Request(ctx5(t), "bob", "2"); remoteCode(err) != "busy" {
		t.Fatalf("want busy, got %v", err)
	}
	safeClose(release)
	if err := recv(t, first, "first request to finish"); err != nil {
		t.Fatalf("first request: %v", err)
	}
}

func TestForgedReplyIgnored(t *testing.T) {
	b := startBus(t)
	ids := make(chan string, 1)
	release := make(chan struct{})
	t.Cleanup(func() { safeClose(release) })
	b.connect(t, "bob", func(o *client.Options) {
		o.Inbox = func(ctx context.Context, e *a2a.Envelope) (any, error) {
			ids <- e.ID
			<-release
			return "real", nil
		}
	})
	carol := b.connect(t, "carol", nil)
	alice := b.connect(t, "alice", nil)

	result := make(chan *a2a.Envelope, 1)
	go func() {
		r, err := alice.Request(ctx5(t), "bob", "hi")
		if err != nil {
			t.Error(err)
		}
		result <- r
	}()
	id := recv(t, ids, "bob to receive the request")
	forged, _ := a2a.New(a2a.TypeResponse, "carol", "forged")
	forged.To, forged.CorrelationID = "alice", id
	if err := carol.Publish(ctx5(t), a2a.InboxTopic("alice"), forged); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the forgery arrive first
	safeClose(release)
	if r := recv(t, result, "the request to complete"); r == nil || r.From != "bob" {
		t.Fatalf("request completed by %+v, want bob", r)
	}
}

func TestBeforePublishCanBlock(t *testing.T) {
	b := startBus(t)
	b.connect(t, "bob", nil)
	alice := b.connect(t, "alice", func(o *client.Options) {
		o.BeforePublish = func(_ context.Context, topic string, e *a2a.Envelope) error {
			if e.Priority == a2a.PriorityHigh {
				return errors.New("high priority sends need approval")
			}
			return nil
		}
	})
	if err := alice.Send(ctx5(t), "bob", "fine"); err != nil {
		t.Fatalf("normal send: %v", err)
	}
	err := alice.Send(ctx5(t), "bob", "urgent", client.WithPriority(a2a.PriorityHigh))
	if err == nil || !strings.Contains(err.Error(), "need approval") {
		t.Fatalf("want blocked send, got %v", err)
	}
}

func TestMessageOptionsArrive(t *testing.T) {
	b := startBus(t)
	got := make(chan *a2a.Envelope, 1)
	b.connect(t, "bob", func(o *client.Options) {
		o.Inbox = func(_ context.Context, e *a2a.Envelope) (any, error) { got <- e; return nil, nil }
	})
	alice := b.connect(t, "alice", nil)
	err := alice.Send(ctx5(t), "bob", "ding", client.WithPriority(a2a.PriorityHigh),
		client.WithEvent("doorbell_ring"), client.WithHeaders(map[string]string{"session": "tg:1"}))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Priority != a2a.PriorityHigh || e.Event != "doorbell_ring" || e.Headers["session"] != "tg:1" {
			t.Errorf("envelope = %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not delivered")
	}
}

func safeClose(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// An observer sees other agents' requests and tasks but must never answer
// them; only the addressed agent or a real worker does.
func TestObserveSeesButNeverAnswers(t *testing.T) {
	b := startBus(t)
	b.connect(t, "bob", func(o *client.Options) {
		o.Inbox = func(context.Context, *a2a.Envelope) (any, error) { return "from bob", nil }
		o.Tasks = map[string]client.Handler{"summarize": func(context.Context, *a2a.Envelope) (any, error) { return "task by bob", nil }}
	})
	seen := make(chan string, 64)
	ops := b.connect(t, "ops", nil)
	if err := ops.Observe(ctx5(t), "a2a/v1/#", func(topic string, _ []byte) { seen <- topic }); err != nil {
		t.Fatal(err)
	}
	alice := b.connect(t, "alice", nil)
	if err := alice.Observe(ctx5(t), "#", func(string, []byte) {}); err == nil {
		t.Error("non-observer allowed to observe #")
	}

	for range 5 {
		r, err := alice.Request(ctx5(t), "bob", "hi")
		if err != nil || r.From != "bob" {
			t.Fatalf("request answered by %v: %v", r, err)
		}
		r, err = alice.RequestTask(ctx5(t), "summarize", "work")
		if err != nil || r.From != "bob" {
			t.Fatalf("task answered by %v: %v", r, err)
		}
	}
	want := map[string]bool{a2a.InboxTopic("bob"): false, a2a.TaskTopic("summarize"): false, a2a.InboxTopic("alice"): false}
	deadline := time.After(3 * time.Second)
	for missing := len(want); missing > 0; {
		select {
		case topic := <-seen:
			if done, tracked := want[topic]; tracked && !done {
				want[topic] = true
				missing--
			}
		case <-deadline:
			t.Fatalf("observer missed topics: %v", want)
		}
	}
}
