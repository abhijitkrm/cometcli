package chatui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/redact"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Chat(context.Context, *agent.Request) (*agent.Response, error) {
	return &agent.Response{Text: "ok", Done: true}, nil
}

func newTestModel(t *testing.T) (*model, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("COMETCLI_HOME", home)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "upgrade"), 0o755)
	os.WriteFile(filepath.Join(dir, "upgrade", "vote-upgrade.sh"), []byte("x"), 0o755)
	a := &agent.Agent{
		Provider: fakeProvider{}, Model: "m", Reg: toolkit.NewRegistry(),
		Ctx:     &toolkit.Context{Context: context.Background(), Cfg: &config.Config{}},
		MaxIter: 4, Redact: redact.NewRedactor(), Tools: toolkit.NewSession(dir), WorkRoot: dir,
	}
	m := newModel(Options{Agent: a})
	m.width = 100
	return m, dir
}

func typeText(m *model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func key(m *model, k tea.KeyType) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: k})
	return cmd
}

func TestApprovalYesAlwaysNo(t *testing.T) {
	m, _ := newTestModel(t)
	sent := make(chan tea.Msg, 1)
	m.send = func(msg tea.Msg) { sent <- msg }
	ctx := &toolkit.Context{Context: context.Background()}

	ask := func(detail map[string]any, tier toolkit.Tier) chan approvalResult {
		done := make(chan approvalResult, 1)
		go func() {
			ok, err := m.approve(ctx, "run: docker compose up -d", tier, detail)
			done <- approvalResult{ok, err}
		}()
		select {
		case msg := <-sent:
			m.Update(msg)
		case <-time.After(2 * time.Second):
			t.Fatal("approval request never arrived")
		}
		return done
	}

	// yes
	done := ask(map[string]any{"command": "docker compose up -d", toolkit.RuleHint: "bash(docker compose:*)"}, toolkit.TierLocalChange)
	title, body := describe(m.approval)
	if title != "Bash command" || !strings.Contains(body, "docker compose up -d") || strings.Contains(body, "_rule") {
		t.Fatalf("dialog: %q %q", title, body)
	}
	if len(m.approval.options()) != 3 {
		t.Fatal("expected yes / always / no")
	}
	key(m, tea.KeyEnter)
	if r := <-done; !r.ok || m.approval != nil {
		t.Fatalf("yes: %+v", r)
	}

	// always: rule added and saved
	done = ask(map[string]any{"command": "docker compose up -d", toolkit.RuleHint: "bash(docker compose:*)"}, toolkit.TierLocalChange)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if r := <-done; !r.ok {
		t.Fatal("always should approve")
	}
	if d, _ := m.a.Rules.Decide(toolkit.Request{Tool: "bash", Kind: "command", Specs: []string{"docker compose down"}}); d != toolkit.DecideAllow {
		t.Fatal("rule not added to the session")
	}
	b, _ := os.ReadFile(filepath.Join(os.Getenv("COMETCLI_HOME"), "settings.json"))
	if !strings.Contains(string(b), "bash(docker compose:*)") {
		t.Fatalf("rule not saved: %s", b)
	}

	// a transaction never offers "don't ask again"; esc declines
	done = ask(map[string]any{toolkit.RuleHint: "val.unjail"}, toolkit.TierOnChain)
	if len(m.approval.options()) != 2 {
		t.Fatalf("tx options = %d", len(m.approval.options()))
	}
	cancelled := false
	m.cancel = func() { cancelled = true }
	key(m, tea.KeyEsc)
	if r := <-done; r.ok || r.err == nil || !cancelled {
		t.Fatalf("esc should decline and interrupt: %+v cancelled=%v", r, cancelled)
	}
}

func TestSlashAndFileSuggestions(t *testing.T) {
	m, _ := newTestModel(t)
	typeText(m, "/com")
	if len(m.sugg) == 0 || m.sugg[0].insert != "/compact" {
		t.Fatalf("slash sugg = %+v", m.sugg)
	}
	key(m, tea.KeyTab)
	if m.ta.Value() != "/compact " {
		t.Fatalf("tab completion = %q", m.ta.Value())
	}
	m.ta.Reset()
	typeText(m, "look at @vote")
	if len(m.sugg) != 1 || m.sugg[0].insert != "@upgrade/vote-upgrade.sh" {
		t.Fatalf("file sugg = %+v", m.sugg)
	}
	key(m, tea.KeyEnter) // enter accepts a file suggestion, doesn't submit
	if m.ta.Value() != "look at @upgrade/vote-upgrade.sh " || m.running {
		t.Fatalf("after enter: %q running=%v", m.ta.Value(), m.running)
	}
}

func TestHistoryModeCycleAndQuit(t *testing.T) {
	m, _ := newTestModel(t)
	m.history = []string{"first", "second"}
	key(m, tea.KeyUp)
	if m.ta.Value() != "second" {
		t.Fatalf("up = %q", m.ta.Value())
	}
	key(m, tea.KeyUp)
	key(m, tea.KeyDown)
	if m.ta.Value() != "second" {
		t.Fatalf("down = %q", m.ta.Value())
	}
	key(m, tea.KeyDown)
	if m.ta.Value() != "" {
		t.Fatalf("draft not restored: %q", m.ta.Value())
	}
	for _, want := range []agent.Mode{agent.ModeAcceptEdits, agent.ModeReadOnly, agent.ModeOps} {
		key(m, tea.KeyShiftTab)
		if m.a.Policy.Mode != want {
			t.Fatalf("mode = %s, want %s", m.a.Policy.Mode, want)
		}
	}
	if !strings.Contains(m.footer(100), "? for shortcuts") {
		t.Fatal("footer")
	}
	if cmd := key(m, tea.KeyCtrlC); cmd != nil || m.hint == "" {
		t.Fatal("first ctrl+c should arm, not quit")
	}
	if cmd := key(m, tea.KeyCtrlC); cmd == nil {
		t.Fatal("second ctrl+c should quit")
	}
}

func TestEventRendering(t *testing.T) {
	m, _ := newTestModel(t)
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "bash", Args: map[string]any{"command": "docker ps"}})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "bash", Output: "validator0\nvalidator1"})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "edit", Output: "--- f\n- a = 1\n+ a = 2"})
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "mcp__grafana__query", Args: map[string]any{}})
	m.running = true
	if v := m.View(); !strings.Contains(v, "grafana · query (MCP)") || !strings.Contains(v, "Running…") {
		t.Errorf("running call not shown live:\n%s", v)
	}
	m.running = false
	m.onEvent(agent.Event{Kind: agent.EvTodos, Todos: []agent.Todo{{Content: "stop", Status: "completed"}, {Content: "swap", Status: "in_progress"}}})
	out := strings.Join(m.printed, "\n")
	for _, want := range []string{"Bash", "(docker ps)", "⎿", "validator1", "+ a = 2", "swap"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	m.running = true
	m.onEvent(agent.Event{Kind: agent.EvDelta, Text: "streaming…"})
	if !strings.Contains(m.View(), "streaming…") {
		t.Fatal("live text not in view")
	}
	m.onDone(doneMsg{err: context.Canceled})
	if !strings.Contains(m.printed[len(m.printed)-1], "Interrupted") || m.running {
		t.Fatal("interrupt not shown")
	}
	m.onDone(doneMsg{err: errors.New("groq: HTTP 429")})
	if !strings.Contains(m.printed[len(m.printed)-1], "429") {
		t.Fatal("error not shown")
	}
}

func TestShellCommandLocalAndSSH(t *testing.T) {
	m, dir := newTestModel(t)
	c := m.shellCmd("./upgrade/vote-upgrade.sh")
	if c.Dir != dir || c.Args[len(c.Args)-1] != "./upgrade/vote-upgrade.sh" {
		t.Fatalf("local cmd = %v in %s", c.Args, c.Dir)
	}
	m.a.Ctx.Profile = &config.Profile{Name: "val02", Transport: config.Transport{Type: "ssh", Host: "10.0.0.5", User: "ops", Port: 2222}}
	m.a.Tools.SetCwd("/srv/node")
	c = m.shellCmd("./vote.sh")
	got := strings.Join(c.Args, " ")
	if got != "ssh -t -p 2222 ops@10.0.0.5 cd '/srv/node' && ./vote.sh" {
		t.Fatalf("ssh cmd = %q", got)
	}
	m.onExecDone(execDoneMsg{cmd: "./vote.sh", code: 0})
	if !strings.Contains(m.printed[len(m.printed)-1], "the agent will know") {
		t.Fatal("exec result not shown")
	}
}

func TestCollapseThinkingAndExpand(t *testing.T) {
	m, _ := newTestModel(t)
	m.running = true
	m.onEvent(agent.Event{Kind: agent.EvThinking, Text: "first I should list the containers"})
	if !strings.Contains(m.View(), "Thinking…") {
		t.Fatalf("spinner should say Thinking…:\n%s", m.View())
	}
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "bash", Args: map[string]any{"command": "seq 1 10"}})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "bash", Output: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10"})
	out := strings.Join(m.printed, "\n")
	if !strings.Contains(out, "Thought for") || strings.Contains(out, "list the containers") {
		t.Fatalf("thinking not collapsed:\n%s", out)
	}
	if !strings.Contains(out, "… +7 lines") || strings.Contains(out, "\n     9") {
		t.Fatalf("output not collapsed:\n%s", out)
	}
	n := len(m.printed)
	key(m, tea.KeyCtrlR)
	if len(m.printed) == n || !strings.Contains(m.printed[len(m.printed)-1], "10") {
		t.Fatal("ctrl+r did not expand")
	}
	key(m, tea.KeyCtrlO)
	if !strings.Contains(m.printed[len(m.printed)-1], "list the containers") {
		t.Fatal("ctrl+o did not show thinking")
	}
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "read", Args: map[string]any{"file_path": "app.toml"}})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "read", Output: "a\nb\nc"})
	if !strings.Contains(m.printed[len(m.printed)-1], "Read") || !strings.Contains(m.printed[len(m.printed)-1], "3") {
		t.Fatalf("read summary: %q", m.printed[len(m.printed)-1])
	}
}
