package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// mockProvider returns scripted responses in order.
type mockProvider struct {
	responses []*Response
	calls     int
	lastReq   *Request
	err       error
}

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) Chat(ctx context.Context, r *Request) (*Response, error) {
	m.calls++
	m.lastReq = r
	if m.err != nil {
		return nil, m.err
	}
	if m.calls > len(m.responses) {
		return &Response{Text: "done", Done: true}, nil
	}
	return m.responses[m.calls-1], nil
}

// stubTool is a minimal registry tool.
type stubTool struct {
	name string
	tier toolkit.Tier
	run  func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error)
}

func (s stubTool) Name() string { return s.name }
func (s stubTool) Desc() string { return "stub " + s.name }
func (s stubTool) Schema() map[string]any {
	return toolkit.ObjSchema(map[string]any{})
}
func (s stubTool) Tier() toolkit.Tier { return s.tier }
func (s stubTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	return s.run(c, a)
}

func newTestAgent(t *testing.T, prov Provider, tools ...toolkit.Tool) *Agent {
	t.Helper()
	t.Setenv("COMETCLI_HOME", t.TempDir())
	reg := toolkit.NewRegistry()
	for _, tool := range tools {
		reg.Register(tool)
	}
	aud, err := audit.Open("testp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	ctx := &toolkit.Context{
		Context:          context.Background(),
		Profile:          &config.Profile{Name: "testp"},
		Audit:            aud,
		AutoApproveBelow: toolkit.TierOnChain, // observe/diagnose/local-change auto-run
	}
	return &Agent{Provider: prov, Model: "mock-1", Reg: reg, Ctx: ctx, MaxIter: 4}
}

func TestLoop_ToolCallRoundTrip(t *testing.T) {
	var gotArg string
	reg := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		gotArg = a.String("verbose", "")
		return &toolkit.Result{Text: "height 42 catching_up=false"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__status", Args: json.RawMessage(`{"verbose":"true"}`)}}},
		{Text: "node is at height 42", Done: true},
	}}
	a := newTestAgent(t, prov, reg)
	out, err := a.Run(context.Background(), "check the node")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "height 42") {
		t.Fatalf("final text = %q", out)
	}
	if gotArg != "true" {
		t.Fatalf("tool arg = %q, want 'true'", gotArg)
	}
	// history: user, assistant(calls), tool result, assistant(final)
	if len(a.history) != 4 {
		t.Fatalf("history len = %d, want 4", len(a.history))
	}
	toolMsg := a.history[2]
	if toolMsg.Role != "tool" || toolMsg.CallID != "c1" || toolMsg.ToolName != "node.status" {
		t.Fatalf("tool msg malformed: %+v", toolMsg)
	}
	if !strings.Contains(toolMsg.Text, "height 42") {
		t.Fatalf("tool result text = %q", toolMsg.Text)
	}
}

func TestLoop_ToolResultRedacted(t *testing.T) {
	secret := "0x" + strings.Repeat("ab", 32)
	reg := stubTool{name: "x.y", tier: toolkit.TierObserve, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "leaked " + secret}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "x__y", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a := newTestAgent(t, prov, reg)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.history[2].Text, secret) {
		t.Fatalf("secret reached history: %q", a.history[2].Text)
	}
	if !strings.Contains(a.history[2].Text, "[REDACTED_HEX]") {
		t.Fatalf("expected redaction marker: %q", a.history[2].Text)
	}
}

func TestLoop_UnknownToolAndBadArgs(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{
			{ID: "c1", Name: "no__such", Args: json.RawMessage(`{}`)},
			{ID: "c2", Name: "node__status", Args: json.RawMessage(`{bad json`)},
		}},
		{Text: "done", Done: true},
	}}
	a := newTestAgent(t, prov, stubTool{name: "node.status", tier: toolkit.TierObserve,
		run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "ok"}, nil
		}})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !a.history[2].IsError || !strings.Contains(a.history[2].Text, "no such tool") {
		t.Fatalf("unknown tool not flagged: %+v", a.history[2])
	}
	if !a.history[3].IsError || !strings.Contains(a.history[3].Text, "bad args") {
		t.Fatalf("bad args not flagged: %+v", a.history[3])
	}
}

func TestLoop_IterationLimit(t *testing.T) {
	calls := []Call{{ID: "c", Name: "x__y", Args: json.RawMessage(`{}`)}}
	prov := &mockProvider{responses: []*Response{
		{Calls: calls}, {Calls: calls}, {Calls: calls}, {Calls: calls}, {Calls: calls},
	}}
	a := newTestAgent(t, prov, stubTool{name: "x.y", tier: toolkit.TierObserve,
		run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "ok"}, nil
		}})
	_, err := a.Run(context.Background(), "loop forever")
	if err == nil || !strings.Contains(err.Error(), "iterations") {
		t.Fatalf("expected iteration limit error, got %v", err)
	}
}

func TestLoop_OnChainApprovalDenied(t *testing.T) {
	// the approval gate lives inside the tool (BroadcastMsgs pattern), so
	// the stub must invoke Approve to mirror real on-chain tools
	reg := stubTool{name: "val.unjail", tier: toolkit.TierOnChain, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		if err := c.Approve("unjail validator", toolkit.TierOnChain, nil); err != nil {
			return nil, err
		}
		return &toolkit.Result{Text: "unjailed"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "val__unjail", Args: json.RawMessage(`{}`)}}},
		{Text: "done", Done: true},
	}}
	a := newTestAgent(t, prov, reg)
	a.Ctx.Approver = func(c *toolkit.Context, p string, tier toolkit.Tier, d map[string]any) (bool, error) {
		return false, nil
	}
	if _, err := a.Run(context.Background(), "unjail me"); err != nil {
		t.Fatal(err)
	}
	// approval denial surfaces as an error tool result, not a crash
	toolMsg := a.history[2]
	if !toolMsg.IsError || !strings.Contains(toolMsg.Text, "denied") {
		t.Fatalf("expected denial in tool result: %+v", toolMsg)
	}
}

func TestLoop_AuditTrail(t *testing.T) {
	reg := stubTool{name: "x.y", tier: toolkit.TierObserve, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "fine"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "x__y", Args: json.RawMessage(`{}`)}}},
		{Text: "done", Done: true},
	}}
	a := newTestAgent(t, prov, reg)
	if _, err := a.Run(context.Background(), "audit me"); err != nil {
		t.Fatal(err)
	}
	a.Audit().Close()
	p := a.Audit().Path()
	b, _ := os.ReadFile(p)
	for _, want := range []string{`"kind":"prompt"`, `"kind":"tool"`, `"name":"x.y"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("audit log missing %s:\n%s", want, b)
		}
	}
}
