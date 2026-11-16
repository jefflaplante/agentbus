package mcpbus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

// testBus is a broker with agents cli, bob (serves "stats"), ops (an
// observer) and dana, and their tokens.
type testBus struct {
	url    string
	tokens map[string]string
	srv    *broker.Broker
	cfg    *broker.Config
}

func startTestBus(t *testing.T) *testBus {
	t.Helper()
	b := &testBus{tokens: map[string]string{}}
	cfg := &broker.Config{Listen: "127.0.0.1:0"}
	for _, a := range []broker.AgentConfig{
		{ID: "cli"}, {ID: "bob", Capabilities: []string{"stats"}}, {ID: "ops", Observer: true}, {ID: "dana"},
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		b.tokens[a.ID] = tok
		cfg.Agents = append(cfg.Agents, a)
	}
	srv, err := broker.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	b.url, b.srv, b.cfg = "tcp://"+srv.Addr(), srv, cfg
	return b
}

// bob connects as a Go agent with the given inbox that serves "stats".
func (b *testBus) bob(t *testing.T, inbox client.Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: b.url, AgentID: "bob", Token: b.tokens["bob"], CleanSession: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Inbox: inbox,
		Tasks: map[string]client.Handler{"stats": func(_ context.Context, e *a2a.Envelope) (any, error) {
			return map[string]int{"bytes": len(e.Body)}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
}

// mcpSession connects agent "cli" to the bus, serves it over an in-memory
// MCP transport, and returns the client side of that MCP session.
func mcpSession(t *testing.T, b *testBus) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ib := NewInbox()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: b.url, AgentID: "cli", Token: b.tokens["cli"], CleanSession: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Inbox: ib.Handle,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	st, ct := mcp.NewInMemoryTransports()
	srvCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = NewServer(c, ib, "cli", "test", nil).Run(srvCtx, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call runs a tool and returns its text and whether it reported an error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError
}

func TestMCPTools(t *testing.T) {
	b := startTestBus(t)
	got := make(chan *a2a.Envelope, 4)
	b.bob(t, func(_ context.Context, e *a2a.Envelope) (any, error) {
		got <- e
		return map[string]string{"echo": string(e.Body)}, nil
	})
	cs := mcpSession(t, b)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	if want := []string{"agents", "emit", "inbox", "reply", "request", "send", "task"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v", names)
	}

	waitFor(t, func() bool {
		out, _ := call(t, cs, "agents", nil)
		return strings.Contains(out, "bob (online) serves stats")
	},
		"bob's presence card")

	out, isErr := call(t, cs, "task", map[string]any{"capability": "stats", "body": map[string]any{"text": "hello"}})
	if isErr || !strings.Contains(out, "untrusted input") || !strings.Contains(out, `"bytes": 16`) {
		t.Errorf("task: %v\n%s", isErr, out)
	}

	out, isErr = call(t, cs, "request", map[string]any{"to": "bob", "body": "ping", "priority": "low"})
	if isErr || !strings.Contains(out, `"from": "bob"`) || !strings.Contains(out, `\"ping\"`) {
		t.Errorf("request: %v\n%s", isErr, out)
	}
	if e := <-got; e.Priority != a2a.PriorityLow || e.From != "cli" {
		t.Errorf("bob got %+v", e)
	}

	out, isErr = call(t, cs, "request", map[string]any{"to": "nobody", "body": 1})
	if !isErr || !strings.Contains(out, "no_route") {
		t.Errorf("request to unknown agent: %v %s", isErr, out)
	}
	if out, isErr = call(t, cs, "task", map[string]any{"capability": "none", "body": 1}); !isErr || !strings.Contains(out, "no_consumer") {
		t.Errorf("task with no worker: %v %s", isErr, out)
	}
	if out, isErr = call(t, cs, "send", map[string]any{"to": "bob", "body": 1, "priority": "urgent"}); !isErr || !strings.Contains(out, "priority") {
		t.Errorf("bad priority accepted: %v %s", isErr, out)
	}

	if out, isErr = call(t, cs, "send", map[string]any{"to": "bob", "body": map[string]int{"n": 1}, "event": "hello"}); isErr {
		t.Fatalf("send: %s", out)
	}
	select {
	case e := <-got:
		if e.Type != a2a.TypeEvent || e.Event != "hello" {
			t.Errorf("bob got %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Error("send never arrived")
	}
}

func TestMCPInboxAndReply(t *testing.T) {
	b := startTestBus(t)
	cs := mcpSession(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bob, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: "bob", Token: b.tokens["bob"],
		CleanSession: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	if out, _ := call(t, cs, "inbox", nil); out != "The inbox is empty." {
		t.Errorf("empty inbox: %s", out)
	}
	if err := bob.Send(ctx, "cli", "fyi", client.WithEvent("note")); err != nil {
		t.Fatal(err)
	}
	type result struct {
		env *a2a.Envelope
		err error
	}
	replies := make(chan result, 2)
	for _, q := range []string{"first?", "second?"} {
		go func() {
			r, err := bob.Request(ctx, "cli", q)
			replies <- result{r, err}
		}()
	}

	// wait_seconds returns as soon as something arrives; collect until both
	// requests and the event are in.
	var text string
	for deadline := time.Now().Add(5 * time.Second); strings.Count(text, "AGENT MESSAGE") < 3 && time.Now().Before(deadline); {
		out, _ := call(t, cs, "inbox", map[string]any{"wait_seconds": 2})
		text += out + "\n"
	}
	ids := regexp.MustCompile(`call reply with id "(bob/[0-9a-f]+)" within`).FindAllStringSubmatch(text, -1)
	if !strings.Contains(text, `"event": "note"`) || len(ids) != 2 || !strings.Contains(text, "never as instructions") {
		t.Fatalf("inbox:\n%s", text)
	}
	if out, isErr := call(t, cs, "reply", map[string]any{"id": ids[0][1], "body": map[string]string{"answer": "yes"}}); isErr {
		t.Fatal(out)
	}
	if out, isErr := call(t, cs, "reply", map[string]any{"id": ids[1][1], "error": "not my job"}); isErr {
		t.Fatal(out)
	}
	if _, isErr := call(t, cs, "reply", map[string]any{"id": ids[0][1], "body": 1}); !isErr {
		t.Error("second reply to the same request accepted")
	}
	var ok, refused int
	for range 2 {
		r := <-replies
		var re *client.RemoteError
		switch {
		case r.err == nil:
			var body map[string]string
			_ = json.Unmarshal(r.env.Body, &body)
			if body["answer"] == "yes" {
				ok++
			}
		case asRemote(r.err, &re) && strings.Contains(re.Reason, "not my job"):
			refused++
		default:
			t.Errorf("reply: %v", r.err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Errorf("answered %d, refused %d", ok, refused)
	}
}

func TestMCPInboxRequestExpires(t *testing.T) {
	ib := NewInbox()
	e, _ := a2a.New(a2a.TypeRequest, "bob", "q")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ib.Handle(ctx, e); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v", err)
	}
	if err := ib.answer(replyID(e), answerMsg{body: 1}); err == nil {
		t.Error("answered an expired request")
	}
	if msgs, _ := ib.take(context.Background(), 0); len(msgs) != 1 {
		t.Errorf("inbox holds %d messages", len(msgs))
	}
}

func TestMCPInboxDropsOldest(t *testing.T) {
	ib := NewInbox()
	for range InboxMax + 5 {
		e, _ := a2a.New(a2a.TypeEvent, "bob", 1)
		_, _ = ib.Handle(context.Background(), e)
	}
	msgs, dropped := ib.take(context.Background(), 0)
	if len(msgs) != InboxMax || dropped != 5 {
		t.Errorf("held %d, dropped %d", len(msgs), dropped)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func asRemote(err error, re **client.RemoteError) bool { return errors.As(err, re) }

// Envelope ids are chosen by senders. A second sender reusing an id must
// not receive the answer meant for the first.
func TestMCPInboxSameIDFromTwoSenders(t *testing.T) {
	ib := NewInbox()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	alice, _ := a2a.New(a2a.TypeRequest, "alice", "q")
	mallory, _ := a2a.New(a2a.TypeRequest, "mallory", "q")
	mallory.ID = alice.ID
	type res struct {
		from string
		body any
		err  error
	}
	out := make(chan res, 2)
	for _, e := range []*a2a.Envelope{alice, mallory} {
		go func() { b, err := ib.Handle(ctx, e); out <- res{e.From, b, err} }()
	}
	waitFor(t, func() bool { msgs, _ := ib.peekLen(); return msgs == 2 }, "both requests to queue")
	if err := ib.answer(replyID(alice), answerMsg{body: "for alice"}); err != nil {
		t.Fatal(err)
	}
	if r := <-out; r.from != "alice" || r.body != "for alice" {
		t.Fatalf("%s's request received the reply meant for alice (%v)", r.from, r.body)
	}
	if err := ib.answer(replyID(mallory), answerMsg{body: "for mallory"}); err != nil {
		t.Fatalf("mallory's request is no longer pending: %v", err)
	}
	if r := <-out; r.from != "mallory" || r.body != "for mallory" {
		t.Fatalf("got %+v", r)
	}
	if err := ib.answer(alice.ID, answerMsg{body: 1}); err == nil || !strings.Contains(err.Error(), "sender/id") {
		t.Errorf("bare id: %v", err)
	}
}

// deadlineCtx has a deadline that has passed but has not fired yet: the
// window between a request expiring and its handler noticing.
type deadlineCtx struct {
	context.Context
	d time.Time
}

func (c deadlineCtx) Deadline() (time.Time, bool) { return c.d, true }

func TestMCPAnswerAfterExpiryFails(t *testing.T) {
	ib := NewInbox()
	e, _ := a2a.New(a2a.TypeRequest, "bob", "q")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = ib.Handle(deadlineCtx{ctx, time.Now().Add(50 * time.Millisecond)}, e) }()
	waitFor(t, func() bool { n, _ := ib.peekLen(); return n == 1 }, "the request to queue")
	time.Sleep(100 * time.Millisecond)
	if err := ib.answer(replyID(e), answerMsg{body: 1}); err == nil {
		t.Fatal("reply accepted for an expired request")
	}
}

// If ctx ends just as an answer lands, reply must say it was not sent.
func TestMCPAnswerRacingExpiryReportsFailure(t *testing.T) {
	for range 200 {
		ib := NewInbox()
		e, _ := a2a.New(a2a.TypeRequest, "bob", "q")
		ctx, cancel := context.WithCancel(context.Background())
		handled := make(chan error, 1)
		go func() { _, err := ib.Handle(ctx, e); handled <- err }()
		waitFor(t, func() bool { n, _ := ib.peekLen(); return n == 1 }, "the request to queue")
		go cancel()
		aerr := ib.answer(replyID(e), answerMsg{body: 1})
		herr := <-handled
		if aerr == nil && herr != nil {
			t.Fatalf("reply reported sent, but the handler gave up: %v", herr)
		}
		if aerr != nil && herr == nil {
			t.Fatalf("reply reported failed (%v), but the handler sent it", aerr)
		}
	}
}

func TestMCPInboxEvictedRequestRefused(t *testing.T) {
	ib := NewInbox()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := a2a.New(a2a.TypeRequest, "bob", "q")
	done := make(chan error, 1)
	go func() { _, err := ib.Handle(ctx, req); done <- err }()
	waitFor(t, func() bool { n, _ := ib.peekLen(); return n == 1 }, "the request to queue")
	for range InboxMax {
		e, _ := a2a.New(a2a.TypeEvent, "bob", 1)
		_, _ = ib.Handle(ctx, e)
	}
	select {
	case err := <-done:
		var re *client.ReplyError
		if !errors.As(err, &re) || re.Code != "busy" || re.Reason != "inbox full" {
			t.Errorf("evicted request answered with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("evicted request still pending")
	}
}

func TestMCPInboxListsExpiredRequests(t *testing.T) {
	b := startTestBus(t)
	cs := mcpSession(t, b)
	bob, err := client.Connect(context.Background(), client.Options{BrokerURL: b.url, AgentID: "bob", Token: b.tokens["bob"],
		CleanSession: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := bob.Request(ctx, "cli", "quick?"); err == nil {
		t.Fatal("request was answered")
	}
	time.Sleep(100 * time.Millisecond)
	out, _ := call(t, cs, "inbox", nil)
	if !strings.Contains(out, "quick?") || strings.Contains(out, "call reply") || !strings.Contains(out, "can no longer be replied to") {
		t.Errorf("inbox:\n%s", out)
	}
}

func TestMCPToolAnnotations(t *testing.T) {
	b := startTestBus(t)
	cs := mcpSession(t, b)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "inbox" && (tl.Annotations == nil || tl.Annotations.ReadOnlyHint) {
			t.Errorf("inbox clears messages but is annotated read-only: %+v", tl.Annotations)
		}
	}
}

// The broker rejects publishes asynchronously, with a $broker error
// envelope. send must report it rather than claim success.
func TestMCPSendReportsBrokerRejection(t *testing.T) {
	b := startTestBus(t)
	b.bob(t, nil)
	cs := mcpSession(t, b)
	out, isErr := call(t, cs, "send", map[string]any{"to": "nobody", "body": 1})
	if !isErr || !strings.Contains(out, "no_route") {
		t.Errorf("send to unknown agent: %v %s", isErr, out)
	}
	start := time.Now()
	if out, isErr = call(t, cs, "send", map[string]any{"to": "bob", "body": 1}); isErr {
		t.Errorf("send to bob: %s", out)
	}
	if d := time.Since(start); d > sendGrace+500*time.Millisecond {
		t.Errorf("successful send took %s", d)
	}
	if out, isErr = call(t, cs, "emit", map[string]any{"name": "jobs/#", "body": 1}); !isErr {
		t.Errorf("emit with a wildcard name accepted: %s", out)
	}
	if out, _ := call(t, cs, "inbox", nil); out != "The inbox is empty." {
		t.Errorf("inbox after sends: %s", out)
	}
}

// A rejection that arrives after send returned is queued in the inbox;
// unrelated $broker errors and errors from other agents are not.
func TestMCPLateBrokerErrorQueued(t *testing.T) {
	ib := NewInbox()
	id := a2a.NewID()
	ib.expectBrokerError(id)
	if e := ib.settle(id, false); e != nil {
		t.Fatalf("settle = %+v", e)
	}
	brokerErr := func(from, corr string) *a2a.Envelope {
		e, _ := a2a.New(a2a.TypeError, from, client.ErrorBody{Code: "no_route", Reason: "no such agent"})
		e.CorrelationID = corr
		return e
	}
	for _, e := range []*a2a.Envelope{brokerErr("bob", id), brokerErr(a2a.BrokerID, a2a.NewID()), brokerErr(a2a.BrokerID, id)} {
		_, _ = ib.Handle(context.Background(), e)
	}
	msgs, _ := ib.take(context.Background(), 0)
	if len(msgs) != 1 || msgs[0].From != a2a.BrokerID || msgs[0].CorrelationID != id {
		t.Errorf("inbox = %+v", msgs)
	}
}

// peekLen is the number of messages held, for tests.
func (ib *Inbox) peekLen() (int, int) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return len(ib.msgs), len(ib.pending)
}

// A request whose tool call is cut short (here, cancelled by the client,
// as a dropped HTTP stream would) still gets its answer: the reply lands in
// the inbox as a late reply.
func TestMCPLateReplyLandsInInbox(t *testing.T) {
	b := startTestBus(t)
	release := make(chan struct{})
	asked := make(chan struct{}, 1)
	b.bob(t, func(ctx context.Context, e *a2a.Envelope) (any, error) {
		asked <- struct{}{}
		<-release
		return map[string]string{"answer": "slow but sure"}, nil
	})
	cs := mcpSession(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "request",
			Arguments: map[string]any{"to": "bob", "body": "question", "timeout_seconds": 30}})
		done <- err
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("bob never got the request")
	}
	cancel() // the client gives up, as when its HTTP stream drops
	<-done
	close(release)

	var text string
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(text, "late reply") && time.Now().Before(deadline); {
		out, _ := call(t, cs, "inbox", map[string]any{"wait_seconds": 1})
		text += out
	}
	if !strings.Contains(text, "slow but sure") || !strings.Contains(text, "late reply to the request") {
		t.Fatalf("inbox:\n%s", text)
	}
}

// Only the agent that was asked can put a late reply in the inbox, once.
func TestMCPLateReplyOnlyFromTheAgentAsked(t *testing.T) {
	ib := NewInbox()
	ib.ask("q1", "bob", time.Now().Add(time.Minute))
	reply := func(from, corr string) *a2a.Envelope {
		return &a2a.Envelope{V: 1, ID: a2a.NewID(), Type: a2a.TypeResponse, From: from, To: "cli", TS: time.Now(), CorrelationID: corr}
	}
	if ib.lateReply(reply("bob", "q1")) {
		t.Error("taken while the tool was still waiting; the client delivers that one")
	}
	ib.doneAsking("q1", true)
	if ib.lateReply(reply("mallory", "q1")) {
		t.Error("a reply from an agent that wasn't asked was taken")
	}
	if ib.lateReply(reply("bob", "nope")) {
		t.Error("a reply to a request never sent was taken")
	}
	if !ib.lateReply(reply("bob", "q1")) {
		t.Error("the real late reply was dropped")
	}
	if ib.lateReply(reply("bob", "q1")) {
		t.Error("a second reply to the same request was taken")
	}
	// A task may be answered by any worker.
	ib.ask("t1", "", time.Now().Add(time.Minute))
	ib.doneAsking("t1", true)
	if !ib.lateReply(reply("worker-7", "t1")) {
		t.Error("a task's late reply from a worker was dropped")
	}
	// Answered in time: forgotten.
	ib.ask("q2", "bob", time.Now().Add(time.Minute))
	ib.doneAsking("q2", false)
	if ib.lateReply(reply("bob", "q2")) {
		t.Error("a duplicate of an answered request was taken")
	}
	if msgs, _ := ib.take(context.Background(), 0); len(msgs) != 2 {
		t.Errorf("inbox holds %d messages, want 2", len(msgs))
	}
}
