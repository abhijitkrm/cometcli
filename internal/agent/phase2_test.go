package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/client/host"
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
