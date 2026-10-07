package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// anthropic implements Provider against the Anthropic Messages API.
type anthropic struct {
	key, model, base string
	hc               *http.Client
	noEffort         bool // the model rejected output_config.effort
}

func (a *anthropic) Name() string { return "anthropic" }

func (a *anthropic) client() *http.Client {
	if a.hc == nil {
		a.hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return a.hc
}

type anthBlock map[string]any

// Models that take adaptive thinking (4.6+ Opus/Sonnet, the 5 family,
// Fable/Mythos). Older ones (Haiku 4.5, Sonnet 4.5, …) reject it.
var anthAdaptiveRe = regexp.MustCompile(`^claude-(fable|mythos)-|^claude-(opus|sonnet)-(4-[6-9]|5)`)

// Models that take output_config.effort (Opus 4.5+, Sonnet 4.6+, the 5
// family, Fable/Mythos). Haiku 4.5 and Sonnet 4.5 reject it with a 400.
var anthEffortRe = regexp.MustCompile(`^claude-(fable|mythos)-|^claude-opus-(4-[5-9]|5)|^claude-sonnet-(4-[6-9]|5)|^claude-haiku-[5-9]`)

// Models that accept the server-side refusal fallback ("default" routing).
var anthFallbackRe = regexp.MustCompile(`^claude-(fable-5-1|opus-5|sonnet-5-5)`)

const anthFallbackBeta = "server-side-fallback-2026-07-01"

func (a *anthropic) firstParty() bool {
	return strings.Contains(a.base, "api.anthropic.com")
}

func (a *anthropic) body(r *Request, stream bool) map[string]any {
	maxTok := r.MaxTok
	if maxTok == 0 {
		// streaming has no HTTP-timeout concern, so give room for thinking
		maxTok = 16000
		if stream {
			maxTok = 64000
		}
	}
	body := map[string]any{
		"model":      r.Model,
		"max_tokens": maxTok,
		"messages":   a.convMessages(r.Messages),
		// auto-places a cache breakpoint on the last cacheable block, so
		// the frozen system prompt + tools + history prefix is reused
		"cache_control": map[string]any{"type": "ephemeral"},
	}
	if r.System != "" {
		body["system"] = []anthBlock{{"type": "text", "text": r.System}}
	}
	if len(r.Tools) > 0 {
		var tools []map[string]any
		for _, t := range r.Tools {
			td := map[string]any{
				"name":         t.Name,
				"description":  t.Desc,
				"input_schema": t.Schema,
			}
			if stream {
				td["eager_input_streaming"] = true
			}
			tools = append(tools, td)
		}
		body["tools"] = tools
	}
	if anthAdaptiveRe.MatchString(r.Model) {
		body["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
	}
	if r.Effort != "" && anthEffortRe.MatchString(r.Model) && !a.noEffort {
		body["output_config"] = map[string]any{"effort": r.Effort}
	}
	if a.firstParty() && anthFallbackRe.MatchString(r.Model) {
		body["fallbacks"] = "default"
	}
	return body
}

func (a *anthropic) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hdr := map[string]string{
		"x-api-key":         a.key,
		"anthropic-version": "2023-06-01",
	}
	if _, ok := body["fallbacks"]; ok {
		hdr["anthropic-beta"] = anthFallbackBeta
	}
	resp, err := doHTTP(ctx, a.client(), strings.TrimSuffix(a.base, "/")+"/v1/messages", raw, hdr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		err := apiError("anthropic", resp)
		// a model we didn't know lacks effort: drop it for the session
		if _, has := body["output_config"]; has && resp.StatusCode == 400 && strings.Contains(err.Error(), "effort") {
			a.noEffort = true
			delete(body, "output_config")
			return a.post(ctx, body)
		}
		return nil, err
	}
	return resp, nil
}

// anthUsage is the Messages API usage object; input_tokens excludes the
// cached parts, so total prompt size is the sum of all three.
type anthUsage struct {
	Input      int `json:"input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	Output     int `json:"output_tokens"`
}

func (u anthUsage) usage() Usage {
	return Usage{Input: u.Input + u.CacheWrite + u.CacheRead, Output: u.Output, CacheRead: u.CacheRead}
}

type anthStopDetails struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

func anthStop(reason string, d *anthStopDetails) (StopReason, string) {
	switch reason {
	case "end_turn", "stop_sequence":
		return StopEnd, ""
	case "tool_use":
		return StopToolUse, ""
	case "max_tokens", "model_context_window_exceeded":
		return StopMaxTokens, ""
	case "refusal":
		detail := ""
		if d != nil {
			detail = strings.TrimSpace(d.Category + " " + d.Explanation)
		}
		return StopRefusal, detail
	}
	return "", ""
}

// anthResponse builds a Response from the final content blocks. The raw
// blocks (thinking signatures included) are kept for verbatim replay.
func anthResponse(blocks []json.RawMessage, stop string, sd *anthStopDetails, u anthUsage) *Response {
	res := &Response{Usage: u.usage(), RawSteps: mustJSON(blocks)}
	res.Stop, res.StopDetail = anthStop(stop, sd)
	var text, thinking strings.Builder
	for _, raw := range blocks {
		var b struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			thinking.WriteString(b.Thinking)
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			res.Calls = append(res.Calls, Call{ID: b.ID, Name: b.Name, Args: args})
		}
	}
	res.Text, res.Thinking = text.String(), thinking.String()
	res.Done = len(res.Calls) == 0
	return res
}

func (a *anthropic) Chat(ctx context.Context, r *Request) (*Response, error) {
	resp, err := a.post(ctx, a.body(r, false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Content     []json.RawMessage `json:"content"`
		StopReason  string            `json:"stop_reason"`
		StopDetails *anthStopDetails  `json:"stop_details"`
		Usage       anthUsage         `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return anthResponse(out.Content, out.StopReason, out.StopDetails, out.Usage), nil
}

// Stream consumes the Messages API SSE stream. Each content block is
// rebuilt from its start event plus deltas — text, thinking (with its
// signature), redacted thinking, and tool_use input (partial JSON) — so
// the assistant turn can be replayed exactly as the model produced it.
func (a *anthropic) Stream(ctx context.Context, r *Request, onText func(string)) (*Response, error) {
	body := a.body(r, true)
	body["stream"] = true
	resp, err := a.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	type block struct {
		obj   map[string]any
		text  strings.Builder // text or thinking
		sig   strings.Builder
		input strings.Builder
	}
	blocks := map[int]*block{}
	var usage anthUsage
	var stop string
	var sd *anthStopDetails
	err = readSSE(resp.Body, func(data string) error {
		var ev struct {
			Type         string         `json:"type"`
			Index        int            `json:"index"`
			ContentBlock map[string]any `json:"content_block"`
			Message      struct {
				Usage anthUsage `json:"usage"`
			} `json:"message"`
			Delta struct {
				Type        string           `json:"type"`
				Text        string           `json:"text"`
				Thinking    string           `json:"thinking"`
				Signature   string           `json:"signature"`
				PartialJSON string           `json:"partial_json"`
				StopReason  string           `json:"stop_reason"`
				StopDetails *anthStopDetails `json:"stop_details"`
			} `json:"delta"`
			Usage *anthUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil // tolerate keepalives / unknown payloads
		}
		switch ev.Type {
		case "message_start":
			usage = ev.Message.Usage
		case "content_block_start":
			b := &block{obj: ev.ContentBlock}
			if t, _ := b.obj["text"].(string); t != "" {
				b.text.WriteString(t)
				onText(t)
			}
			blocks[ev.Index] = b
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				onText(ev.Delta.Text)
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
				if r.OnThinking != nil {
					r.OnThinking(ev.Delta.Thinking)
				}
			case "signature_delta":
				b.sig.WriteString(ev.Delta.Signature)
			case "input_json_delta":
				b.input.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			if ev.Delta.StopDetails != nil {
				sd = ev.Delta.StopDetails
			}
			if ev.Usage != nil {
				usage.Output = ev.Usage.Output
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
	idx := make([]int, 0, len(blocks))
	for i := range blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var out []json.RawMessage
	bad := map[string]string{} // tool_use id → unparseable input
	for _, i := range idx {
		b := blocks[i]
		switch b.obj["type"] {
		case "text":
			b.obj["text"] = b.text.String()
		case "thinking":
			b.obj["thinking"] = b.text.String()
			if b.sig.Len() > 0 {
				b.obj["signature"] = b.sig.String()
			}
		case "tool_use":
			// eager input streaming means the server no longer validates
			// the JSON (and max_tokens can cut it off): replay an empty
			// object, but hand the raw text to the loop so the call fails
			// as "bad args" and the model re-issues it
			in := strings.TrimSpace(b.input.String())
			if in == "" {
				in = "{}"
			}
			if json.Valid([]byte(in)) {
				b.obj["input"] = json.RawMessage(in)
			} else {
				b.obj["input"] = map[string]any{}
				id, _ := b.obj["id"].(string)
				bad[id] = in
			}
		}
		out = append(out, mustJSON(b.obj))
	}
	res := anthResponse(out, stop, sd, usage)
	for j, c := range res.Calls {
		if in, ok := bad[c.ID]; ok {
			res.Calls[j].Args = json.RawMessage(in)
		}
	}
	return res, nil
}

// convMessages maps neutral history to Messages API turns. Assistant
// turns produced by this provider replay their raw blocks verbatim
// (thinking blocks must come back unchanged); consecutive tool results
// are merged into one user message, as parallel tool use expects.
func (a *anthropic) convMessages(in []Msg) []map[string]any {
	var out []map[string]any
	var results []anthBlock
	flush := func() {
		if len(results) > 0 {
			out = append(out, map[string]any{"role": "user", "content": results})
			results = nil
		}
	}
	for _, m := range in {
		if m.Role != "tool" {
			flush()
		}
		switch m.Role {
		case "user":
			out = append(out, map[string]any{"role": "user", "content": []anthBlock{{"type": "text", "text": m.Text}}})
		case "assistant":
			if m.RawProvider == "anthropic" && len(m.RawSteps) > 0 {
				var blocks []json.RawMessage
				if json.Unmarshal(m.RawSteps, &blocks) == nil && len(blocks) > 0 {
					out = append(out, map[string]any{"role": "assistant", "content": blocks})
					continue
				}
			}
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
			if len(blocks) == 0 {
				blocks = append(blocks, anthBlock{"type": "text", "text": "(no output)"})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			results = append(results, anthBlock{
				"type": "tool_result", "tool_use_id": m.CallID,
				"content": m.Text, "is_error": m.IsError,
			})
		}
	}
	flush()
	return out
}
