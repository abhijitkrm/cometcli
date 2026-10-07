package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// "is anyone jailed?" in general mode: the model switches to a node
// profile itself and answers — the operator never types /one.
func TestModelSwitchesToNodeItself(t *testing.T) {
	var sawProfile string
	validators := stubTool{name: "chain.validators", tier: toolkit.TierObserve, run: func(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
		if c.Profile != nil {
			sawProfile = c.Profile.Name
		}
		return &toolkit.Result{Text: "val-3  UNBONDING  JAILED"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "use_node", Args: json.RawMessage(`{"profile":"val01"}`)}}},
		{Calls: []Call{{ID: "c2", Name: "chain__validators", Args: json.RawMessage(`{"status":"jailed"}`)}}},
		{Text: "val-3 is jailed", Done: true},
	}}
	a := newTestAgent(t, prov, validators)
	cfg := &config.Config{Profiles: map[string]*config.Profile{
		"val01": {ChainID: "primium-1", Role: "validator"},
		"val02": {ChainID: "primium-1", Role: "validator"},
	}}
	a.Ctx.Profile, a.Ctx.Cfg = nil, cfg // general mode
	if a.Node() {
		t.Fatal("should start in general mode")
	}
	out, err := a.Run(context.Background(), "is anyone jailed?")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Node() || a.Ctx.Profile.Name != "val01" || sawProfile != "val01" {
		t.Fatalf("node=%v profile=%v tool saw %q", a.Node(), a.Ctx.Profile, sawProfile)
	}
	if !strings.Contains(out, "val-3 is jailed") {
		t.Fatalf("answer %q", out)
	}
	// the first request (general mode) offered use_node and told the model to use it
	first := prov.reqs[0]
	offered := false
	for _, d := range first.Tools {
		offered = offered || d.Name == "use_node"
	}
	if !offered || !strings.Contains(first.System, "call use_node") || strings.Contains(first.System, "operator switches with /one") {
		t.Fatalf("general prompt/tools don't steer to use_node")
	}
	// after the switch the node prompt and node tools were in play
	if !strings.Contains(prov.reqs[1].System, "You operate ONE node") {
		t.Fatal("system prompt not switched to node mode mid-turn")
	}
	// and the model can go back
	a.execCall(context.Background(), Call{ID: "c3", Name: "use_node", Args: json.RawMessage(`{"profile":"off"}`)})
	if a.Node() {
		t.Fatal("off didn't return to general mode")
	}
	if m := a.execCall(context.Background(), Call{ID: "c4", Name: "use_node", Args: json.RawMessage(`{"profile":"nope"}`)}); !m.IsError {
		t.Fatal("unknown profile accepted")
	}
}
