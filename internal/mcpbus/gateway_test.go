package mcpbus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

// startGateway serves a gateway for b over HTTP and returns its /mcp URL.
func startGateway(t *testing.T, b *testBus, tune func(*GatewayOptions)) (string, *Gateway) {
	t.Helper()
	o := GatewayOptions{
		BrokerURL: b.url, Version: "test", Sweep: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Lookup: func(token string) (string, bool, bool) {
			a, ok := b.srv.AgentForToken(token)
			return a.ID, a.Observer, ok
		},
	}
	if tune != nil {
		tune(&o)
	}
	gw := NewGateway(o)
	mux := http.NewServeMux()
	mux.Handle("/mcp", gw.Handler())
	hs := httptest.NewServer(mux)
	t.Cleanup(func() { gw.Close(); hs.Close() })
	return hs.URL + "/mcp", gw
}

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	if b.next == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return b.next.RoundTrip(r)
}

// httpSession opens an MCP session to endpoint with token.
func httpSession(t *testing.T, endpoint, token string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	return httpSessionRetries(t, endpoint, token, 0, opts)
}

// httpSessionRetries is httpSession with the client's stream-resumption
// retries set (0: the SDK default; negative: off).
func httpSessionRetries(t *testing.T, endpoint, token string, retries int, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: endpoint, MaxRetries: retries,
		HTTPClient: &http.Client{Transport: bearer{token: token}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Two clients with one token share one bus connection and one inbox, and
// neither knocks the other off the bus.
func TestGatewayOneConnectionPerAgent(t *testing.T) {
	b := startTestBus(t)
	b.bob(t, nil)
	endpoint, gw := startGateway(t, b, nil)
	one := httpSession(t, endpoint, b.tokens["cli"], nil)
	two := httpSession(t, endpoint, b.tokens["cli"], nil)

	if got := gw.Agents(); !slices.Equal(got, []string{"cli"}) {
		t.Fatalf("gateway connections = %v, want just cli", got)
	}
	for i, cs := range []*mcp.ClientSession{one, two, one, two} {
		if out, isErr := call(t, cs, "task", map[string]any{"capability": "stats", "body": "abc"}); isErr || !strings.Contains(out, `"bytes"`) {
			t.Fatalf("call %d: %v %s", i, isErr, out)
		}
	}
	// A message for cli is read once, by whichever session asks first.
	sender := connectAs(t, b, "dana")
	if err := sender.Send(context.Background(), "cli", "hello", client.WithEvent("note")); err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, two, "inbox", map[string]any{"wait_seconds": 3})
	if !strings.Contains(out, `"event": "note"`) {
		t.Fatalf("second session's inbox: %s", out)
	}
	if out, _ := call(t, one, "inbox", nil); out != "The inbox is empty." {
		t.Errorf("first session saw it too: %s", out)
	}
}

// connectAs connects to the bus as agent id, for sending.
func connectAs(t *testing.T, b *testBus, id string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: id, Token: b.tokens[id], CleanSession: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// The inbox belongs to the agent, not to a session: a message that
// arrives between sessions is there for the next one.
func TestGatewayInboxOutlivesSessions(t *testing.T) {
	b := startTestBus(t)
	endpoint, _ := startGateway(t, b, nil)
	first := httpSession(t, endpoint, b.tokens["cli"], nil)
	call(t, first, "agents", nil)
	_ = first.Close()

	bob := connectAs(t, b, "dana")
	if err := bob.Send(context.Background(), "cli", "while you were out", client.WithEvent("note")); err != nil {
		t.Fatal(err)
	}
	second := httpSession(t, endpoint, b.tokens["cli"], nil)
	if out, _ := call(t, second, "inbox", map[string]any{"wait_seconds": 3}); !strings.Contains(out, "while you were out") {
		t.Fatalf("inbox in the new session: %s", out)
	}
}

// postInit sends an MCP initialize request with the given Authorization
// header and returns the HTTP status.
func postInit(t *testing.T, endpoint, authz string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestGatewayRefuses(t *testing.T) {
	b := startTestBus(t)
	endpoint, gw := startGateway(t, b, nil)
	for _, c := range []struct {
		name, authz string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer not-a-real-token", http.StatusUnauthorized},
		{"observer", "Bearer " + b.tokens["ops"], http.StatusForbidden},
		{"agent", "Bearer " + b.tokens["cli"], http.StatusOK},
	} {
		if got := postInit(t, endpoint, c.authz); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	if got := gw.Agents(); !slices.Equal(got, []string{"cli"}) {
		t.Errorf("connections = %v: refused requests must not connect", got)
	}
	// Too many bad tokens from one address: refused outright for a while.
	for range 25 {
		postInit(t, endpoint, "Bearer guess")
	}
	if got := postInit(t, endpoint, "Bearer "+b.tokens["cli"]); got != http.StatusTooManyRequests {
		t.Errorf("after many bad tokens: status %d, want 429", got)
	}
}

// Rotating a token in the config cuts off the old one at once: its
// requests get 401 and the agent's bus connection closes.
func TestGatewayRevokedToken(t *testing.T) {
	b := startTestBus(t)
	endpoint, gw := startGateway(t, b, nil)
	cs := httpSession(t, endpoint, b.tokens["cli"], nil)
	call(t, cs, "agents", nil)

	next := *b.cfg
	next.Agents = slices.Clone(b.cfg.Agents)
	tok, hash := broker.NewToken()
	for i := range next.Agents {
		if next.Agents[i].ID == "cli" {
			next.Agents[i].TokenSHA256 = hash
		}
	}
	if _, err := b.srv.Reload(&next); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the gateway to drop cli", func() bool { return len(gw.Agents()) == 0 })
	if got := postInit(t, endpoint, "Bearer "+b.tokens["cli"]); got != http.StatusUnauthorized {
		t.Errorf("old token: status %d, want 401", got)
	}
	fresh := httpSession(t, endpoint, tok, nil)
	if _, isErr := call(t, fresh, "agents", nil); isErr {
		t.Error("new token doesn't work")
	}
}

// An agent's connection closes once idle, but not while something is
// held for it.
func TestGatewayIdleClose(t *testing.T) {
	b := startTestBus(t)
	endpoint, gw := startGateway(t, b, func(o *GatewayOptions) { o.IdleClose = 200 * time.Millisecond })
	cs := httpSession(t, endpoint, b.tokens["cli"], &mcp.ClientOptions{})
	call(t, cs, "agents", nil)

	bob := connectAs(t, b, "dana")
	if err := bob.Send(context.Background(), "cli", "unread", client.WithEvent("note")); err != nil {
		t.Fatal(err)
	}
	_ = cs.Close()
	time.Sleep(600 * time.Millisecond)
	if len(gw.Agents()) != 1 {
		t.Fatal("closed while an unread message was held")
	}
	reader := httpSession(t, endpoint, b.tokens["cli"], nil)
	if out, _ := call(t, reader, "inbox", map[string]any{"wait_seconds": 2}); !strings.Contains(out, "unread") {
		t.Fatalf("inbox: %s", out)
	}
	_ = reader.Close()
	eventually(t, "the idle connection to close", func() bool { return len(gw.Agents()) == 0 })
}

// idleCutter is a reverse proxy that drops a response stream when nothing
// arrives for idle, as load balancers do.
func idleCutter(t *testing.T, target string, idle time.Duration) string {
	t.Helper()
	u, _ := url.Parse(target)
	rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: u.Scheme, Host: u.Host})
	rp.FlushInterval = -1
	rp.Transport = cutTransport{idle: idle}
	rp.ErrorLog = nil
	hs := httptest.NewServer(rp)
	t.Cleanup(hs.Close)
	return hs.URL + u.Path
}

type cutTransport struct{ idle time.Duration }

func (c cutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		resp.Body = &idleBody{rc: resp.Body, idle: c.idle}
	}
	return resp, err
}

type idleBody struct {
	rc   io.ReadCloser
	idle time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	type res struct {
		buf []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, len(p))
		n, err := b.rc.Read(buf)
		ch <- res{buf[:n], err}
	}()
	select {
	case r := <-ch:
		return copy(p, r.buf), r.err
	case <-time.After(b.idle):
		_ = b.rc.Close()
		return 0, errors.New("idle timeout")
	}
}

func (b *idleBody) Close() error { return b.rc.Close() }

// A request that takes longer than a proxy's idle timeout survives,
// because the gateway keeps both streams busy while it waits: progress on
// the tool call's stream, and pings on the client's standalone stream
// (which a proxy would otherwise cut, ending the session).
func TestGatewayLongWaitThroughIdleProxy(t *testing.T) {
	for _, c := range []struct {
		name      string
		keepalive time.Duration // progress and ping interval
		retries   int           // the client's stream resumption; negative is off
		ok        bool
	}{
		{"progress and pings keep the streams open", 150 * time.Millisecond, -1, true},
		{"quiet streams are cut", time.Hour, -1, false}, // shows the proxy really cuts
		{"a cut stream resumes from the event store", time.Hour, 5, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := startTestBus(t)
			b.bob(t, func(ctx context.Context, e *a2a.Envelope) (any, error) {
				select {
				case <-time.After(1500 * time.Millisecond):
				case <-ctx.Done():
				}
				return "worth the wait", nil
			})
			endpoint, _ := startGateway(t, b, func(o *GatewayOptions) { o.Progress, o.KeepAlive = c.keepalive, c.keepalive })
			var progress atomic.Int32
			cs := httpSessionRetries(t, idleCutter(t, endpoint, 500*time.Millisecond), b.tokens["cli"], c.retries, &mcp.ClientOptions{
				ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) { progress.Add(1) },
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			params := &mcp.CallToolParams{Name: "request", Arguments: map[string]any{"to": "bob", "body": "q", "timeout_seconds": 5}}
			params.SetProgressToken("p1")
			res, err := cs.CallTool(ctx, params)
			got := err == nil && !res.IsError && strings.Contains(textOf(res), "worth the wait")
			if got != c.ok {
				t.Fatalf("result ok = %v, want %v (err %v, progress notes %d)", got, c.ok, err, progress.Load())
			}
			if c.ok && c.retries < 0 && progress.Load() < 3 {
				t.Errorf("only %d progress notifications", progress.Load())
			}
		})
	}
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}
