package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// model is a fake OpenAI-compatible server. Each call gets the next
// scripted assistant message; the requests are kept for checking.
type model struct {
	mu    sync.Mutex
	turns []string // JSON of choices[0].message
	reqs  []string
}

func (m *model) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		n := len(m.reqs)
		m.reqs = append(m.reqs, string(raw))
		m.mu.Unlock()
		if n >= len(m.turns) {
			t.Errorf("model called %d times; only %d turns scripted", n+1, len(m.turns))
			w.WriteHeader(500)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, m.turns[n])
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (m *model) request(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.reqs) {
		return ""
	}
	return m.reqs[i]
}

func callTurn(name, args string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`{"tool_calls":[{"id":"c-%s","type":"function","function":{"name":%q,"arguments":%s}}]}`, name, name, a)
}

func textTurn(s string) string {
	b, _ := json.Marshal(s)
	return `{"content":` + string(b) + `}`
}

// connectWithInbox connects id with an inbox handler.
func (b *bus) connectWithInbox(t *testing.T, id string, h client.Handler) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: b.url, AgentID: id, Token: b.tokens[id], CleanSession: true, Logger: quiet, Inbox: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestLLMHandler(t *testing.T) {
	b := startBus(t)
	t.Setenv("TEST_MODEL_KEY", "s3cret")
	var nobodyHops sync.Map
	b.connectWithInbox(t, "nobody", func(_ context.Context, e *a2a.Envelope) (any, error) {
		nobodyHops.Store(e.ID, e.Headers[HopsHeader])
		return map[string]string{"weather": "rain"}, nil
	})

	m := &model{turns: []string{
		callTurn("echo", `{"text":"a b"}`),
		callTurn("env", `{}`),
		callTurn("bus_request", `{"to":"nobody","body":{"q":"weather?"}}`),
		callTurn("bus_request", `{"to":"alice","body":{}}`),
		textTurn("```json\n{\"done\": true}\n```"),
	}}
	url := m.start(t)
	startAgent(t, b, `
agent: probe
llm: {provider: openai, base_url: "`+url+`", model: fake-1, api_key_env: TEST_MODEL_KEY}
handlers:
  - inbox: true
    description: answers questions
    llm:
      system: Answer the question.
      tools: [echo, env]
      bus: {request: [nobody]}
      reply: json
  - name: echo
    go: test.echo
    description: echoes its input
    params: {type: object, properties: {text: {type: string}}}
  - name: env
    run: 'printf "%s" "${TEST_MODEL_KEY:-unset}"'
`, fstest.MapFS{})

	alice := b.connect(t, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := alice.Request(ctx, "probe", map[string]string{"question": "weather?"})
	if err != nil || string(r.Body) != `{"done":true}` {
		t.Fatalf("reply: %v %s", err, bodyOf(r))
	}

	first := m.request(0)
	for _, want := range []string{`"model":"fake-1"`, `untrusted`, `Answer the question.`, `\"from\":\"alice\"`,
		`"name":"echo"`, `"name":"env"`, `"name":"bus_request"`, `echoes its input`, `"enum":["nobody"]`} {
		if !strings.Contains(first, want) {
			t.Errorf("first model request lacks %s:\n%s", want, first)
		}
	}
	if strings.Contains(first, "bus_task") || strings.Contains(first, "bus_agents") {
		t.Errorf("tools not granted were offered:\n%s", first)
	}
	if got := m.request(1); !strings.Contains(got, `\"from\":\"alice\"`) || !strings.Contains(got, `\"text\":\"a b\"`) {
		t.Errorf("go tool should run as the sender with the input as body:\n%s", got)
	}
	if got := m.request(2); !strings.Contains(got, `"content":"unset"`) {
		t.Errorf("the model key leaked into a command's environment:\n%s", got)
	}
	if got := m.request(3); !strings.Contains(got, `{\"weather\":\"rain\"}`) || !strings.Contains(got, "untrusted") {
		t.Errorf("bus reply should reach the model behind the preamble:\n%s", got)
	}
	if got := m.request(4); !strings.Contains(got, "may not ask alice") {
		t.Errorf("request outside bus.request should be refused:\n%s", got)
	}
	hops := ""
	nobodyHops.Range(func(_, v any) bool { hops = v.(string); return false })
	if hops != "1" {
		t.Errorf("hops header = %q, want 1", hops)
	}
}

func TestLLMHopLimit(t *testing.T) {
	b := startBus(t)
	b.connectWithInbox(t, "nobody", func(context.Context, *a2a.Envelope) (any, error) {
		t.Error("request past the hop limit reached the bus")
		return nil, nil
	})
	m := &model{turns: []string{callTurn("bus_request", `{"to":"nobody","body":{}}`), textTurn("plain answer")}}
	url := m.start(t)
	startAgent(t, b, `
agent: probe
max_hops: 2
llm: {provider: openai, base_url: "`+url+`", model: fake-1}
handlers:
  - inbox: true
    llm: {bus: {request: ["*"]}}
`, fstest.MapFS{})

	alice := b.connect(t, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := alice.Request(ctx, "probe", "hi", client.WithHeaders(map[string]string{HopsHeader: "2"}))
	if err != nil || string(r.Body) != `"plain answer"` {
		t.Fatalf("reply: %v %s", err, bodyOf(r))
	}
	if got := m.request(1); !strings.Contains(got, "hop limit reached") {
		t.Errorf("model wasn't told about the hop limit:\n%s", got)
	}
}

func TestLLMBadJSONAndMissingKey(t *testing.T) {
	b := startBus(t)
	m := &model{turns: []string{textTurn("sorry, no JSON today")}}
	url := m.start(t)
	startAgent(t, b, `
agent: probe
handlers:
  - inbox: true
    llm: {provider: openai, base_url: "`+url+`", model: fake-1, reply: json}
`, fstest.MapFS{})
	alice := b.connect(t, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := alice.Request(ctx, "probe", "hi"); remoteCode(err) != "bad_model_output" {
		t.Errorf("non-JSON answer: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "")
	mf, err := ParseManifest([]byte("agent: probe\nhandlers:\n  - inbox: true\n    llm: {provider: anthropic, model: claude-haiku-5-5}\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Config{Manifest: mf, URL: b.url, AgentID: "probe", Token: b.tokens["probe"], Logger: quiet})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("missing key: %v", err)
	}
	if p := Check(mf, nil, true); len(p) != 1 || !strings.Contains(p[0], "no API key") {
		t.Errorf("Check: %v", p)
	}
}

func TestModelReply(t *testing.T) {
	for _, c := range []struct{ in, mode, want string }{
		{`{"a":1}`, "", `{"a":1}`},
		{"```json\n[1,2]\n```", "", `[1,2]`},
		{"hello", "", `"hello"`},
		{"```\nnot json\n```", "", "\"```\\nnot json\\n```\""},
		{`{"a":1}`, "text", `"{\"a\":1}"`},
		{"", "", `null`},
	} {
		v, err := modelReply(c.in, c.mode)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		got, _ := json.Marshal(v)
		if string(got) != c.want {
			t.Errorf("modelReply(%q, %q) = %s, want %s", c.in, c.mode, got, c.want)
		}
	}
}
