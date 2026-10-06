package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
		a.hc = &http.Client{Timeout: 300 * time.Second}
	}
	return a.hc
}

type anthBlock map[string]any

func (a *anthropic) body(r *Request) map[string]any {
	body := map[string]any{
		"model":      r.Model,
		"max_tokens": r.MaxTok,
		"system":     r.System,
		"messages":   a.convMessages(r.Messages),
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
	return body
}

func (a *anthropic) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(a.base, "/")+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	return a.client().Do(req)
}

func (a *anthropic) Chat(ctx context.Context, r *Request) (*Response, error) {
	resp, err := a.post(ctx, a.body(r))
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

// Stream consumes the Messages API SSE stream: text_delta chunks are
// forwarded to onText as they arrive; tool_use input arrives as
// input_json_delta fragments and is assembled per content-block index.
func (a *anthropic) Stream(ctx context.Context, r *Request, onText func(string)) (*Response, error) {
	body := a.body(r)
	body["stream"] = true
	resp, err := a.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, apiError("anthropic", resp)
	}
	type block struct {
		typ, id, name string
		input         strings.Builder
	}
	blocks := map[int]*block{}
	var text strings.Builder
	stop := ""
	err = readSSE(resp.Body, func(data string) error {
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil // tolerate keepalives / unknown payloads
		}
		switch ev.Type {
		case "content_block_start":
			blocks[ev.Index] = &block{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			if ev.ContentBlock.Text != "" {
				text.WriteString(ev.ContentBlock.Text)
				onText(ev.ContentBlock.Text)
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				onText(ev.Delta.Text)
			case "input_json_delta":
				if b := blocks[ev.Index]; b != nil {
					b.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
		case "error":
			if ev.Error != nil {
				return fmt.Errorf("anthropic: %s", ev.Error.Message)
			}
			return fmt.Errorf("anthropic: stream error")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &Response{Text: text.String(), Done: stop == "end_turn"}
	idx := make([]int, 0, len(blocks))
	for i := range blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		b := blocks[i]
		if b.typ != "tool_use" {
			continue
		}
		args := b.input.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		res.Calls = append(res.Calls, Call{ID: b.id, Name: b.name, Args: json.RawMessage(args)})
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
				if err := json.Unmarshal(cl.Args, &input); err != nil {
					input = map[string]any{"_raw": string(cl.Args)}
				}
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
