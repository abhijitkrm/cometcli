package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
)

func TestProviderGeminiDefaults(t *testing.T) {
	os.Unsetenv("COMETCLI_LLM_API_KEY")
	t.Setenv("GEMINI_API_KEY", "gem_test")
	p, err := NewProvider(config.AgentConf{Provider: "gemini"})
	if err != nil {
		t.Fatal(err)
	}
	g := p.(*gemini)
	if g.base != "https://generativelanguage.googleapis.com" {
		t.Fatalf("base = %q", g.base)
	}
	if g.model != "gemini-3.8-flash" || g.key != "gem_test" || g.Name() != "gemini" {
		t.Fatalf("model/key/name = %q/%q/%q", g.model, g.key, g.Name())
	}
	if _, ok := p.(Streamer); !ok {
		t.Fatal("gemini must implement Streamer")
	}
}

// serveGemini captures the request body and replies with payload.
func serveGemini(t *testing.T, payload string) (*gemini, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "gem_test" {
			t.Errorf("missing/wrong api key header")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, payload)
	}))
	t.Cleanup(srv.Close)
	return &gemini{key: "gem_test", model: "m", base: srv.URL}, &got
}

func TestGeminiChatRequestShape(t *testing.T) {
	g, got := serveGemini(t, `{"id":"v1_x","output_text":"all good","steps":[{"type":"model_output","content":[{"type":"text","text":"all good"}]}]}`)
	res, err := g.Chat(context.Background(), &Request{
		Model:  "m",
		System: "you are an SRE",
		Messages: []Msg{
			{Role: "user", Text: "check node"},
		},
		Tools:  []ToolDef{{Name: "node__status", Desc: "[observe] node status", Schema: map[string]any{"type": "object"}}},
		MaxTok: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "all good" || !res.Done {
		t.Fatalf("res = %+v", res)
	}
	if (*got)["store"] != false {
		t.Fatal("store must be false (stateless mode)")
	}
	if (*got)["system_instruction"] != "you are an SRE" {
		t.Fatalf("system_instruction = %v", (*got)["system_instruction"])
	}
	if (*got)["stream"] != nil {
		t.Fatal("Chat must not set stream")
	}
	tools := (*got)["tools"].([]any)
	fn := tools[0].(map[string]any)
	if fn["type"] != "function" || fn["name"] != "node__status" {
		t.Fatalf("tool decl = %v", fn)
	}
	input := (*got)["input"].([]any)
	u := input[0].(map[string]any)
	if u["type"] != "user_input" {
		t.Fatalf("input[0] = %v", u)
	}
}

func TestGeminiFunctionCallAndEchoBack(t *testing.T) {
	// Turn 1: model asks for node__status with a thought step.
	rawSteps := `[{"type":"thought","content":[{"type":"text","text":"need status"}],"signature":"sig"},{"type":"function_call","id":"fc_1","name":"node__status","arguments":{"verbose":true}}]`
	g, got := serveGemini(t, `{"id":"v1_a","steps":`+rawSteps+`}`)
	res, err := g.Chat(context.Background(), &Request{
		Model:    "m",
		Messages: []Msg{{Role: "user", Text: "status?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || len(res.Calls) != 1 {
		t.Fatalf("calls = %+v", res.Calls)
	}
	c := res.Calls[0]
	if c.ID != "fc_1" || c.Name != "node__status" || string(c.Args) != `{"verbose":true}` {
		t.Fatalf("call = %+v", c)
	}
	var steps []map[string]any
	if err := json.Unmarshal(res.RawSteps, &steps); err != nil || len(steps) != 2 {
		t.Fatalf("RawSteps = %s", res.RawSteps)
	}

	// Turn 2: history = user + assistant(raw steps) + tool result.
	history := []Msg{
		{Role: "user", Text: "status?"},
		{Role: "assistant", RawSteps: res.RawSteps},
		{Role: "tool", CallID: "fc_1", ToolName: "node.status", Text: "h=100 ok"},
	}
	g.Chat(context.Background(), &Request{Model: "m", Messages: history})
	input := (*got)["input"].([]any)
	if len(input) != 4 { // user + thought + function_call + function_result
		t.Fatalf("input len = %d: %v", len(input), input)
	}
	th := input[1].(map[string]any)
	if th["type"] != "thought" || th["signature"] != "sig" {
		t.Fatalf("thought step not echoed verbatim: %v", th)
	}
	fc := input[2].(map[string]any)
	if fc["type"] != "function_call" || fc["id"] != "fc_1" {
		t.Fatalf("function_call echo = %v", fc)
	}
	fr := input[3].(map[string]any)
	if fr["type"] != "function_result" || fr["name"] != "node__status" || fr["call_id"] != "fc_1" {
		t.Fatalf("function_result = %v", fr)
	}
}

func TestGeminiStreamAssemblesCalls(t *testing.T) {
	sse := strings.Join([]string{
		`event: interaction.created`,
		`data: {"interaction":{"id":"v1_s","status":"in_progress"},"event_type":"interaction.created"}`,
		``,
		`event: step.start`,
		`data: {"index":0,"step":{"type":"model_output"},"event_type":"step.start"}`,
		``,
		`event: step.delta`,
		`data: {"index":0,"delta":{"text":"checking ","type":"text"},"event_type":"step.delta"}`,
		``,
		`event: step.delta`,
		`data: {"index":0,"delta":{"text":"now","type":"text"},"event_type":"step.delta"}`,
		``,
		`event: step.stop`,
		`data: {"index":0,"event_type":"step.stop"}`,
		``,
		`event: step.start`,
		`data: {"index":1,"step":{"type":"function_call","id":"fc_9","name":"chain__balance"},"event_type":"step.start"}`,
		``,
		`event: step.delta`,
		`data: {"index":1,"delta":{"partial_arguments":"{\"addr\":","type":"arguments"},"event_type":"step.delta"}`,
		``,
		`event: step.delta`,
		`data: {"index":1,"delta":{"partial_arguments":"\"cosmos1x\"}","type":"arguments"},"event_type":"step.delta"}`,
		``,
		`event: interaction.completed`,
		`data: {"interaction":{"id":"v1_s","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"checking now"}]},{"type":"function_call","id":"fc_9","name":"chain__balance","arguments":{"addr":"cosmos1x"}}]},"event_type":"interaction.completed"}`,
		``,
		`event: done`,
		`data: [DONE]`,
		``,
	}, "\n")
	g, _ := serveGemini(t, sse)
	var deltas []string
	res, err := g.Stream(context.Background(), &Request{Model: "m"}, func(s string) {
		deltas = append(deltas, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "checking now" {
		t.Fatalf("deltas = %v", deltas)
	}
	if res.Done || len(res.Calls) != 1 {
		t.Fatalf("calls = %+v", res.Calls)
	}
	if res.Calls[0].ID != "fc_9" || res.Calls[0].Name != "chain__balance" ||
		string(res.Calls[0].Args) != `{"addr":"cosmos1x"}` {
		t.Fatalf("assembled call = %+v", res.Calls[0])
	}
	// completed.steps must be kept verbatim for echo-back
	var steps []map[string]any
	if err := json.Unmarshal(res.RawSteps, &steps); err != nil || len(steps) != 2 {
		t.Fatalf("RawSteps = %s", res.RawSteps)
	}
}

func TestGeminiStreamFallbackNoCompletedSteps(t *testing.T) {
	// stream ends without interaction.completed steps — assemble locally
	sse := strings.Join([]string{
		`event: step.start`,
		`data: {"index":0,"step":{"type":"function_call","id":"fc_1","name":"node__status","arguments":{"a":1}},"event_type":"step.start"}`,
		``,
		`event: done`,
		`data: [DONE]`,
		``,
	}, "\n")
	g, _ := serveGemini(t, sse)
	res, err := g.Stream(context.Background(), &Request{Model: "m"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 1 || res.Calls[0].ID != "fc_1" {
		t.Fatalf("calls = %+v", res.Calls)
	}
}

func TestGeminiAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"quota exceeded"}}`)
	}))
	defer srv.Close()
	g := &gemini{key: "k", model: "m", base: srv.URL}
	_, err := g.Chat(context.Background(), &Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v", err)
	}
}
