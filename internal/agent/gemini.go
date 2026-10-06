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

// gemini implements Provider against the Gemini Interactions API
// (https://ai.google.dev/gemini-api/docs/interactions). It runs stateless
// (store=false): model steps are echoed back verbatim in each request via
// Msg.RawSteps, so nothing is retained server-side.
type gemini struct {
	key, model, base string
	hc               *http.Client
}

func (g *gemini) Name() string { return "gemini" }

func (g *gemini) client() *http.Client {
	if g.hc == nil {
		g.hc = &http.Client{Timeout: 300 * time.Second}
	}
	return g.hc
}

// gemStep is the union of the Interactions step shapes we use.
type gemStep struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// gemInput converts the neutral message history to Interactions steps.
// Assistant turns replay their raw steps when available (required for
// thought signatures), else synthesize model_output/function_call steps.
func gemInput(msgs []Msg) []json.RawMessage {
	var out []json.RawMessage
	put := func(v any) {
		b, err := json.Marshal(v)
		if err == nil {
			out = append(out, b)
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			put(map[string]any{"type": "user_input", "content": []map[string]any{
				{"type": "text", "text": m.Text},
			}})
		case "assistant":
			if len(m.RawSteps) > 0 {
				var steps []json.RawMessage
				if json.Unmarshal(m.RawSteps, &steps) == nil {
					out = append(out, steps...)
					continue
				}
			}
			if m.Text != "" {
				put(map[string]any{"type": "model_output", "content": []map[string]any{
					{"type": "text", "text": m.Text},
				}})
			}
			for _, c := range m.Calls {
				var args any
				if len(c.Args) > 0 {
					_ = json.Unmarshal(c.Args, &args)
				}
				if args == nil {
					args = map[string]any{}
				}
				put(map[string]any{"type": "function_call", "id": c.ID, "name": c.Name, "arguments": args})
			}
		case "tool":
			put(map[string]any{
				"type": "function_result", "name": toolFnName(m.ToolName),
				"call_id": m.CallID, "result": []map[string]any{{"type": "text", "text": m.Text}},
			})
		}
	}
	return out
}

func (g *gemini) body(r *Request, stream bool) map[string]any {
	var tools []map[string]any
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{
			"type": "function", "name": t.Name,
			"description": t.Desc, "parameters": t.Schema,
		})
	}
	b := map[string]any{
		"model": r.Model, "store": false,
		"input": gemInput(r.Messages),
	}
	if r.System != "" {
		b["system_instruction"] = r.System
	}
	if len(tools) > 0 {
		b["tools"] = tools
	}
	if r.MaxTok > 0 {
		b["generation_config"] = map[string]any{"max_output_tokens": r.MaxTok}
	}
	if stream {
		b["stream"] = true
	}
	return b
}

func (g *gemini) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	url := strings.TrimSuffix(g.base, "/") + "/v1beta/interactions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if g.key != "" {
		req.Header.Set("x-goog-api-key", g.key)
	}
	return g.client().Do(req)
}

// gemStepText extracts assistant text from a parsed step.
func gemStepText(s *gemStep) string {
	if s.Type != "model_output" && s.Type != "text" || len(s.Content) == 0 {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(s.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// gemResponse builds a Response from the interaction's steps.
func gemResponse(steps []json.RawMessage, outputText string) *Response {
	res := &Response{Text: outputText}
	raw, _ := json.Marshal(steps)
	res.RawSteps = raw
	for _, s := range steps {
		var st gemStep
		if json.Unmarshal(s, &st) != nil {
			continue
		}
		switch st.Type {
		case "function_call":
			args := st.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			} else if args[0] == '"' { // arguments may arrive as a JSON string
				var s2 string
				if json.Unmarshal(args, &s2) == nil {
					args = json.RawMessage(s2)
				}
			}
			res.Calls = append(res.Calls, Call{ID: st.ID, Name: st.Name, Args: args})
		default:
			if res.Text == "" {
				if t := gemStepText(&st); t != "" {
					res.Text = t
				}
			}
		}
	}
	res.Done = len(res.Calls) == 0
	return res
}

func (g *gemini) Chat(ctx context.Context, r *Request) (*Response, error) {
	resp, err := g.post(ctx, g.body(r, false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, apiError(g.Name(), resp)
	}
	var out struct {
		OutputText string            `json:"output_text"`
		Steps      []json.RawMessage `json:"steps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Steps) == 0 && out.OutputText == "" {
		return nil, fmt.Errorf("%s: empty interaction", g.Name())
	}
	return gemResponse(out.Steps, out.OutputText), nil
}

// gemEvent is one SSE data payload from the Interactions stream.
type gemEvent struct {
	EventType string          `json:"event_type"`
	Index     int             `json:"index"`
	Step      json.RawMessage `json:"step"`
	Delta     struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		PartialArguments string `json:"partial_arguments"`
	} `json:"delta"`
	Interaction struct {
		Steps      []json.RawMessage `json:"steps"`
		OutputText string            `json:"output_text"`
	} `json:"interaction"`
}

// gemStreamState accumulates a streamed interaction's steps.
type gemStreamState struct {
	steps  map[int]*json.RawMessage // index → assembled step
	order  []int
	argBuf map[int]*strings.Builder
}

func newGemStream() *gemStreamState {
	return &gemStreamState{steps: map[int]*json.RawMessage{}, argBuf: map[int]*strings.Builder{}}
}

func (s *gemStreamState) handle(ev *gemEvent, onText func(string)) (completed *Response, _ error) {
	switch ev.EventType {
	case "step.start":
		var st gemStep
		if json.Unmarshal(ev.Step, &st) != nil {
			return nil, nil
		}
		if st.Type == "function_call" {
			raw := append(json.RawMessage(nil), ev.Step...)
			s.steps[ev.Index] = &raw
			s.order = append(s.order, ev.Index)
			if len(st.Arguments) > 0 {
				b := &strings.Builder{}
				if st.Arguments[0] == '"' {
					var str string
					if json.Unmarshal(st.Arguments, &str) == nil {
						b.WriteString(str)
					}
				} else {
					b.Write(st.Arguments)
				}
				s.argBuf[ev.Index] = b
			}
		}
	case "step.delta":
		switch ev.Delta.Type {
		case "text":
			onText(ev.Delta.Text)
		case "arguments":
			if b := s.argBuf[ev.Index]; b != nil {
				b.WriteString(ev.Delta.PartialArguments)
			}
		}
	case "interaction.completed", "interaction.complete":
		if len(ev.Interaction.Steps) > 0 {
			return &Response{RawSteps: mustJSON(ev.Interaction.Steps), Text: ev.Interaction.OutputText}, nil
		}
	}
	return nil, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// assemble rebuilds function_call steps from buffered argument fragments.
func (s *gemStreamState) assemble() []json.RawMessage {
	sort.Ints(s.order)
	var steps []json.RawMessage
	for _, i := range s.order {
		raw := *s.steps[i]
		var st gemStep
		if json.Unmarshal(raw, &st) != nil {
			continue
		}
		if b := s.argBuf[i]; b != nil && b.Len() > 0 {
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				obj["arguments"] = json.RawMessage(b.String())
				if re, err := json.Marshal(obj); err == nil {
					raw = re
				}
			}
		}
		steps = append(steps, raw)
	}
	return steps
}

// Stream consumes the Interactions SSE stream: text deltas go to onText,
// function_call steps are reassembled from argument fragments.
func (g *gemini) Stream(ctx context.Context, r *Request, onText func(string)) (*Response, error) {
	resp, err := g.post(ctx, g.body(r, true))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, apiError(g.Name(), resp)
	}
	st := newGemStream()
	var text strings.Builder
	var completed *Response
	err = readSSE(resp.Body, func(data string) error {
		if data == "[DONE]" {
			return nil
		}
		var ev gemEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			return nil
		}
		res, err := st.handle(&ev, func(t string) {
			text.WriteString(t)
			onText(t)
		})
		if err != nil {
			return err
		}
		if res != nil {
			completed = res
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed != nil && len(completed.RawSteps) > 0 {
		res := gemResponse(mustUnmarshalSteps(completed.RawSteps), firstNonEmpty(completed.Text, text.String()))
		return res, nil
	}
	steps := st.assemble()
	res := gemResponse(steps, text.String())
	return res, nil
}

func mustUnmarshalSteps(raw json.RawMessage) []json.RawMessage {
	var s []json.RawMessage
	_ = json.Unmarshal(raw, &s)
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
