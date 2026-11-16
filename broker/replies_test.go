package broker_test

import (
	"context"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

// replyBus runs a broker where bob may only send to carol, so every reply
// bob sends alice is outside his send_to.
func replyBus(t *testing.T) *reloadBus {
	return newReloadBus(t,
		broker.AgentConfig{ID: "alice"},
		broker.AgentConfig{ID: "bob", Capabilities: []string{"work"}, SendTo: []string{"carol"}},
		broker.AgentConfig{ID: "carol"},
	)
}

// connectInbox connects id with an inbox handler and optional task handlers.
func (r *reloadBus) connectInbox(id string, inbox client.Handler, tasks map[string]client.Handler) *client.Client {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: r.url, AgentID: id, Token: r.tokens[id],
		CleanSession: true, Logger: quietLog(), Inbox: inbox, Tasks: tasks})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(c.Close)
	return c
}

// errorsTo collects the error envelopes an agent receives (broker refusals).
func errorsTo(ch chan<- *a2a.Envelope) client.Handler {
	return func(_ context.Context, e *a2a.Envelope) (any, error) {
		if e.Type == a2a.TypeError {
			ch <- e
		}
		return nil, nil
	}
}

func refusal(t *testing.T, ch <-chan *a2a.Envelope, what string) string {
	t.Helper()
	select {
	case e := <-ch:
		var b client.ErrorBody
		_ = e.DecodeBody(&b)
		return b.Code
	case <-time.After(3 * time.Second):
		t.Fatalf("no refusal for %s", what)
		return ""
	}
}

func TestReplyAllowedOutsideSendTo(t *testing.T) {
	r := replyBus(t)
	taskIDs := make(chan string, 1)
	r.connectInbox("bob",
		func(_ context.Context, e *a2a.Envelope) (any, error) { return "inbox answer", nil },
		map[string]client.Handler{"work": func(_ context.Context, e *a2a.Envelope) (any, error) {
			taskIDs <- e.ID
			return "task answer", nil
		}})
	aliceErrs := make(chan *a2a.Envelope, 4)
	alice := r.connectInbox("alice", errorsTo(aliceErrs), nil)

	for name, do := range map[string]func(context.Context) (*a2a.Envelope, error){
		"task":    func(ctx context.Context) (*a2a.Envelope, error) { return alice.RequestTask(ctx, "work", 1) },
		"request": func(ctx context.Context) (*a2a.Envelope, error) { return alice.Request(ctx, "bob", 1) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		reply, err := do(ctx)
		cancel()
		if err != nil {
			t.Errorf("%s: bob's reply didn't arrive: %v", name, err)
		} else if reply.From != "bob" {
			t.Errorf("%s: reply from %s", name, reply.From)
		}
	}
}

func TestReplyExceptionIsNarrow(t *testing.T) {
	r := replyBus(t)
	taskIDs := make(chan string, 1)
	bobErrs := make(chan *a2a.Envelope, 8)
	bob := r.connectInbox("bob", errorsTo(bobErrs), map[string]client.Handler{"work": func(_ context.Context, e *a2a.Envelope) (any, error) {
		taskIDs <- e.ID
		return "answer", nil
	}})
	alice := r.connectInbox("alice", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Unsolicited: still refused.
	if err := bob.Send(ctx, "alice", "hi"); err != nil {
		t.Fatal(err)
	}
	if code := refusal(t, bobErrs, "an unsolicited send"); code != "not_authorized" {
		t.Errorf("unsolicited send: %s", code)
	}

	// A reply to a request that was never sent: refused.
	forged, _ := a2a.New(a2a.TypeResponse, "bob", "forged")
	forged.To, forged.CorrelationID = "alice", "0123456789abcdef0123456789abcdef"
	if err := bob.Publish(ctx, a2a.InboxTopic("alice"), forged); err != nil {
		t.Fatal(err)
	}
	if code := refusal(t, bobErrs, "a reply to no request"); code != "not_authorized" {
		t.Errorf("forged reply: %s", code)
	}

	// A real task: the first reply goes through, a second one doesn't.
	if _, err := alice.RequestTask(ctx, "work", 1); err != nil {
		t.Fatalf("task: %v", err)
	}
	id := <-taskIDs
	again, _ := a2a.New(a2a.TypeResponse, "bob", "again")
	again.To, again.CorrelationID = "alice", id
	if err := bob.Publish(ctx, a2a.InboxTopic("alice"), again); err != nil {
		t.Fatal(err)
	}
	if code := refusal(t, bobErrs, "a second reply"); code != "not_authorized" {
		t.Errorf("second reply: %s", code)
	}
	// Events and requests to alice are not replies, even with a matching ID.
	ev, _ := a2a.New(a2a.TypeEvent, "bob", "x")
	ev.To, ev.CorrelationID = "alice", id
	if err := bob.Publish(ctx, a2a.InboxTopic("alice"), ev); err != nil {
		t.Fatal(err)
	}
	if code := refusal(t, bobErrs, "an event dressed as a reply"); code != "not_authorized" {
		t.Errorf("event: %s", code)
	}
}

func TestReplyAfterRequestExpiresIsRefused(t *testing.T) {
	r := replyBus(t)
	bobErrs := make(chan *a2a.Envelope, 4)
	r.connectInbox("bob", func(_ context.Context, e *a2a.Envelope) (any, error) {
		if e.Type == a2a.TypeError {
			bobErrs <- e
			return nil, nil
		}
		time.Sleep(700 * time.Millisecond) // past the request's TTL
		return "late", nil
	}, nil)
	alice := r.connectInbox("alice", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := alice.Request(ctx, "bob", 1); err == nil {
		t.Fatal("request answered before its handler finished")
	}
	if code := refusal(t, bobErrs, "a reply after the TTL"); code != "not_authorized" {
		t.Errorf("late reply: %s", code)
	}
}

// Only a request's answerer may reply to it, whatever send_to says. carol
// may send to alice, but her forged replies to alice's open task and
// direct request are refused, and bob's real replies still arrive.
func TestForgedRepliesAreRefused(t *testing.T) {
	r := replyBus(t)
	got := make(chan *a2a.Envelope, 8)
	alice := r.connectInbox("alice", func(_ context.Context, e *a2a.Envelope) (any, error) {
		got <- e
		return nil, nil
	}, nil)
	ids := make(chan string, 2)
	release := make(chan struct{})
	work := func(_ context.Context, e *a2a.Envelope) (any, error) {
		ids <- e.ID
		<-release
		return "from bob", nil
	}
	r.connectInbox("bob", work, map[string]client.Handler{"work": work})
	carolErrs := make(chan *a2a.Envelope, 4)
	carol := r.connectInbox("carol", errorsTo(carolErrs), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Published directly rather than with RequestTask and Request, so every
	// reply that reaches alice lands in her inbox handler.
	task, _ := a2a.New(a2a.TypeRequest, "alice", 1)
	task.Capability = "work"
	direct, _ := a2a.New(a2a.TypeRequest, "alice", 2)
	direct.To = "bob"
	if err := alice.Publish(ctx, a2a.TaskTopic("work"), task); err != nil {
		t.Fatal(err)
	}
	if err := alice.Publish(ctx, a2a.InboxTopic("bob"), direct); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		id := <-ids
		fake, _ := a2a.New(a2a.TypeResponse, "carol", "from carol")
		fake.To, fake.CorrelationID = "alice", id
		if err := carol.Publish(ctx, a2a.InboxTopic("alice"), fake); err != nil {
			t.Fatal(err)
		}
		if code := refusal(t, carolErrs, "a forged reply"); code != "not_authorized" {
			t.Errorf("forged reply to %s: %s", id, code)
		}
	}
	close(release)
	for range 2 {
		select {
		case e := <-got:
			if e.From != "bob" {
				t.Errorf("alice got a reply from %s", e.From)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("bob's reply never arrived")
		}
	}
	select {
	case e := <-got:
		t.Errorf("extra delivery: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}
