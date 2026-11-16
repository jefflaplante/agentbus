// Package llm runs a model with tools until it gives a final answer. It
// speaks two wire formats with only the standard library: Anthropic's
// Messages API, and the OpenAI-compatible chat completions API that
// OpenAI, OpenRouter, Ollama, vLLM, llama.cpp and LM Studio all serve.
//
// The agent runtime uses it for `llm:` handlers (see docs/AGENTS.md), but
// it has no agentbus dependencies and can be used from a Go handler too.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Provider names.
const (
	Anthropic = "anthropic"
	OpenAI    = "openai" // any OpenAI-compatible chat completions server
)

// Default endpoints and key variables.
const (
	AnthropicURL = "https://api.anthropic.com"
	OpenAIURL    = "https://api.openai.com/v1"
	AnthropicKey = "ANTHROPIC_API_KEY"
	OpenAIKey    = "OPENAI_API_KEY"
)

// MaxResponse caps how much of one model response is read.
const MaxResponse = 8 << 20

// Tool is a function the model may call. Schema is a JSON Schema for the
// input object.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Call is one tool call the model made.
type Call struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Result answers one Call.
type Result struct {
	CallID  string
	Content string
	IsError bool
}

// Msg is one turn of the conversation, in a neutral shape each provider
// converts to its own: a user's text, an assistant's text and calls, or
// the results of those calls.
type Msg struct {
	Role    string // "user", "assistant" or "tool"
	Text    string
	Calls   []Call
	Results []Result
}

// Usage counts tokens.
type Usage struct {
	In, Out int
}

// Turn is one model response.
type Turn struct {
	Text  string
	Calls []Call
	Usage Usage
	Stop  string // the provider's stop reason, for logs
}

// Request is everything one model call needs.
type Request struct {
	System    string
	Messages  []Msg
	Tools     []Tool
	MaxTokens int
}

// Client calls one model at one endpoint.
type Client struct {
	Provider string // Anthropic or OpenAI
	Model    string
	BaseURL  string // default: the provider's public API
	APIKey   string // may be empty for a local OpenAI-compatible server
	HTTP     *http.Client
	// Retries is how many times a 429, 5xx or network error is retried,
	// with backoff, before giving up. Default 3; negative means none.
	Retries int
}

// Complete makes one model call.
func (c *Client) Complete(ctx context.Context, r Request) (*Turn, error) {
	var (
		url  string
		body any
		hdr  = http.Header{"Content-Type": {"application/json"}}
	)
	switch c.Provider {
	case Anthropic:
		url = strings.TrimRight(first(c.BaseURL, AnthropicURL), "/") + "/v1/messages"
		body = anthropicRequest(c.Model, r)
		hdr.Set("anthropic-version", "2023-06-01")
		if c.APIKey != "" {
			hdr.Set("x-api-key", c.APIKey)
		}
	case OpenAI:
		base := strings.TrimRight(first(c.BaseURL, OpenAIURL), "/")
		url = base + "/chat/completions"
		body = openaiRequest(c.Model, r, strings.Contains(base, "api.openai.com"))
		if c.APIKey != "" {
			hdr.Set("Authorization", "Bearer "+c.APIKey)
		}
	default:
		return nil, fmt.Errorf("unknown provider %q (want %s or %s)", c.Provider, Anthropic, OpenAI)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw, err := c.post(ctx, url, hdr, data)
	if err != nil {
		return nil, err
	}
	if c.Provider == Anthropic {
		return anthropicTurn(raw)
	}
	return openaiTurn(raw)
}

// StatusError is a non-2xx answer from the model API.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("model API: HTTP %d: %s", e.Status, e.Body)
}

func (c *Client) post(ctx context.Context, url string, hdr http.Header, data []byte) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	retries := c.Retries
	if retries == 0 {
		retries = 3
	}
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header = hdr.Clone()
		resp, err := hc.Do(req)
		var raw []byte
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
			resp.Body.Close()
			if err == nil && len(raw) > MaxResponse {
				return nil, errors.New("model API: response too large")
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		retryable := err != nil
		wait := backoff
		if err == nil {
			if resp.StatusCode/100 == 2 {
				return raw, nil
			}
			msg := strings.TrimSpace(string(raw))
			if len(msg) > 500 {
				msg = msg[:500] + "..."
			}
			err = &StatusError{Status: resp.StatusCode, Body: msg}
			retryable = resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode >= 500
			if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 && s <= 60 {
				wait = time.Duration(s) * time.Second
			}
		}
		if !retryable || retries < 0 || attempt >= retries {
			return nil, err
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff *= 2
	}
}

// ToolFunc runs one tool call and returns its result text. An error is
// shown to the model as a failed call, not returned from Run.
type ToolFunc func(ctx context.Context, name string, input json.RawMessage) (string, error)

// Loop is a tool-use conversation with limits.
type Loop struct {
	Client    *Client
	System    string
	Tools     []Tool
	Run       ToolFunc
	MaxTurns  int // model calls; default 8
	MaxTokens int // per model call; default 4096
}

// Outcome is how a Loop ended.
type Outcome struct {
	Text  string
	Turns int
	Usage Usage
}

// ErrMaxTurns is returned when the model is still calling tools after
// MaxTurns model calls.
var ErrMaxTurns = errors.New("model still calling tools after max_turns")

// Do runs the conversation from one user message to the model's final
// text. Tool calls in one turn run one after another, in order.
func (l *Loop) Do(ctx context.Context, user string) (*Outcome, error) {
	maxTurns, maxTokens := l.MaxTurns, l.MaxTokens
	if maxTurns <= 0 {
		maxTurns = 8
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	out := &Outcome{}
	msgs := []Msg{{Role: "user", Text: user}}
	for out.Turns < maxTurns {
		turn, err := l.Client.Complete(ctx, Request{System: l.System, Messages: msgs, Tools: l.Tools, MaxTokens: maxTokens})
		if err != nil {
			return out, err
		}
		out.Turns++
		out.Usage.In += turn.Usage.In
		out.Usage.Out += turn.Usage.Out
		if len(turn.Calls) == 0 {
			out.Text = turn.Text
			return out, nil
		}
		msgs = append(msgs, Msg{Role: "assistant", Text: turn.Text, Calls: turn.Calls})
		results := make([]Result, 0, len(turn.Calls))
		for _, call := range turn.Calls {
			res := Result{CallID: call.ID}
			switch {
			case l.Run == nil:
				res.Content, res.IsError = "no tools are available", true
			case len(bytes.TrimSpace(call.Input)) > 0 && !json.Valid(call.Input):
				res.Content, res.IsError = "tool input is not valid JSON", true
			default:
				if text, err := l.Run(ctx, call.Name, inputOrEmpty(call.Input)); err != nil {
					res.Content, res.IsError = err.Error(), true
				} else {
					res.Content = text
				}
			}
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			results = append(results, res)
		}
		msgs = append(msgs, Msg{Role: "tool", Results: results})
	}
	return out, ErrMaxTurns
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// emptyObject is the schema for a tool with no declared input.
var emptyObject = json.RawMessage(`{"type":"object","properties":{}}`)

func schemaOf(t Tool) json.RawMessage {
	if len(t.Schema) == 0 {
		return emptyObject
	}
	return t.Schema
}

func inputOrEmpty(in json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(in)) == 0 {
		return json.RawMessage(`{}`)
	}
	return in
}
