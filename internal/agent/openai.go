package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// openai implements Provider against OpenAI chat completions and any
// OpenAI-compatible endpoint (Groq, Ollama, vLLM, llama.cpp server).
type openai struct {
	key, model, base, name string
	hc                     *http.Client
}

func (o *openai) Name() string {
	if o.name != "" {
		return o.name
	}
	return "openai"
}

func (o *openai) client() *http.Client {
	if o.hc == nil {
		o.hc = &http.Client{Timeout: 300 * time.Second}
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

func (o *openai) body(r *Request) map[string]any {
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
				tc := oaiToolCall{ID: cl.ID, Type: "function"}
				tc.Function.Name, tc.Function.Arguments = cl.Name, string(cl.Args)
				tcs = append(tcs, tc)
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
	if r.MaxTok > 0 {
		if o.hosted() {
			body["max_completion_tokens"] = r.MaxTok
		} else {
			body["max_tokens"] = r.MaxTok // llama.cpp / older vLLM / Ollama
		}
	}
	if r.Effort != "" {
		body["reasoning_effort"] = oaiEffort(r.Effort)
	}
	return body
}

// hosted reports whether this is OpenAI or Groq proper (vs a local
// OpenAI-compatible server that may lack newer request fields).
func (o *openai) hosted() bool {
	n := o.Name()
	return n == "openai" || n == "groq"
}

// oaiEffort maps the neutral effort scale onto reasoning_effort, which
// tops out at "high" on OpenAI and Groq reasoning models.
func oaiEffort(e string) string {
	switch e {
	case "xhigh", "max":
		return "high"
	}
	return e
}

func oaiStop(finish string) StopReason {
	switch finish {
	case "stop":
		return StopEnd
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	case "content_filter":
		return StopRefusal
	}
	return ""
}

// oaiUsage is the chat-completions usage object.
type oaiUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Details    struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *oaiUsage) usage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{Input: u.Prompt, Output: u.Completion, CacheRead: u.Details.Cached}
}

func (o *openai) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hdr := map[string]string{}
	if o.key != "" {
		hdr["authorization"] = "Bearer " + o.key
	}
	resp, err := doHTTP(ctx, o.client(), strings.TrimSuffix(o.base, "/")+"/v1/chat/completions", raw, hdr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, apiError(o.Name(), resp)
	}
	return resp, nil
}

func (o *openai) Chat(ctx context.Context, r *Request) (*Response, error) {
	resp, err := o.post(ctx, o.body(r))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				Reasoning string        `json:"reasoning"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaiUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", o.Name(), out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s: no choices", o.Name())
	}
	ch := out.Choices[0].Message
	res := &Response{
		Text: ch.Content, Thinking: ch.Reasoning, Done: len(ch.ToolCalls) == 0,
		Stop: oaiStop(out.Choices[0].FinishReason), Usage: out.Usage.usage(),
	}
	for _, tc := range ch.ToolCalls {
		res.Calls = append(res.Calls, Call{
			ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments),
		})
	}
	return res, nil
}

// Stream consumes chat-completions SSE chunks. Text deltas go to onText;
// tool calls arrive as fragments keyed by index (id/name first, then
// argument pieces) and are assembled here. Servers that send a whole tool
// call in one chunk (Ollama) work the same way.
func (o *openai) Stream(ctx context.Context, r *Request, onText func(string)) (*Response, error) {
	body := o.body(r)
	body["stream"] = true
	if o.Name() == "openai" {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	resp, err := o.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	type partial struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*partial{}
	var text, reasoning strings.Builder
	var finish string
	var usage *oaiUsage
	err = readSSE(resp.Body, func(data string) error {
		if data == "[DONE]" {
			return nil
		}
		var ch struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *oaiUsage `json:"usage"`
			XGroq *struct {
				Usage *oaiUsage `json:"usage"`
			} `json:"x_groq"` // Groq reports stream usage here
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ch) != nil {
			return nil
		}
		if ch.Error != nil {
			return fmt.Errorf("%s: %s", o.Name(), ch.Error.Message)
		}
		if ch.Usage != nil {
			usage = ch.Usage
		} else if ch.XGroq != nil && ch.XGroq.Usage != nil {
			usage = ch.XGroq.Usage
		}
		if len(ch.Choices) == 0 {
			return nil
		}
		if f := ch.Choices[0].FinishReason; f != "" {
			finish = f
		}
		d := ch.Choices[0].Delta
		if d.Reasoning != "" {
			reasoning.WriteString(d.Reasoning)
			if r.OnThinking != nil {
				r.OnThinking(d.Reasoning)
			}
		}
		if d.Content != "" {
			text.WriteString(d.Content)
			onText(d.Content)
		}
		for _, tc := range d.ToolCalls {
			p := calls[tc.Index]
			if p == nil {
				p = &partial{}
				calls[tc.Index] = p
			}
			if tc.ID != "" {
				p.id = tc.ID
			}
			if tc.Function.Name != "" {
				p.name = tc.Function.Name
			}
			p.args.WriteString(tc.Function.Arguments)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &Response{Text: text.String(), Thinking: reasoning.String(), Stop: oaiStop(finish), Usage: usage.usage()}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		p := calls[i]
		args := p.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		res.Calls = append(res.Calls, Call{ID: id, Name: p.name, Args: json.RawMessage(args)})
	}
	res.Done = len(res.Calls) == 0
	return res, nil
}
