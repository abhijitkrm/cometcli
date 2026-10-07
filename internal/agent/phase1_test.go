package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// noSleep makes retry backoff instant for the duration of a test.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = old })
	return &waits
}

func TestRetryHonorsRetryAfterThenSucceeds(t *testing.T) {
	waits := noSleep(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(529) // Anthropic overloaded
		default:
			io.WriteString(w, "ok")
		}
	}))
	defer srv.Close()
	resp, err := doHTTP(context.Background(), srv.Client(), srv.URL, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || n.Load() != 3 {
		t.Fatalf("status %d after %d attempts", resp.StatusCode, n.Load())
	}
	if len(*waits) != 2 || (*waits)[0] != 7*time.Second {
		t.Fatalf("waits = %v, want Retry-After 7s first", *waits)
	}
}

func TestRetrySkipsClientErrors(t *testing.T) {
	noSleep(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	resp, err := doHTTP(context.Background(), srv.Client(), srv.URL, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 || n.Load() != 1 {
		t.Fatalf("400 retried: %d attempts", n.Load())
	}
}

func TestRetryGivesUpAfterMax(t *testing.T) {
	noSleep(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	resp, err := doHTTPWith(context.Background(), srv.Client(), srv.URL, []byte(`{}`), nil, retryPolicy{Max: 2, Base: time.Millisecond, Cap: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || n.Load() != 3 {
		t.Fatalf("want final 503 after 3 attempts, got %d after %d", resp.StatusCode, n.Load())
	}
}

// sseServer serves a fixed SSE body and records each request body.
func sseServer(t *testing.T, events string, bodies *[]map[string]any, hdrs *[]http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		*bodies = append(*bodies, b)
		if hdrs != nil {
			*hdrs = append(*hdrs, r.Header.Clone())
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, events)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sseBody(evs ...string) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func TestAnthropicStreamThinkingToolUseAndReplay(t *testing.T) {
	var bodies []map[string]any
	srv := sseServer(t, sseBody(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check the "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"node"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG123"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Looking."}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu1","name":"node__status","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"lines\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"5}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
	), &bodies, nil)
	p := &anthropic{key: "k", model: "claude-opus-5-5", base: srv.URL, hc: srv.Client()}
	var thought, text string
	req := &Request{Model: "claude-opus-5-5", System: "sys", Effort: "high",
		Messages:   []Msg{{Role: "user", Text: "hi"}},
		Tools:      []ToolDef{{Name: "node__status", Schema: map[string]any{"type": "object"}}},
		OnThinking: func(s string) { thought += s }}
	res, err := p.Stream(context.Background(), req, func(s string) { text += s })
	if err != nil {
		t.Fatal(err)
	}
	if thought != "check the node" || text != "Looking." || res.Thinking != "check the node" {
		t.Fatalf("thinking=%q text=%q", thought, text)
	}
	if res.Stop != StopToolUse || len(res.Calls) != 1 || string(res.Calls[0].Args) != `{"lines":5}` {
		t.Fatalf("stop=%s calls=%+v", res.Stop, res.Calls)
	}
	if res.Usage != (Usage{Input: 100, Output: 42, CacheRead: 90}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if !strings.Contains(string(res.RawSteps), `"signature":"SIG123"`) {
		t.Fatalf("signature lost from raw blocks: %s", res.RawSteps)
	}
	b := bodies[0]
	if b["cache_control"] == nil {
		t.Error("no top-level cache_control")
	}
	if th, _ := b["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking = %v", b["thinking"])
	}
	if oc, _ := b["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v", b["output_config"])
	}
	if tl, _ := b["tools"].([]any); len(tl) != 1 || tl[0].(map[string]any)["eager_input_streaming"] != true {
		t.Errorf("tools = %v", b["tools"])
	}
	if _, ok := b["fallbacks"]; ok {
		t.Error("fallbacks sent to a non-first-party base URL")
	}

	// the next request replays the assistant turn byte-for-byte, with both
	// tool results in one user message
	hist := []Msg{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: res.Text, Calls: res.Calls, RawSteps: res.RawSteps, RawProvider: "anthropic"},
		{Role: "tool", CallID: "tu1", Text: "height 5"},
		{Role: "tool", CallID: "tu2", Text: "peers 3"},
	}
	msgs := p.convMessages(hist)
	if len(msgs) != 3 {
		t.Fatalf("want user/assistant/user, got %d messages", len(msgs))
	}
	got, _ := json.Marshal(msgs[1]["content"])
	if !jsonEqual(t, got, res.RawSteps) {
		t.Fatalf("assistant not replayed verbatim:\n got %s\nwant %s", got, res.RawSteps)
	}
	if c, _ := msgs[2]["content"].([]anthBlock); len(c) != 2 {
		t.Fatalf("tool results not merged: %v", msgs[2]["content"])
	}
	// raw from another provider is never replayed to Anthropic
	hist[1].RawProvider = "gemini"
	got, _ = json.Marshal(p.convMessages(hist)[1]["content"])
	if strings.Contains(string(got), "SIG123") {
		t.Fatal("foreign raw steps replayed")
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

func TestAnthropicFallbackOnFirstParty(t *testing.T) {
	p := &anthropic{model: "claude-opus-5-5", base: "https://api.anthropic.com"}
	b := p.body(&Request{Model: "claude-opus-5-5"}, false)
	if b["fallbacks"] != "default" || b["max_tokens"] != 16000 {
		t.Fatalf("body = %v", b)
	}
	b = p.body(&Request{Model: "claude-haiku-4-5"}, true)
	if _, ok := b["fallbacks"]; ok {
		t.Fatal("fallbacks sent for a model without it")
	}
	if _, ok := b["thinking"]; ok {
		t.Fatal("adaptive thinking sent to haiku")
	}
	if b["max_tokens"] != 64000 {
		t.Fatalf("stream max_tokens = %v", b["max_tokens"])
	}
}

func TestAnthropicRefusalAndMaxTokens(t *testing.T) {
	r := anthResponse(nil, "refusal", &anthStopDetails{Category: "cyber"}, anthUsage{})
	if r.Stop != StopRefusal || r.StopDetail != "cyber" {
		t.Fatalf("refusal = %s %q", r.Stop, r.StopDetail)
	}
	if r := anthResponse(nil, "max_tokens", nil, anthUsage{}); r.Stop != StopMaxTokens {
		t.Fatalf("stop = %s", r.Stop)
	}
}

func TestAnthropicInvalidToolInputReportedNotReplayed(t *testing.T) {
	var bodies []map[string]any
	srv := sseServer(t, sseBody(
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu1","name":"x","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
	), &bodies, nil)
	p := &anthropic{key: "k", base: srv.URL, hc: srv.Client()}
	res, err := p.Stream(context.Background(), &Request{Model: "m"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Calls[0].Args) != `{"a":` {
		t.Fatalf("truncated input should reach the loop as-is, got %s", res.Calls[0].Args)
	}
	if !json.Valid(res.RawSteps) || !strings.Contains(string(res.RawSteps), `"input":{}`) {
		t.Fatalf("raw blocks must stay valid JSON: %s", res.RawSteps)
	}
}

func TestOpenAIGroqStreamUsageReasoningAndLength(t *testing.T) {
	var bodies []map[string]any
	srv := sseServer(t, sseBody(
		`{"choices":[{"delta":{"reasoning":"hmm "}}]}`,
		`{"choices":[{"delta":{"content":"partial"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}],"x_groq":{"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":100}}}}`,
		`[DONE]`,
	), &bodies, nil)
	o := &openai{name: "groq", model: "openai/gpt-oss-120b", base: srv.URL, hc: srv.Client()}
	var thought string
	res, err := o.Stream(context.Background(), &Request{Model: o.model, MaxTok: 2048, Effort: "max",
		Messages: []Msg{{Role: "user", Text: "x"}}, OnThinking: func(s string) { thought += s }}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != StopMaxTokens || res.Text != "partial" || thought != "hmm " {
		t.Fatalf("stop=%s text=%q thought=%q", res.Stop, res.Text, thought)
	}
	if res.Usage != (Usage{Input: 120, Output: 30, CacheRead: 100}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
	b := bodies[0]
	if b["reasoning_effort"] != "high" || b["max_completion_tokens"] != float64(2048) {
		t.Fatalf("body = %v", b)
	}
	if _, ok := b["stream_options"]; ok {
		t.Fatal("stream_options sent to groq")
	}
}

func TestOpenAICompatUsesMaxTokens(t *testing.T) {
	o := &openai{model: "qwen3:32b", base: "http://localhost:11434", name: "openai-compat"}
	b := o.body(&Request{Model: "qwen3:32b", MaxTok: 100})
	if b["max_tokens"] != 100 || b["max_completion_tokens"] != nil || b["reasoning_effort"] != nil {
		t.Fatalf("body = %v", b)
	}
}

func TestOpenAIErrorStatusSurfaced(t *testing.T) {
	noSleep(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()
	o := &openai{name: "groq", base: srv.URL, hc: srv.Client()}
	_, err := o.Chat(context.Background(), &Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "bad key") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestGeminiEffortUsageAndStatus(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		io.WriteString(w, `{"status":"incomplete","output_text":"cut","steps":[{"type":"thought","signature":"s","summary":[{"type":"text","text":"plan"}]},{"type":"model_output","content":[{"type":"text","text":"cut"}]}],
			"usage":{"total_input_tokens":50,"total_cached_tokens":20,"total_output_tokens":5,"total_thought_tokens":7}}`)
	}))
	defer srv.Close()
	g := &gemini{model: "gemini-x", base: srv.URL, hc: srv.Client()}
	res, err := g.Chat(context.Background(), &Request{Model: "gemini-x", Effort: "xhigh", MaxTok: 900})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != StopMaxTokens || res.Thinking != "plan" || res.Usage != (Usage{Input: 50, Output: 12, CacheRead: 20}) {
		t.Fatalf("res = %+v", res)
	}
	gc, _ := bodies[0]["generation_config"].(map[string]any)
	if gc["thinking_level"] != "high" || gc["max_output_tokens"] != float64(900) {
		t.Fatalf("generation_config = %v", gc)
	}
}

func TestSystemFrozenSnapshotRefreshAppended(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "a", Done: true}, {Text: "b", Done: true}}}
	a := newTestAgent(t, prov)
	n := 0
	a.SnapshotFn = func(*toolkit.Context) string { n++; return fmt.Sprintf("LIVE: height=%d", n) }
	a.SnapshotTTL = time.Nanosecond
	a.Run(context.Background(), "one")
	time.Sleep(time.Millisecond)
	a.Run(context.Background(), "two")
	if prov.reqs[0].System != prov.reqs[1].System {
		t.Fatal("system prompt changed between turns — breaks the cached prefix")
	}
	if !strings.Contains(prov.reqs[0].System, "height=1") {
		t.Fatalf("first snapshot missing from system: %q", prov.reqs[0].System)
	}
	last := prov.reqs[1].Messages[len(prov.reqs[1].Messages)-1].Text
	if !strings.Contains(last, "<refreshed-snapshot>") || !strings.Contains(last, "height=2") || !strings.HasSuffix(last, "two") {
		t.Fatalf("refresh not appended to the user turn: %q", last)
	}
	// earlier messages are untouched (append-only history)
	if prov.reqs[1].Messages[0].Text != prov.reqs[0].Messages[0].Text {
		t.Fatal("earlier turn edited")
	}
}

func TestMaxTokensAutoContinues(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Text: "first half ", Stop: StopMaxTokens},
		{Text: "second half", Stop: StopEnd, Done: true},
	}}
	a := newTestAgent(t, prov)
	var notices []string
	a.OnEvent = func(e Event) {
		if e.Kind == EvNotice {
			notices = append(notices, e.Text)
		}
	}
	out, err := a.Run(context.Background(), "write it")
	if err != nil {
		t.Fatal(err)
	}
	if out != "first half second half" || prov.calls != 2 || len(notices) != 1 {
		t.Fatalf("out=%q calls=%d notices=%v", out, prov.calls, notices)
	}
}

func TestRefusalSurfacesAsError(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Stop: StopRefusal, StopDetail: "cyber"}}}
	a := newTestAgent(t, prov)
	_, err := a.Run(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "cyber") {
		t.Fatalf("err = %v", err)
	}
}

func TestUsageAccumulates(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__status", Args: json.RawMessage(`{}`)}}, Usage: Usage{Input: 100, Output: 10}},
		{Text: "ok", Done: true, Usage: Usage{Input: 150, Output: 5, CacheRead: 100}},
	}}
	tool := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "h"}, nil
	}}
	a := newTestAgent(t, prov, tool)
	a.Run(context.Background(), "x")
	tot, last := a.Usage()
	if tot != (Usage{Input: 250, Output: 15, CacheRead: 100}) || last != 155 {
		t.Fatalf("total=%+v last=%d", tot, last)
	}
}

// compactProvider answers summarizer requests with a summary and records
// what each normal request carried.
type compactProvider struct {
	mockProvider
	overflowOnce bool
}

func (c *compactProvider) Chat(ctx context.Context, r *Request) (*Response, error) {
	last := r.Messages[len(r.Messages)-1].Text
	if strings.Contains(last, "Summarize this conversation") {
		return &Response{Text: "SUMMARY: disk was 95%", Done: true}, nil
	}
	if c.overflowOnce {
		c.overflowOnce = false
		return nil, errors.New("anthropic: prompt is too long: 1000001 tokens > 1000000 maximum (HTTP 400)")
	}
	return c.mockProvider.Chat(ctx, r)
}

func TestAutoCompactAtTurnBoundary(t *testing.T) {
	prov := &compactProvider{mockProvider: mockProvider{responses: []*Response{
		{Text: "one", Done: true, Usage: Usage{Input: 900, Output: 200}},
		{Text: "two", Done: true},
	}}}
	a := newTestAgent(t, prov)
	a.CompactAt = 1000
	a.Run(context.Background(), "check disk")
	a.Run(context.Background(), "and now?")
	req := prov.lastReq
	if len(req.Messages) != 1 {
		t.Fatalf("history not replaced by summary: %d messages", len(req.Messages))
	}
	m := req.Messages[0].Text
	if !strings.Contains(m, "SUMMARY: disk was 95%") || !strings.HasSuffix(m, "and now?") {
		t.Fatalf("summary not attached to next turn: %q", m)
	}
}

func TestContextOverflowCompactsAndRetries(t *testing.T) {
	prov := &compactProvider{overflowOnce: true, mockProvider: mockProvider{responses: []*Response{{Text: "recovered", Done: true}}}}
	a := newTestAgent(t, prov)
	a.history = []Msg{{Role: "user", Text: "old"}, {Role: "assistant", Text: "old reply"}}
	out, err := a.Run(context.Background(), "go")
	if err != nil || out != "recovered" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !strings.Contains(prov.lastReq.Messages[0].Text, "Continue the task in progress") {
		t.Fatalf("mid-turn summary missing: %q", prov.lastReq.Messages[0].Text)
	}
}

func TestToolSetChangeDropsBoundThinking(t *testing.T) {
	prov := &mockProvider{}
	ro := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) { return &toolkit.Result{}, nil }}
	rw := stubTool{name: "node.restart", tier: toolkit.TierLocalChange, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) { return &toolkit.Result{}, nil }}
	a := newTestAgent(t, prov, ro, rw)
	a.conf.Tools = "all"
	a.Run(context.Background(), "one")
	a.history[1].RawSteps, a.history[1].RawProvider = json.RawMessage(`[{"type":"thinking","signature":"s"}]`), "anthropic"
	a.Policy.Mode = ModeReadOnly // hides node.restart → tool set changes
	a.Run(context.Background(), "two")
	if a.history[1].RawSteps != nil {
		t.Fatal("thinking bound to the old tool set was replayed")
	}
}

func TestSessionSaveRestoreRoundTrip(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "hello", Done: true, Usage: Usage{Input: 10, Output: 2}}}}
	a := newTestAgent(t, prov)
	a.Persist = true
	if _, err := a.Run(context.Background(), "first question"); err != nil {
		t.Fatal(err)
	}
	list, err := ListSessions()
	if err != nil || len(list) != 1 || list[0].Title != "first question" || list[0].History != nil {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	sf, err := LoadSession(a.ID()[:12])
	if err != nil {
		t.Fatal(err)
	}
	b := newTestAgent(t, &mockProvider{})
	b.Restore(sf)
	if b.ID() != a.ID() || len(b.History()) != 2 || b.system() != a.system() {
		t.Fatalf("restore lost state: id=%s hist=%d", b.ID(), len(b.History()))
	}
	if tot, _ := b.Usage(); tot.Input != 10 {
		t.Fatalf("usage not restored: %+v", tot)
	}
	if _, err := LoadSession("../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestClipKeepsHeadTailAndUTF8(t *testing.T) {
	s := strings.Repeat("é", 5000) + "TAIL-ERROR"
	out := clip(s, 1000)
	if !utf8.ValidString(out) || !strings.HasSuffix(out, "TAIL-ERROR") || len(out) > 1100 {
		t.Fatalf("clip: valid=%v len=%d tail=%q", utf8.ValidString(out), len(out), out[len(out)-12:])
	}
	if clip("short", 1000) != "short" {
		t.Fatal("short input altered")
	}
}

func TestValidEffort(t *testing.T) {
	for _, e := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if ValidEffort(e) != nil {
			t.Fatalf("%q rejected", e)
		}
	}
	if ValidEffort("ultra") == nil {
		t.Fatal("bad effort accepted")
	}
}

func TestRetryIsAnnouncedAndTimeoutsAreNot(t *testing.T) {
	noSleep(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	var notes []string
	ctx := WithRetryNotice(context.Background(), func(attempt int, wait time.Duration, reason string) {
		notes = append(notes, fmt.Sprintf("%d %s", attempt, reason))
	})
	resp, err := doHTTP(ctx, srv.Client(), srv.URL, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(notes) != 1 || notes[0] != "1 HTTP 503" {
		t.Fatalf("notices = %v", notes)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	n.Store(0)
	hc := &http.Client{Timeout: 20 * time.Millisecond}
	if _, err := doHTTP(context.Background(), hc, slow.URL, []byte(`{}`), nil); err == nil {
		t.Fatal("want timeout error")
	}
	if n.Load() > 1 { // 0 when the timeout fires before the handler runs
		t.Fatalf("timed-out request retried: %d attempts", n.Load())
	}
}

func TestRetrySkipsDailyQuota(t *testing.T) {
	noSleep(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day"}}`))
	}))
	defer srv.Close()
	resp, err := doHTTP(context.Background(), srv.Client(), srv.URL, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if n.Load() != 1 {
		t.Fatalf("daily quota retried: %d attempts", n.Load())
	}
	if err := apiError("openrouter", resp); err == nil || !strings.Contains(err.Error(), "free-models-per-day") {
		t.Fatalf("body lost: %v", err)
	}
}
