package agent

// eval_test.go — golden agent scenarios. Each drives a scripted mock
// provider through the real loop and asserts on tool-call order, safety
// boundaries, and the final answer. These are the behavioral spec for
// "is the agent actually agentic": a regression here means the agent
// changed how it operates, not just what it says.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// evalScenario describes one scripted run and what must hold at the end.
type evalScenario struct {
	name     string
	input    string
	safe     bool
	maxCalls int
	script   []*Response
	// expectations
	wantCalls []string // tool names that must have been invoked, in order
	blocked   []string // tool names that must NOT have executed
	wantText  string   // substring required in the final answer
	wantErrIn string   // substring required inside some tool error result
}

func runScenario(t *testing.T, sc evalScenario, tools ...toolkit.Tool) *Agent {
	t.Helper()
	ran := map[string]bool{}
	var wrapped []toolkit.Tool
	for _, tl := range tools {
		tl := tl
		wrapped = append(wrapped, stubTool{
			name: tl.Name(), tier: tl.Tier(),
			run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
				ran[tl.Name()] = true
				return tl.Run(c, a)
			},
		})
	}
	prov := &mockProvider{responses: sc.script}
	a := newTestAgent(t, prov, wrapped...)
	if sc.safe {
		a.Policy.Mode = ModeReadOnly
	}
	a.MaxCalls = sc.maxCalls
	a.MaxIter = 8

	out, err := a.Run(context.Background(), sc.input)
	if err != nil {
		t.Fatalf("%s: Run: %v", sc.name, err)
	}

	// collect invoked tool names from history, preserving order
	var got []string
	var errTexts []string
	for _, m := range a.history {
		if m.Role == "tool" {
			got = append(got, m.ToolName)
			if m.IsError {
				errTexts = append(errTexts, m.Text)
			}
		}
	}
	if len(got) < len(sc.wantCalls) {
		t.Fatalf("%s: calls %v shorter than expected %v", sc.name, got, sc.wantCalls)
	}
	for i, want := range sc.wantCalls {
		if got[i] != want {
			t.Fatalf("%s: call[%d] = %q, want %q (all: %v)", sc.name, i, got[i], want, got)
		}
	}
	for _, b := range sc.blocked {
		if ran[b] {
			t.Fatalf("%s: %s executed but should have been blocked", sc.name, b)
		}
	}
	if sc.wantText != "" && !strings.Contains(out, sc.wantText) {
		t.Fatalf("%s: final answer %q missing %q", sc.name, out, sc.wantText)
	}
	if sc.wantErrIn != "" {
		found := false
		for _, e := range errTexts {
			if strings.Contains(e, sc.wantErrIn) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no tool error contains %q (errs: %v)", sc.name, sc.wantErrIn, errTexts)
		}
	}
	return a
}

// A jailed validator: the agent should check status, then signing window,
// then explain — not jump straight to unjail.
func TestEval_JailedValidatorTriage(t *testing.T) {
	valStatus := stubTool{name: "val.status", tier: toolkit.TierObserve,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "valoper xyz: JAILED, missed 9931/10000"}, nil
		}}
	valSigning := stubTool{name: "val.signing", tier: toolkit.TierObserve,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "missed=9931 window=10000 jailed_until=2025-01-01T01:00:00Z"}, nil
		}}
	runScenario(t, evalScenario{
		name:  "jailed triage",
		input: "the monitor says my validator is jailed",
		script: []*Response{
			{Calls: []Call{{ID: "c1", Name: "val__status", Args: json.RawMessage(`{}`)}}},
			{Calls: []Call{{ID: "c2", Name: "val__signing", Args: json.RawMessage(`{}`)}}},
			{Text: "Validator is jailed after missing 9931/10000 blocks. jailed_until passed — safe to run `val unjail`.", Done: true},
		},
		wantCalls: []string{"val.status", "val.signing"},
		wantText:  "unjail",
	}, valStatus, valSigning)
}

// Safe mode: a mutating tool isn't advertised AND can't execute even if
// the model hallucinates the call — the refusal becomes a tool error the
// model must narrate around.
func TestEval_SafeModeRefusesMutation(t *testing.T) {
	unjail := stubTool{name: "val.unjail", tier: toolkit.TierOnChain,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "unjailed"}, nil
		}}
	a := runScenario(t, evalScenario{
		name:  "safe mode",
		input: "unjail my validator now",
		safe:  true,
		script: []*Response{
			{Calls: []Call{{ID: "c1", Name: "val__unjail", Args: json.RawMessage(`{}`)}}},
			{Text: "Can't unjail in safe mode — run `cometcli val unjail` yourself.", Done: true},
		},
		wantCalls: []string{"val.unjail"}, // attempted…
		blocked:   []string{"val.unjail"}, // …but never executed
		wantErrIn: "blocked",
		wantText:  "safe mode",
	}, unjail)
	// and it was filtered from the advertised tool set
	for _, d := range a.toolDefs() {
		if d.Name == "val__unjail" {
			t.Fatal("mutating tool leaked into safe-mode toolDefs")
		}
	}
}

// Budget: after N calls the loop refuses and tells the model to wrap up.
func TestEval_BudgetCapsToolCalls(t *testing.T) {
	status := stubTool{name: "node.status", tier: toolkit.TierObserve,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "h=100"}, nil
		}}
	peers := stubTool{name: "node.peers", tier: toolkit.TierObserve,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "peers=5"}, nil
		}}
	runScenario(t, evalScenario{
		name:     "budget",
		input:    "check everything",
		maxCalls: 1,
		script: []*Response{
			{Calls: []Call{
				{ID: "c1", Name: "node__status", Args: json.RawMessage(`{}`)},
				{ID: "c2", Name: "node__peers", Args: json.RawMessage(`{}`)},
				{ID: "c3", Name: "node__peers", Args: json.RawMessage(`{}`)},
			}},
			{Text: "Partial: height 100; budget stopped further checks.", Done: true},
		},
		wantCalls: []string{"node.status", "node.peers", "node.peers"},
		blocked:   []string{"node.peers"}, // ran once? no — budget=1, so BOTH peers calls refused
		wantErrIn: "budget exhausted",
		wantText:  "budget",
	}, status, peers)
}

// The live snapshot is probed once per turn, not once per iteration.
func TestEval_SnapshotCachedPerTurn(t *testing.T) {
	st := stubTool{name: "node.status", tier: toolkit.TierObserve,
		run: func(_ *toolkit.Context, _ toolkit.Args) (*toolkit.Result, error) {
			return &toolkit.Result{Text: "h=7"}, nil
		}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "node__status", Args: json.RawMessage(`{}`)}}},
		{Calls: []Call{{ID: "c2", Name: "node__status", Args: json.RawMessage(`{}`)}}},
		{Text: "all good", Done: true},
	}}
	a := newTestAgent(t, prov, st)
	if _, err := a.Run(context.Background(), "check twice"); err != nil {
		t.Fatal(err)
	}
	if len(prov.reqs) < 2 {
		t.Fatalf("expected ≥2 provider calls, got %d", len(prov.reqs))
	}
	for i, r := range prov.reqs {
		if i == 0 {
			if r.System == "" {
				t.Fatal("empty system prompt")
			}
			continue
		}
		if r.System != prov.reqs[0].System {
			t.Fatalf("system prompt re-rendered between iterations %d and %d — snapshot should cache per turn", 0, i)
		}
	}
}
