package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fake serves scripted responses in order and records each request.
type fake struct {
	mu      sync.Mutex
	replies []string
	status  []int // optional per-call status; 0 means 200
	reqs    []map[string]any
	headers []http.Header
	paths   []string
}

func (f *fake) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("request isn't JSON: %s", raw)
		}
		n := len(f.reqs)
		f.reqs = append(f.reqs, m)
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.Path)
		if n < len(f.status) && f.status[n] != 0 {
			w.WriteHeader(f.status[n])
			io.WriteString(w, `{"error":"try again"}`)
			return
		}
		if n >= len(f.replies) {
			t.Errorf("unexpected call %d", n+1)
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, f.replies[n])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func echoTool(_ context.Context, name string, in json.RawMessage) (string, error) {
	if name == "fail" {
		return "", errors.New("it broke")
	}
	return "got " + string(in), nil
}

func TestAnthropicLoop(t *testing.T) {
	f := &fake{replies: []string{
		`{"content":[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}},{"type":"tool_use","id":"tu_2","name":"fail","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`,
		`{"content":[{"type":"text","text":"{\"answer\":42}"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":3}}`,
	}}
	srv := f.serve(t)
	l := &Loop{Client: &Client{Provider: Anthropic, Model: "claude-haiku-5-5", BaseURL: srv.URL, APIKey: "k"},
		System: "be brief", Tools: []Tool{{Name: "lookup", Description: "looks up"}, {Name: "fail"}}, Run: echoTool}
	out, err := l.Do(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != `{"answer":42}` || out.Turns != 2 || out.Usage.In != 30 || out.Usage.Out != 8 {
		t.Errorf("outcome = %+v", out)
	}
	if f.paths[0] != "/v1/messages" || f.headers[0].Get("x-api-key") != "k" || f.headers[0].Get("anthropic-version") == "" {
		t.Errorf("path %s headers %v", f.paths[0], f.headers[0])
	}
	first := f.reqs[0]
	if first["system"] != "be brief" || first["model"] != "claude-haiku-5-5" || first["max_tokens"] != float64(4096) {
		t.Errorf("first request = %v", first)
	}
	tools := first["tools"].([]any)
	if s := tools[1].(map[string]any)["input_schema"].(map[string]any); s["type"] != "object" {
		t.Errorf("default schema = %v", s)
	}
	msgs := f.reqs[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 3 || asst[1].(map[string]any)["id"] != "tu_1" {
		t.Errorf("assistant turn not echoed: %v", asst)
	}
	results := msgs[2].(map[string]any)["content"].([]any)
	r1, r2 := results[0].(map[string]any), results[1].(map[string]any)
	if r1["tool_use_id"] != "tu_1" || r1["content"] != `got {"q":"x"}` || r1["is_error"] != nil {
		t.Errorf("result 1 = %v", r1)
	}
	if r2["tool_use_id"] != "tu_2" || r2["content"] != "it broke" || r2["is_error"] != true {
		t.Errorf("result 2 = %v", r2)
	}
}

func TestOpenAILoop(t *testing.T) {
	f := &fake{replies: []string{
		`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"y\"}"}},{"type":"function","function":{"name":"lookup","arguments":"not json"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		`{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`,
	}}
	srv := f.serve(t)
	l := &Loop{Client: &Client{Provider: OpenAI, Model: "llama3", BaseURL: srv.URL + "/v1/"},
		System: "sys", Tools: []Tool{{Name: "lookup"}}, Run: echoTool, MaxTokens: 100}
	out, err := l.Do(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "done" || out.Turns != 2 {
		t.Errorf("outcome = %+v", out)
	}
	if f.paths[0] != "/v1/chat/completions" || f.headers[0].Get("Authorization") != "" {
		t.Errorf("path %s, auth %q", f.paths[0], f.headers[0].Get("Authorization"))
	}
	if f.reqs[0]["max_tokens"] != float64(100) || f.reqs[0]["max_completion_tokens"] != nil {
		t.Errorf("token limit fields = %v", f.reqs[0])
	}
	msgs := f.reqs[1]["messages"].([]any)
	// system, user, assistant, tool, tool
	if len(msgs) != 5 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
	calls := msgs[2].(map[string]any)["tool_calls"].([]any)
	if fn := calls[0].(map[string]any)["function"].(map[string]any); fn["arguments"] != `{"q":"y"}` {
		t.Errorf("call echoed as %v", fn)
	}
	t1, t2 := msgs[3].(map[string]any), msgs[4].(map[string]any)
	if t1["tool_call_id"] != "c1" || t1["content"] != `got {"q":"y"}` {
		t.Errorf("tool 1 = %v", t1)
	}
	if t2["tool_call_id"] != "call_1" || !strings.Contains(t2["content"].(string), "not valid JSON") {
		t.Errorf("tool 2 = %v", t2)
	}
}

func TestOpenAIOfficialUsesMaxCompletionTokens(t *testing.T) {
	r := openaiRequest("gpt", Request{MaxTokens: 50}, true)
	if r.MaxCompletionTokens != 50 || r.MaxTokens != 0 {
		t.Errorf("%+v", r)
	}
}

func TestRetries(t *testing.T) {
	f := &fake{status: []int{429, 503}, replies: []string{"", "", `{"choices":[{"message":{"content":"ok"}}]}`}}
	srv := f.serve(t)
	c := &Client{Provider: OpenAI, Model: "m", BaseURL: srv.URL}
	turn, err := c.Complete(context.Background(), Request{Messages: []Msg{{Role: "user", Text: "x"}}})
	if err != nil || turn.Text != "ok" || len(f.reqs) != 3 {
		t.Fatalf("turn %+v, err %v, calls %d", turn, err, len(f.reqs))
	}

	f = &fake{status: []int{400}}
	srv = f.serve(t)
	c.BaseURL = srv.URL
	_, err = c.Complete(context.Background(), Request{})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 400 || len(f.reqs) != 1 {
		t.Fatalf("400 should fail at once: %v, calls %d", err, len(f.reqs))
	}
}

func TestMaxTurns(t *testing.T) {
	call := `{"choices":[{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`
	f := &fake{replies: []string{call, call}}
	srv := f.serve(t)
	l := &Loop{Client: &Client{Provider: OpenAI, Model: "m", BaseURL: srv.URL}, Tools: []Tool{{Name: "lookup"}}, Run: echoTool, MaxTurns: 2}
	out, err := l.Do(context.Background(), "x")
	if !errors.Is(err, ErrMaxTurns) || out.Turns != 2 {
		t.Fatalf("err %v, turns %d", err, out.Turns)
	}
}

func TestUnknownProvider(t *testing.T) {
	_, err := (&Client{Provider: "bard"}).Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatal(err)
	}
}
