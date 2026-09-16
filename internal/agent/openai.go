package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// openai implements Provider against OpenAI chat completions and any
// OpenAI-compatible endpoint (Ollama, vLLM, llama.cpp server).
type openai struct {
	key, model, base string
	hc               *http.Client
}

func (o *openai) Name() string { return "openai" }

func (o *openai) client() *http.Client {
	if o.hc == nil {
		o.hc = &http.Client{Timeout: 120 * time.Second}
	}
	return o.hc
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (o *openai) Chat(ctx context.Context, r *Request) (*Response, error) {
	var msgs []map[string]any
	if r.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": r.System})
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			msgs = append(msgs, map[string]any{"role": "user", "content": m.Text})
		case "assistant":
			mm := map[string]any{"role": "assistant"}
			if m.Text != "" {
				mm["content"] = m.Text
			}
			var tcs []oaiToolCall
			for _, cl := range m.Calls {
				tcs = append(tcs, oaiToolCall{ID: cl.ID, Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: cl.Name, Arguments: string(cl.Args)}})
			}
			if len(tcs) > 0 {
				mm["tool_calls"] = tcs
			}
			msgs = append(msgs, mm)
		case "tool":
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": m.CallID,
				"content": m.Text,
			})
		}
	}
	var tools []map[string]any
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Desc,
				"parameters":  t.Schema,
			},
		})
	}
	body := map[string]any{"model": r.Model, "messages": msgs}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	raw, _ := json.Marshal(body)
	url := strings.TrimSuffix(o.base, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if o.key != "" {
		req.Header.Set("authorization", "Bearer "+o.key)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("openai: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openai: no choices")
	}
	ch := out.Choices[0].Message
	res := &Response{Text: ch.Content, Done: len(ch.ToolCalls) == 0}
	for _, tc := range ch.ToolCalls {
		res.Calls = append(res.Calls, Call{
			ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments),
		})
	}
	return res, nil
}
