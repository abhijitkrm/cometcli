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

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/redact"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// streamProvider wraps mockProvider with a Streamer that emits each
// response's text in two chunks.
type streamProvider struct{ *mockProvider }

func (s streamProvider) Stream(ctx context.Context, r *Request, onText func(string)) (*Response, error) {
	resp, err := s.Chat(ctx, r)
	if err != nil {
		return nil, err
	}
	if n := len(resp.Text); n > 0 {
		onText(resp.Text[:n/2])
		onText(resp.Text[n/2:])
	}
	return resp, nil
}

func TestUserInputRedactedBeforeLLM(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "ok", Done: true}}}
	a := newTestAgent(t, prov)
	a.Redact = redact.NewRedactor("val.internal")
	// (a mnemonic refuses the whole message: TestKeyMaterialNeverReachesTheProvider)
	if _, err := a.Run(context.Background(), "host val.internal is down, token=abcd1234secret"); err != nil {
		t.Fatal(err)
	}
	sent := prov.lastReq.Messages[0].Text
	if strings.Contains(sent, "abcd1234secret") || strings.Contains(sent, "val.internal") {
		t.Fatalf("secret reached the provider: %q", sent)
	}
	if !strings.Contains(sent, "[REDACTED_HOST]") {
		t.Fatalf("missing redaction markers: %q", sent)
	}
}

func TestSnapshotRedactedAndCached(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "a", Done: true}, {Text: "b", Done: true}}}
	a := newTestAgent(t, prov)
	n := 0
	a.SnapshotFn = func(*toolkit.Context) string {
		n++
		return "LIVE: peer 10.9.9.9 token=abcd1234secret"
	}
	a.Redact = redact.NewRedactor("10.9.9.9")
	a.Run(context.Background(), "one")
	a.Run(context.Background(), "two")
	if n != 1 {
		t.Fatalf("snapshot probed %d times, want 1 (TTL cache)", n)
	}
	sys := prov.lastReq.System
	if strings.Contains(sys, "10.9.9.9") || strings.Contains(sys, "abcd1234secret") {
		t.Fatalf("snapshot not redacted: %q", sys)
	}
}

func TestStreamingEmitsDeltasThenText(t *testing.T) {
	prov := streamProvider{&mockProvider{responses: []*Response{{Text: "node is healthy", Done: true}}}}
	a := newTestAgent(t, prov)
	a.Stream = true
	var kinds []EventKind
	var deltas string
	a.OnEvent = func(e Event) {
		kinds = append(kinds, e.Kind)
		if e.Kind == EvDelta {
			deltas += e.Text
		}
	}
	out, err := a.Run(context.Background(), "status?")
	if err != nil {
		t.Fatal(err)
	}
	if deltas != "node is healthy" || out != "node is healthy" {
		t.Fatalf("deltas=%q out=%q", deltas, out)
	}
	if len(kinds) != 3 || kinds[0] != EvDelta || kinds[2] != EvText {
		t.Fatalf("event order = %v", kinds)
	}
}

func TestEventsForToolCalls(t *testing.T) {
	tool := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "height 42\nmore"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__status", Args: json.RawMessage(`{"key":"` + strings.Repeat("ab", 32) + `"}`)}}},
		{Text: "done", Done: true},
	}}
	a := newTestAgent(t, prov, tool)
	var evs []Event
	a.OnEvent = func(e Event) { evs = append(evs, e) }
	a.Run(context.Background(), "go")
	if len(evs) != 3 || evs[0].Kind != EvToolStart || evs[1].Kind != EvToolResult || evs[2].Kind != EvText {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Args["key"] != "[REDACTED_HEX]" {
		t.Fatalf("tool args shown unredacted: %v", evs[0].Args)
	}
	if evs[1].Text != "height 42" {
		t.Fatalf("result summary = %q", evs[1].Text)
	}
}

func TestPolicy(t *testing.T) {
	var p Policy
	if p.Decision(toolkit.TierObserve) != "auto" || p.Decision(toolkit.TierLocalChange) != "confirm" || p.Decision(toolkit.TierOnChain) != "confirm" {
		t.Fatalf("default policy wrong: %s", p)
	}
	if err := p.SetAutopilot("on-chain", true); err == nil {
		t.Fatal("on-chain autopilot must be refused")
	}
	if err := p.SetAutopilot("local-change", true); err != nil {
		t.Fatal(err)
	}
	if p.Decision(toolkit.TierLocalChange) != "auto" || p.Decision(toolkit.TierOnChain) != "confirm" {
		t.Fatalf("autopilot policy wrong: %s", p)
	}
	if p.AutoApproveBelow() > toolkit.TierOnChain {
		t.Fatal("on-chain must never be auto-approved")
	}
	p.Mode = ModeReadOnly
	if p.Decision(toolkit.TierLocalChange) != "deny" || p.Decision(toolkit.TierDiagnose) != "auto" {
		t.Fatalf("readonly policy wrong: %s", p)
	}
	if _, err := PolicyFrom(config.AgentConf{Autopilot: []string{"on-chain"}}); err == nil {
		t.Fatal("profile autopilot on-chain must be rejected")
	}
}

func TestReadOnlyHidesAndBlocks(t *testing.T) {
	ran := false
	restart := stubTool{name: "node.service", tier: toolkit.TierLocalChange, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		ran = true
		return &toolkit.Result{Text: "restarted"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__service", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a := newTestAgent(t, prov, restart)
	a.Policy.Mode = ModeReadOnly
	a.Run(context.Background(), "restart")
	if ran {
		t.Fatal("local-change tool ran in readonly mode")
	}
	for _, td := range prov.reqs[0].Tools {
		if td.Name != todoToolName && td.Name != toolSearchName && td.Name != taskToolName {
			t.Fatalf("readonly advertised mutating tool %s", td.Name)
		}
	}
}

func TestAutopilotSkipsLocalChangePromptButNotOnChain(t *testing.T) {
	var asked []toolkit.Tier
	mk := func(name string, tier toolkit.Tier) stubTool {
		return stubTool{name: name, tier: tier, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
			if err := c.Approve(name, tier, nil); err != nil {
				return nil, err
			}
			return &toolkit.Result{Text: "ok"}, nil
		}}
	}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "1", Name: "net__add-peer", Args: json.RawMessage(`{}`)}, {ID: "2", Name: "val__unjail", Args: json.RawMessage(`{}`)}}},
		{Text: "done", Done: true},
	}}
	a := newTestAgent(t, prov, mk("net.add-peer", toolkit.TierLocalChange), mk("val.unjail", toolkit.TierOnChain))
	a.Ctx.Approver = func(c *toolkit.Context, p string, tier toolkit.Tier, d map[string]any) (bool, error) {
		asked = append(asked, tier)
		return true, nil
	}
	a.Policy.AutoLocal = true
	a.Run(context.Background(), "fix it")
	if len(asked) != 1 || asked[0] != toolkit.TierOnChain {
		t.Fatalf("approvals asked for %v, want only on-chain", asked)
	}
}

func TestEventApprover(t *testing.T) {
	var got *Approval
	appr := EventApprover(func(e Event) {
		got = e.Approval
		go e.Approval.Answer(true)
	})
	c := &toolkit.Context{Context: context.Background()}
	ok, err := appr(c, "broadcast tx", toolkit.TierOnChain, map[string]any{"fee": "1uatom"})
	if err != nil || !ok || got == nil || got.Tier != "on-chain" {
		t.Fatalf("ok=%v err=%v approval=%+v", ok, err, got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, _ = EventApprover(func(Event) {})(&toolkit.Context{Context: ctx}, "x", toolkit.TierOnChain, nil)
	if ok {
		t.Fatal("cancelled approval must deny")
	}
}

func TestCommands(t *testing.T) {
	a := newTestAgent(t, &mockProvider{}, stubTool{name: "val.unjail", tier: toolkit.TierOnChain})
	run := func(line string) (CmdResult, error) { return RunCommand(a, a.Ctx, a.Reg, line) }
	if r, _ := run("/mode readonly"); !strings.Contains(r.Text, "mode readonly") || !a.Policy.ReadOnly() {
		t.Fatalf("/mode readonly: %q", r.Text)
	}
	if r, _ := run("/tools"); !strings.Contains(r.Text, "deny") {
		t.Fatalf("/tools should show deny in readonly: %q", r.Text)
	}
	run("/mode ops")
	if _, err := run("/approve on-chain on"); err == nil {
		t.Fatal("/approve on-chain must fail")
	}
	if _, err := run("/approve local-change on"); err != nil || !a.Policy.AutoLocal {
		t.Fatalf("/approve local-change: %v", err)
	}
	if _, err := run("/nope"); err != ErrUnknownCommand {
		t.Fatalf("unknown command err = %v", err)
	}
	if r, _ := run("/runbook jail-recovery"); !strings.Contains(r.Prompt, "jail-recovery") {
		t.Fatalf("/runbook should produce a prompt: %+v", r)
	}
	if r, _ := RunCommand(nil, a.Ctx, a.Reg, "/profile"); !strings.Contains(r.Text, "testp") {
		t.Fatalf("/profile without agent: %q", r.Text)
	}
	if _, err := RunCommand(nil, a.Ctx, a.Reg, "/mode"); err == nil {
		t.Fatal("/mode without agent should error")
	}
}

func TestOffline(t *testing.T) {
	t.Setenv("COMETCLI_OFFLINE", "1")
	if _, err := NewProvider(config.AgentConf{Provider: "anthropic"}); err != ErrOffline {
		t.Fatalf("err = %v, want ErrOffline", err)
	}
}

func TestReplayRoundTrip(t *testing.T) {
	tool := stubTool{name: "val.signing", tier: toolkit.TierObserve, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "missed 12/10000"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Text: "checking", Calls: []Call{{ID: "c1", Name: "val__signing", Args: json.RawMessage(`{"n":1}`)}}},
		{Text: "you missed 12 blocks", Done: true},
		{Text: "nothing else", Done: true},
	}}
	a := newTestAgent(t, prov, tool)
	var live []Event
	a.OnEvent = func(e Event) { live = append(live, e) }
	a.Run(context.Background(), "am I missing blocks?")
	a.Run(context.Background(), "anything else?")
	id := a.Audit().Session()

	evs, err := audit.ReadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	var replayed []Event
	rep, err := Replay(context.Background(), evs, func(e Event) {
		if e.Kind != "prompt" {
			replayed = append(replayed, e)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 || rep.Prompts != 2 || rep.Rounds != 3 || rep.Tools != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if fmt.Sprint(live) != fmt.Sprint(replayed) {
		t.Fatalf("replay diverged:\nlive:   %v\nreplay: %v", live, replayed)
	}
}

func TestAuditSessionStamped(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "hi", Done: true}}}
	a := newTestAgent(t, prov)
	a.Run(context.Background(), "hello")
	b, _ := os.ReadFile(a.Audit().Path())
	for _, want := range []string{`"session":"` + a.ID() + `"`, `"kind":"llm"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("audit missing %s:\n%s", want, b)
		}
	}
	old := a.ID()
	a.Reset()
	if a.ID() == old {
		t.Fatal("Reset must start a new session")
	}
}

func sse(w http.ResponseWriter, lines ...string) {
	w.Header().Set("content-type", "text/event-stream")
	for _, l := range lines {
		fmt.Fprintf(w, "data: %s\n\n", l)
	}
}

func TestAnthropicStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream flag not sent")
		}
		sse(w,
			`{"type":"message_start"}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Check"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ing."}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"val__status"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"ver"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"bose\":true}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := &anthropic{key: "k", model: "m", base: srv.URL}
	var chunks []string
	res, err := p.Stream(context.Background(), &Request{Model: "m", MaxTok: 10}, func(s string) { chunks = append(chunks, s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Checking." || len(chunks) != 2 || res.Done {
		t.Fatalf("res=%+v chunks=%v", res, chunks)
	}
	if len(res.Calls) != 1 || res.Calls[0].Name != "val__status" || string(res.Calls[0].Args) != `{"verbose":true}` {
		t.Fatalf("calls = %+v", res.Calls)
	}
}

func TestOpenAIStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w,
			`{"choices":[{"delta":{"content":"Look"}}]}`,
			`{"choices":[{"delta":{"content":"ing"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"node__peers","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"n\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"3}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`)
	}))
	defer srv.Close()
	p := &openai{model: "m", base: srv.URL}
	res, err := p.Stream(context.Background(), &Request{Model: "m"}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Looking" || len(res.Calls) != 1 || string(res.Calls[0].Args) != `{"n":3}` || res.Calls[0].ID != "call_a" {
		t.Fatalf("res = %+v", res)
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()
	_, err := (&anthropic{base: srv.URL}).Stream(context.Background(), &Request{}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("err = %v", err)
	}
}

func TestToolOutputANSIStripped(t *testing.T) {
	tool := stubTool{name: "node.logs", tier: toolkit.TierDiagnose, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "\x1b[90m10:15AM\x1b[0m \x1b[32mINF\x1b[0m committed height=15171"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__logs", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a := newTestAgent(t, prov, tool)
	a.conf.Egress = "filtered"
	a.Run(context.Background(), "logs")
	if got := a.history[2].Text; got != "10:15AM INF committed height=15171" {
		t.Fatalf("tool text = %q", got)
	}
}
