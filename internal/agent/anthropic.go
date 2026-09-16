package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// anthropic implements Provider against the Anthropic Messages API.
type anthropic struct {
	key, model, base string
	hc               *http.Client
}

func (a *anthropic) Name() string { return "anthropic" }

func (a *anthropic) client() *http.Client {
	if a.hc == nil {
		a.hc = &http.Client{Timeout: 120 * time.Second}
	}
	return a.hc
}

type anthBlock map[string]any

func (a *anthropic) Chat(ctx context.Context, r *Request) (*Response, error) {
	msgs := a.convMessages(r.Messages)
	body := map[string]any{
		"model":      r.Model,
		"max_tokens": r.MaxTok,
		"system":     r.System,
		"messages":   msgs,
	}
	if len(r.Tools) > 0 {
		var tools []map[string]any
		for _, t := range r.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Desc,
				"input_schema": t.Schema,
			})
		}
		body["tools"] = tools
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", a.base+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("anthropic: %s", out.Error.Message)
	}
	res := &Response{Done: out.StopReason == "end_turn"}
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			res.Text += b.Text
		case "tool_use":
			res.Calls = append(res.Calls, Call{ID: b.ID, Name: b.Name, Args: b.Input})
		}
	}
	return res, nil
}

func (a *anthropic) convMessages(in []Msg) []map[string]any {
	var out []map[string]any
	for _, m := range in {
		switch m.Role {
		case "user":
			out = append(out, map[string]any{"role": "user", "content": []anthBlock{{"type": "text", "text": m.Text}}})
		case "assistant":
			var blocks []anthBlock
			if m.Text != "" {
				blocks = append(blocks, anthBlock{"type": "text", "text": m.Text})
			}
			for _, cl := range m.Calls {
				var input any
				json.Unmarshal(cl.Args, &input)
				blocks = append(blocks, anthBlock{"type": "tool_use", "id": cl.ID, "name": cl.Name, "input": input})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			out = append(out, map[string]any{"role": "user", "content": []anthBlock{{
				"type": "tool_result", "tool_use_id": m.CallID,
				"content": m.Text, "is_error": m.IsError,
			}}})
		}
	}
	return out
}
