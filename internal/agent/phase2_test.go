package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/shell"
)

func bashCall(id, cmd string) Call {
	b, _ := json.Marshal(map[string]any{"command": cmd})
	return Call{ID: id, Name: "bash", Args: b}
}

func newShellAgent(t *testing.T, prov Provider, tools ...toolkit.Tool) (*Agent, string) {
	a := newTestAgent(t, prov, append([]toolkit.Tool{shell.Bash{}}, tools...)...)
	dir := t.TempDir()
	a.Tools = toolkit.NewSession(dir)
	a.WorkRoot = dir
	a.Ctx.SetHost(&host.Local{})
	return a, dir
}

func lastToolText(a *Agent) string {
	h := a.History()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "tool" {
			return h[i].Text
		}
	}
	return ""
}

func TestReadOnlyModeShellReadsButNeverWrites(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{bashCall("c1", "echo hello")}},
		{Calls: []Call{bashCall("c2", "touch made.txt")}},
		{Text: "done", Done: true},
	}}
	a, dir := newShellAgent(t, prov)
	a.Policy.Mode = ModeReadOnly
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, td := range prov.reqs[0].Tools {
		names = append(names, td.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "bash") {
		t.Fatalf("bash hidden in readonly: %v", names)
	}
	h := a.History()
	if !strings.Contains(h[2].Text, "hello") {
		t.Fatalf("read command blocked: %q", h[2].Text)
	}
	if !strings.Contains(h[4].Text, "read-only") {
		t.Fatalf("write not blocked: %q", h[4].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); err == nil {
		t.Fatal("file created in readonly mode")
	}
}

func TestDenyRuleBlocksRegistryTool(t *testing.T) {
	ran := false
	unjail := stubTool{name: "val.unjail", tier: toolkit.TierOnChain, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		ran = true
		return &toolkit.Result{}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "val__unjail", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a, _ := newShellAgent(t, prov, unjail)
	a.Rules, _ = toolkit.NewRules(nil, nil, []string{"val.unjail"})
	a.Run(context.Background(), "unjail")
	if ran || !strings.Contains(lastToolText(a), "denied by permission rule val.unjail") {
		t.Fatalf("ran=%v result=%q", ran, lastToolText(a))
	}
}

func TestAllowRuleSkipsLocalChangePrompt(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{bashCall("c1", "touch ok.txt")}},
		{Text: "ok", Done: true},
	}}
	a, dir := newShellAgent(t, prov)
	asked := 0
	a.Ctx.Approver = func(*toolkit.Context, string, toolkit.Tier, map[string]any) (bool, error) { asked++; return false, nil }
	a.Ctx.AutoApproveBelow = toolkit.TierLocalChange
	a.Rules, _ = toolkit.NewRules([]string{"bash(touch:*)"}, nil, nil)
	a.Run(context.Background(), "x")
	if asked != 0 {
		t.Fatal("allow rule still asked")
	}
	if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
		t.Fatalf("allowed command didn't run: %s", lastToolText(a))
	}
}

func TestTodoWriteEmitsChecklist(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "todo_write", Args: json.RawMessage(`{"todos":[{"content":"stop node","status":"completed"},{"content":"swap binary","status":"in_progress"},{"content":"start node","status":"pending"}]}`)}}},
		{Text: "ok", Done: true},
	}}
	a, _ := newShellAgent(t, prov)
	var got []Todo
	a.OnEvent = func(e Event) {
		if e.Kind == EvTodos {
			got = e.Todos
		}
	}
	a.Run(context.Background(), "upgrade")
	if len(got) != 3 || !strings.Contains(lastToolText(a), "1/3 done") {
		t.Fatalf("todos=%v result=%q", got, lastToolText(a))
	}
	if r := RenderTodos(got); !strings.Contains(r, "☒ stop node") || !strings.Contains(r, "▸ swap binary") {
		t.Fatalf("render = %q", r)
	}
}

func TestBashOutputUsesToolLimit(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{bashCall("c1", "head -c 20000 /dev/zero | tr '\\0' 'a'")}},
		{Text: "ok", Done: true},
	}}
	a, _ := newShellAgent(t, prov)
	a.Run(context.Background(), "x")
	if n := len(lastToolText(a)); n < 19000 || n > 30500 {
		t.Fatalf("bash output length %d — should keep up to 30k", n)
	}
}

func TestDeferredToolsLoadOnDemand(t *testing.T) {
	gov := stubTool{name: "chain.gov", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "proposal 3 voting"}, nil
	}}
	core := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "tool_search", Args: json.RawMessage(`{"query":"select:chain.gov"}`)}}},
		{Calls: []Call{{ID: "c2", Name: "chain__gov", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a, _ := newShellAgent(t, prov, gov, core)
	a.Run(context.Background(), "any proposals?")
	names := func(r *Request) string {
		var n []string
		for _, td := range r.Tools {
			n = append(n, td.Name)
		}
		return strings.Join(n, ",")
	}
	if first := names(prov.reqs[0]); strings.Contains(first, "chain__gov") || !strings.Contains(first, "node__status") || !strings.Contains(first, "tool_search") {
		t.Fatalf("first request tools = %s", first)
	}
	if !strings.Contains(prov.reqs[0].System, "- chain: gov") {
		t.Fatalf("catalog missing from system prompt: %q", prov.reqs[0].System)
	}
	if second := names(prov.reqs[1]); !strings.Contains(second, "chain__gov") {
		t.Fatalf("loaded tool not advertised: %s", second)
	}
	if !strings.Contains(lastToolText(a), "proposal 3") {
		t.Fatalf("loaded tool didn't run: %q", lastToolText(a))
	}
	// keyword search ranks by name/description
	if got := a.searchTools("governance gov proposals", 3); len(got) == 0 || got[0].Name() != "chain.gov" {
		t.Fatalf("search = %v", got)
	}
	if lt := a.LoadedTools(); len(lt) != 1 || lt[0] != "chain.gov" {
		t.Fatalf("loaded = %v", lt)
	}
}

func TestGeneralModeAndScopeSwitch(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "a", Done: true}, {Text: "b", Done: true}, {Text: "c", Done: true}}}
	nodeTool := stubTool{name: "node.status", tier: toolkit.TierObserve, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) { return &toolkit.Result{}, nil }}
	a, _ := newShellAgent(t, prov, nodeTool)
	prof := a.Ctx.Profile
	prof.Role, prof.ChainID = "validator", "primium-1"
	a.Ctx.Cfg = &config.Config{Profiles: map[string]*config.Profile{"testp": prof}}
	a.SwitchScope(nil) // start in general mode
	a.carry = ""
	a.Run(context.Background(), "one")
	r := prov.reqs[0]
	if !strings.Contains(r.System, "working in the operator's terminal") || !strings.Contains(r.System, "testp (validator, primium-1)") {
		t.Fatalf("general prompt = %q", r.System)
	}
	for _, td := range r.Tools {
		if td.Name == "node__status" || td.Name == toolSearchName {
			t.Fatalf("node tool %s advertised in general mode", td.Name)
		}
	}
	if strings.Contains(r.System, "LIVE SNAPSHOT") {
		t.Fatal("snapshot in general mode")
	}

	a.SwitchScope(prof)
	a.Run(context.Background(), "two")
	r = prov.reqs[1]
	if !strings.Contains(r.System, "Cosmos-EVM validators") || !strings.Contains(r.System, "LIVE: height=42") {
		t.Fatalf("node prompt = %q", r.System)
	}
	var names []string
	for _, td := range r.Tools {
		names = append(names, td.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "node__status") {
		t.Fatalf("node tools missing: %v", names)
	}
	last := r.Messages[len(r.Messages)-1].Text
	if !strings.Contains(last, "switched this session to node mode for profile testp") || !strings.HasSuffix(last, "two") {
		t.Fatalf("switch note missing: %q", last)
	}
	if len(r.Messages) != 3 {
		t.Fatalf("conversation not kept: %d messages", len(r.Messages))
	}
}

type opOnly struct{ stubTool }

func (opOnly) OperatorOnly() bool { return true }

func TestOperatorOnlyToolsNeverReachTheAgent(t *testing.T) {
	ran := false
	keyAdd := opOnly{stubTool{name: "keys.add", tier: toolkit.TierLocalChange, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		ran = true
		return &toolkit.Result{Text: "mnemonic: …"}, nil
	}}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "keys__add", Args: json.RawMessage(`{"name":"x"}`)}}},
		{Text: "ok", Done: true},
	}}
	a, _ := newShellAgent(t, prov, keyAdd)
	a.conf.Tools = "all"
	a.Run(context.Background(), "make me a key")
	for _, td := range prov.reqs[0].Tools {
		if td.Name == "keys__add" {
			t.Fatal("operator-only tool advertised")
		}
	}
	if ran || !strings.Contains(lastToolText(a), "operator-only") {
		t.Fatalf("ran=%v result=%q", ran, lastToolText(a))
	}
}
