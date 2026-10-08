package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/hooks"
	"github.com/abhijitkrm/cometcli/internal/settings"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

func hookScript(t *testing.T, dir, name, body string) string {
	p := filepath.Join(dir, name)
	os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	return p
}

func withExt(a *Agent, dir string, hm map[string][]settings.HookMatcher) {
	a.ext = &ext{Settings: &settings.Settings{Root: dir}, Hooks: &hooks.Runner{Matchers: hm, Dir: dir},
		Commands: map[string]*settings.Command{}, Agents: map[string]*settings.AgentDef{}}
}

func TestPreAndPostToolUseHooks(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{bashCall("c1", "echo should-not-run > x.txt")}},
		{Calls: []Call{bashCall("c2", "echo fine")}},
		{Text: "done", Done: true},
	}}
	a, dir := newShellAgent(t, prov)
	pre := hookScript(t, dir, "pre.sh", `grep -q 'x.txt' && { echo "writing x.txt is not allowed" >&2; exit 2; }; exit 0`)
	post := hookScript(t, dir, "post.sh", `echo '{"decision":"block","reason":"remember to check peers"}'`)
	withExt(a, dir, map[string][]settings.HookMatcher{
		hooks.PreToolUse:  {{Matcher: "bash", Hooks: []settings.HookCommand{{Command: pre}}}},
		hooks.PostToolUse: {{Matcher: "Bash", Hooks: []settings.HookCommand{{Command: post}}}},
	})
	a.Run(context.Background(), "go")
	h := a.History()
	if !strings.Contains(h[2].Text, "blocked by PreToolUse hook: writing x.txt is not allowed") {
		t.Fatalf("pre hook: %q", h[2].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); err == nil {
		t.Fatal("blocked command ran")
	}
	if !strings.Contains(h[4].Text, "fine") || !strings.Contains(h[4].Text, "[PostToolUse hook] remember to check peers") {
		t.Fatalf("post hook: %q", h[4].Text)
	}
}

func TestPreToolUseAllowSkipsPromptButNotForTx(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{bashCall("c1", "touch ok.txt")}},
		{Calls: []Call{bashCall("c2", "evmd tx gov vote 1 yes --from v")}},
		{Text: "done", Done: true},
	}}
	a, dir := newShellAgent(t, prov)
	asked := []string{}
	a.Ctx.Approver = func(_ *toolkit.Context, p string, _ toolkit.Tier, _ map[string]any) (bool, error) {
		asked = append(asked, p)
		return false, nil
	}
	a.Ctx.AutoApproveBelow = toolkit.TierLocalChange
	allow := hookScript(t, dir, "allow.sh", `echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`)
	withExt(a, dir, map[string][]settings.HookMatcher{hooks.PreToolUse: {{Hooks: []settings.HookCommand{{Command: allow}}}}})
	a.Run(context.Background(), "go")
	if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
		t.Fatal("hook allow didn't skip the prompt")
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "evmd tx") {
		t.Fatalf("a tx must still ask despite a hook allow: %v", asked)
	}
}

func TestUserPromptSubmitAndStopHooks(t *testing.T) {
	prov := &mockProvider{responses: []*Response{{Text: "first", Done: true}, {Text: "second", Done: true}}}
	a, dir := newShellAgent(t, prov)
	ups := hookScript(t, dir, "ups.sh", `in=$(cat); case "$in" in *secret-project*) echo "not here" >&2; exit 2;; esac; echo "on-call: alice"`)
	stop := hookScript(t, dir, "stop.sh", `in=$(cat); case "$in" in *'"stop_hook_active":true'*) exit 0;; esac; echo '{"decision":"block","reason":"verify with node status"}'`)
	withExt(a, dir, map[string][]settings.HookMatcher{
		hooks.UserPromptSubmit: {{Hooks: []settings.HookCommand{{Command: ups}}}},
		hooks.Stop:             {{Hooks: []settings.HookCommand{{Command: stop}}}},
	})
	if _, err := a.Run(context.Background(), "tell me about secret-project"); err == nil || !strings.Contains(err.Error(), "not here") {
		t.Fatalf("prompt not blocked: %v", err)
	}
	if prov.calls != 0 {
		t.Fatal("blocked prompt reached the model")
	}
	out, err := a.Run(context.Background(), "status?")
	if err != nil || out != "second" || prov.calls != 2 {
		t.Fatalf("stop hook should force one more round: out=%q calls=%d err=%v", out, prov.calls, err)
	}
	if !strings.Contains(prov.reqs[0].Messages[0].Text, "on-call: alice") {
		t.Fatalf("hook context missing: %q", prov.reqs[0].Messages[0].Text)
	}
	last := prov.reqs[1].Messages[len(prov.reqs[1].Messages)-1].Text
	if !strings.Contains(last, "[Stop hook] verify with node status") {
		t.Fatalf("stop reason not fed back: %q", last)
	}
}

func TestCustomCommandExpandsAndGrantsTurnRules(t *testing.T) {
	prov := &mockProvider{}
	a, dir := newShellAgent(t, prov)
	withExt(a, dir, nil)
	a.ext.Settings.Trusted = true
	os.WriteFile(filepath.Join(dir, "network-config.env"), []byte("CHAIN_ID=mychain-1\n"), 0o644)
	a.ext.Commands["vote"] = &settings.Command{Name: "vote", Scope: "project", AllowedTools: []string{"bash(./upgrade/vote-upgrade.sh:*)"},
		Body: "Vote $1 on proposal $2.\nHost: !`uname -s`\nRefused: !`touch x`\nConfig: @network-config.env"}
	res, err := RunCommand(a, a.Ctx, a.Reg, "/vote yes 7")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Vote yes on proposal 7.", "Host: " + uname(t), "not run: only read-only", "CHAIN_ID=mychain-1"} {
		if !strings.Contains(res.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, res.Prompt)
		}
	}
	if !strings.Contains(res.Text, "allowing for this turn") {
		t.Fatalf("text = %q", res.Text)
	}
	done := a.grantTurnRules()
	if d, _ := a.Rules.Decide(toolkit.Request{Tool: "bash", Kind: "command", Specs: []string{"./upgrade/vote-upgrade.sh 7 yes"}}); d != toolkit.DecideAllow {
		t.Fatal("turn rule not granted")
	}
	done()
	if d, _ := a.Rules.Decide(toolkit.Request{Tool: "bash", Kind: "command", Specs: []string{"./upgrade/vote-upgrade.sh 7 yes"}}); d != toolkit.DecideDefault {
		t.Fatal("turn rule outlived the turn")
	}
	// untrusted project: allowed-tools ignored
	a.ext.Settings.Trusted = false
	res, _ = RunCommand(a, a.Ctx, a.Reg, "/vote yes 7")
	if !strings.Contains(res.Text, "ignored") || len(a.ext.turnRule) != 0 {
		t.Fatalf("untrusted grant: %q %v", res.Text, a.ext.turnRule)
	}
}

func TestMemoryInSystemPrompt(t *testing.T) {
	prov := &mockProvider{}
	a, dir := newShellAgent(t, prov)
	withExt(a, dir, nil)
	os.WriteFile(filepath.Join(dir, "COMET.md"), []byte("Validators run in docker; never restart val0 without asking."), 0o644)
	a.WorkRoot = dir
	a.Run(context.Background(), "hi")
	if !strings.Contains(prov.reqs[0].System, "never restart val0 without asking") {
		t.Fatalf("memory missing from system prompt")
	}
	res, err := RunCommand(a, a.Ctx, a.Reg, "/remember peers live in config.toml")
	if err != nil || !strings.Contains(res.Text, filepath.Join(dir, "COMET.md")) {
		t.Fatalf("/remember: %v %v", res, err)
	}
	a.Run(context.Background(), "next")
	last := prov.reqs[1].Messages[len(prov.reqs[1].Messages)-1].Text
	if !strings.Contains(last, "Operator added to memory") || prov.reqs[1].System != prov.reqs[0].System {
		t.Fatalf("memory update should ride on the turn, system frozen: %q", last)
	}
}

func TestSubagentTask(t *testing.T) {
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "t1", Name: "task", Args: json.RawMessage(`{"description":"scan logs","prompt":"find panics in the logs"}`)}}},
		{Calls: []Call{bashCall("s1", "echo 'panic: oom'")}}, // subagent round 1
		{Text: "REPORT: one OOM panic", Done: true},          // subagent final
		{Text: "The node OOM-panicked once.", Done: true},    // parent final
	}}
	a, _ := newShellAgent(t, prov)
	var notices []string
	a.OnEvent = func(e Event) {
		if e.Kind == EvToolStart {
			notices = append(notices, e.Tool)
		}
	}
	out, err := a.Run(context.Background(), "check logs")
	if err != nil || out != "The node OOM-panicked once." {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got := a.History()[2].Text; got != "REPORT: one OOM panic" {
		t.Fatalf("task result = %q", got)
	}
	child := prov.reqs[1]
	if !strings.Contains(child.System, "YOU ARE A SUBAGENT") || len(child.Messages) != 1 {
		t.Fatalf("subagent request: %d msgs", len(child.Messages))
	}
	for _, td := range child.Tools {
		if td.Name == taskToolName {
			t.Fatal("subagent can spawn subagents")
		}
	}
	if strings.Join(notices, ",") != "task,↳ bash" {
		t.Fatalf("events = %v", notices)
	}
	if len(a.History()) != 4 {
		t.Fatalf("subagent turns leaked into the parent history: %d", len(a.History()))
	}
}

func TestMCPToolsDeferredInGeneralMode(t *testing.T) {
	mcpTool := stubTool{name: "mcp__grafana__query", tier: toolkit.TierDiagnose, run: func(*toolkit.Context, toolkit.Args) (*toolkit.Result, error) {
		return &toolkit.Result{Text: "p99 120ms"}, nil
	}}
	prov := &mockProvider{responses: []*Response{
		{Calls: []Call{{ID: "c1", Name: "tool_search", Args: json.RawMessage(`{"query":"select:mcp__grafana__query"}`)}}},
		{Calls: []Call{{ID: "c2", Name: "mcp__grafana__query", Args: json.RawMessage(`{}`)}}},
		{Text: "ok", Done: true},
	}}
	a, _ := newShellAgent(t, prov, mcpTool)
	a.Ctx.Profile = nil // general mode
	a.Run(context.Background(), "latency?")
	first := strings.Join(toolNames(prov.reqs[0]), ",")
	if strings.Contains(first, "mcp__grafana__query") || !strings.Contains(first, "tool_search") {
		t.Fatalf("first tools = %s", first)
	}
	if !strings.Contains(prov.reqs[0].System, "mcp__grafana: query") {
		t.Fatalf("catalog missing MCP tools")
	}
	if !strings.Contains(strings.Join(toolNames(prov.reqs[1]), ","), "mcp__grafana__query") || !strings.Contains(lastToolText(a), "p99 120ms") {
		t.Fatalf("MCP tool not loaded/called: %q", lastToolText(a))
	}
}

func uname(t *testing.T) string {
	out, err := exec.Command("uname", "-s").Output()
	if err != nil {
		t.Skip("no uname")
	}
	return strings.TrimSpace(string(out))
}

func toolNames(r *Request) []string {
	var n []string
	for _, td := range r.Tools {
		n = append(n, td.Name)
	}
	return n
}
