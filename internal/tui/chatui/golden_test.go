package chatui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometcli/internal/agent"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

var update = flag.Bool("update", false, "rewrite golden files")

// golden compares a rendered screen (ANSI stripped, trailing spaces
// trimmed) with testdata/<name>.golden.
func golden(t *testing.T, name, got string) {
	t.Helper()
	got = ansiRe.ReplaceAllString(got, "")
	lines := strings.Split(got, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	got = strings.Join(lines, "\n")
	p := filepath.Join("testdata", name+".golden")
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(p, []byte(got), 0o644)
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v — run go test -update", err)
	}
	if string(want) != got {
		t.Errorf("%s changed (go test -update to accept):\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func goldenModel(t *testing.T) *model {
	m, _ := newTestModel(t)
	m.width = 80
	m.ta.SetWidth(74)
	return m
}

func TestGoldenScreens(t *testing.T) {
	m := goldenModel(t)
	golden(t, "idle", m.View())

	typeText(m, "/c")
	golden(t, "slash_menu", m.View())
	m.ta.Reset()
	m.sugg = nil

	m.approval = &pendingApproval{prompt: "run on local: docker compose up -d", tier: toolkit.TierLocalChange,
		detail: map[string]any{"command": "docker compose up -d", "description": "start the validator stack", "host": "local",
			"why": "changes containers (docker compose up)", toolkit.RuleHint: "bash(docker compose:*)"}}
	golden(t, "approval_bash", m.View())

	m.approval = &pendingApproval{prompt: `broadcast transaction
{"chain_id": "primium-1", "messages": ["/cosmos.gov.v1.MsgVote {proposalId:3 option:VOTE_OPTION_YES}"], "fee": "140000000000000adex"}`,
		tier: toolkit.TierOnChain, detail: map[string]any{"action": "gov-vote", toolkit.RuleHint: "val.vote"}}
	golden(t, "approval_tx", m.View())
	m.approval = nil

	m.running, m.started = true, time.Now()
	m.onEvent(agent.Event{Kind: agent.EvDelta, Text: "The validator missed 12 blocks in the last window; peers are"})
	golden(t, "running", m.View())
	m.running = false
	m.live.Reset()

	m.a.Policy.Mode = agent.ModeAcceptEdits
	golden(t, "accept_edits_footer", m.footer(80))
}

func TestGoldenTranscript(t *testing.T) {
	m := goldenModel(t)
	m.printed = nil
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "bash", Args: map[string]any{"command": "docker ps --format '{{.Names}}'"}})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "bash", Output: "primium-validator0\nprimium-validator1"})
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "edit", Args: map[string]any{"file_path": "config/app.toml"}})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "edit", Output: "--- config/app.toml\n  [api]\n- swagger = true\n+ swagger = false"})
	m.onEvent(agent.Event{Kind: agent.EvToolStart, Tool: "↳ grep", Args: map[string]any{"pattern": "panic", "path": "/var/log"}})
	m.onEvent(agent.Event{Kind: agent.EvToolResult, Tool: "↳ grep", Err: "no matches for panic under /var/log"})
	m.onEvent(agent.Event{Kind: agent.EvTodos, Todos: []agent.Todo{{Content: "stop node", Status: "completed"}, {Content: "swap binary", Status: "in_progress"}, {Content: "start node", Status: "pending"}}})
	m.onEvent(agent.Event{Kind: agent.EvText, Text: "Both validators are **up**.\n\n- validator0: height 162248\n- validator1: height 162248"})
	golden(t, "transcript", strings.Join(m.printed, "\n"))
}
