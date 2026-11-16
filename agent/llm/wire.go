package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---- Anthropic Messages API ----

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthMsg struct {
	Role    string      `json:"role"`
	Content []anthBlock `json:"content"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthReq struct {
	Model     string     `json:"model"`
	MaxTokens int        `json:"max_tokens"`
	System    string     `json:"system,omitempty"`
	Messages  []anthMsg  `json:"messages"`
	Tools     []anthTool `json:"tools,omitempty"`
}

func anthropicRequest(model string, r Request) anthReq {
	out := anthReq{Model: model, MaxTokens: r.MaxTokens, System: r.System}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, anthTool{Name: t.Name, Description: t.Description, InputSchema: schemaOf(t)})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			out.Messages = append(out.Messages, anthMsg{Role: "user", Content: []anthBlock{{Type: "text", Text: m.Text}}})
		case "assistant":
			var blocks []anthBlock
			if m.Text != "" {
				blocks = append(blocks, anthBlock{Type: "text", Text: m.Text})
			}
			for _, c := range m.Calls {
				blocks = append(blocks, anthBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: inputOrEmpty(c.Input)})
			}
			out.Messages = append(out.Messages, anthMsg{Role: "assistant", Content: blocks})
		case "tool":
			var blocks []anthBlock
			for _, res := range m.Results {
				blocks = append(blocks, anthBlock{Type: "tool_result", ToolUseID: res.CallID, Content: res.Content, IsError: res.IsError})
			}
			out.Messages = append(out.Messages, anthMsg{Role: "user", Content: blocks})
		}
	}
	return out
}

func anthropicTurn(raw []byte) (*Turn, error) {
	var resp struct {
		Content    []anthBlock `json:"content"`
		StopReason string      `json:"stop_reason"`
		Usage      struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("model API: unreadable response: %w", err)
	}
	t := &Turn{Stop: resp.StopReason, Usage: Usage{In: resp.Usage.In, Out: resp.Usage.Out}}
	var text []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			t.Calls = append(t.Calls, Call{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	t.Text = strings.Join(text, "")
	return t, nil
}

// ---- OpenAI-compatible chat completions ----

type oaiFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   *string         `json:"arguments,omitempty"`
}

type oaiCall struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Function oaiFunc `json:"function"`
}

type oaiMsg struct {
	Role       string    `json:"role"`
	Content    *string   `json:"content"`
	ToolCalls  []oaiCall `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string  `json:"type"`
	Function oaiFunc `json:"function"`
}

type oaiReq struct {
	Model               string    `json:"model"`
	Messages            []oaiMsg  `json:"messages"`
	Tools               []oaiTool `json:"tools,omitempty"`
	MaxTokens           int       `json:"max_tokens,omitempty"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"`
}

func openaiRequest(model string, r Request, official bool) oaiReq {
	out := oaiReq{Model: model}
	// OpenAI's own API wants max_completion_tokens; most compatible
	// servers only know max_tokens.
	if official {
		out.MaxCompletionTokens = r.MaxTokens
	} else {
		out.MaxTokens = r.MaxTokens
	}
	str := func(s string) *string { return &s }
	if r.System != "" {
		out.Messages = append(out.Messages, oaiMsg{Role: "system", Content: str(r.System)})
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, oaiTool{Type: "function", Function: oaiFunc{Name: t.Name, Description: t.Description, Parameters: schemaOf(t)}})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			out.Messages = append(out.Messages, oaiMsg{Role: "user", Content: str(m.Text)})
		case "assistant":
			msg := oaiMsg{Role: "assistant"}
			if m.Text != "" {
				msg.Content = str(m.Text)
			}
			for _, c := range m.Calls {
				msg.ToolCalls = append(msg.ToolCalls, oaiCall{ID: c.ID, Type: "function",
					Function: oaiFunc{Name: c.Name, Arguments: str(string(inputOrEmpty(c.Input)))}})
			}
			out.Messages = append(out.Messages, msg)
		case "tool":
			for _, res := range m.Results {
				content := res.Content
				if res.IsError {
					content = "error: " + content
				}
				out.Messages = append(out.Messages, oaiMsg{Role: "tool", ToolCallID: res.CallID, Content: str(content)})
			}
		}
	}
	return out
}

func openaiTurn(raw []byte) (*Turn, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   *string   `json:"content"`
				ToolCalls []oaiCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("model API: unreadable response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("model API: response has no choices")
	}
	ch := resp.Choices[0]
	t := &Turn{Stop: ch.FinishReason, Usage: Usage{In: resp.Usage.In, Out: resp.Usage.Out}}
	if ch.Message.Content != nil {
		t.Text = *ch.Message.Content
	}
	for i, c := range ch.Message.ToolCalls {
		id := c.ID
		if id == "" { // some local servers leave it out
			id = fmt.Sprintf("call_%d", i)
		}
		var args json.RawMessage
		if c.Function.Arguments != nil {
			args = json.RawMessage(*c.Function.Arguments) // Loop refuses it if it isn't JSON
		}
		t.Calls = append(t.Calls, Call{ID: id, Name: c.Function.Name, Input: args})
	}
	return t, nil
}
