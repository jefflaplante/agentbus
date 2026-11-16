package main

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

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/client"
)

// mcpSession connects agent "cli" to the bus, serves it over an in-memory
// MCP transport, and returns the client side of that MCP session.
func mcpSession(t *testing.T, b *cliBus) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ib := newMCPInbox()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: b.url, AgentID: "cli", Token: b.tokens["cli"], CleanSession: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Inbox: ib.handle,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	st, ct := mcp.NewInMemoryTransports()
	srvCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = newMCPServer(c, ib, "cli", "test").Run(srvCtx, st) }()
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
	b := startCLIBus(t)
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
	b := startCLIBus(t)
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
	ids := regexp.MustCompile(`call reply with id "([0-9a-f]+)" within`).FindAllStringSubmatch(text, -1)
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
	ib := newMCPInbox()
	e, _ := a2a.New(a2a.TypeRequest, "bob", "q")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ib.handle(ctx, e); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v", err)
	}
	if ib.answer(e.ID, mcpAnswer{body: 1}) {
		t.Error("answered an expired request")
	}
	if msgs, _ := ib.take(context.Background(), 0); len(msgs) != 1 {
		t.Errorf("inbox holds %d messages", len(msgs))
	}
}

func TestMCPInboxDropsOldest(t *testing.T) {
	ib := newMCPInbox()
	for range mcpInboxMax + 5 {
		e, _ := a2a.New(a2a.TypeEvent, "bob", 1)
		_, _ = ib.handle(context.Background(), e)
	}
	msgs, dropped := ib.take(context.Background(), 0)
	if len(msgs) != mcpInboxMax || dropped != 5 {
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
