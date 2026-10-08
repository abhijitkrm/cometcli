package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijitkrm/cometcli/internal/settings"
)

func runner(t *testing.T, event, matcher, script string) *Runner {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hook.sh")
	os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	return &Runner{Dir: dir, Matchers: map[string][]settings.HookMatcher{
		event: {{Matcher: matcher, Hooks: []settings.HookCommand{{Type: "command", Command: p, Timeout: 5}}}},
	}}
}

func TestExitCodes(t *testing.T) {
	ctx := context.Background()
	out := runner(t, PreToolUse, "bash", `echo "no rm here" >&2; exit 2`).Run(ctx, Input{Event: PreToolUse, ToolName: "bash"})
	if !out.Block || out.Reason != "no rm here" {
		t.Fatalf("exit 2: %+v", out)
	}
	out = runner(t, PreToolUse, "bash", `exit 1`).Run(ctx, Input{Event: PreToolUse, ToolName: "bash"})
	if out.Block || len(out.Messages) != 1 {
		t.Fatalf("exit 1 should be non-blocking: %+v", out)
	}
	// matcher doesn't fit → not run
	out = runner(t, PreToolUse, "edit|write", `exit 2`).Run(ctx, Input{Event: PreToolUse, ToolName: "bash"})
	if out.Block {
		t.Fatal("hook ran for a non-matching tool")
	}
	out = runner(t, PreToolUse, "Bash", `exit 2`).Run(ctx, Input{Event: PreToolUse, ToolName: "bash"})
	if !out.Block {
		t.Fatal("matcher should be case-insensitive (Bash)")
	}
}

func TestStdinPayloadAndJSONDecisions(t *testing.T) {
	ctx := context.Background()
	r := runner(t, PreToolUse, "*", `in=$(cat); case "$in" in *'"command":"rm -rf x"'*) echo '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"rm blocked"}}';; *) echo '{"hookSpecificOutput":{"permissionDecision":"allow"}}';; esac`)
	out := r.Run(ctx, Input{Event: PreToolUse, ToolName: "bash", ToolInput: map[string]any{"command": "rm -rf x"}})
	if out.Decision != "deny" || !strings.Contains(out.Reason, "rm blocked") {
		t.Fatalf("deny: %+v", out)
	}
	out = r.Run(ctx, Input{Event: PreToolUse, ToolName: "bash", ToolInput: map[string]any{"command": "ls"}})
	if out.Decision != "allow" || out.Block {
		t.Fatalf("allow: %+v", out)
	}
	out = runner(t, UserPromptSubmit, "", `echo "on-call: alice"`).Run(ctx, Input{Event: UserPromptSubmit, Prompt: "hi"})
	if out.Context != "on-call: alice" {
		t.Fatalf("context: %+v", out)
	}
	out = runner(t, Stop, "", `echo '{"decision":"block","reason":"run the tests first"}'`).Run(ctx, Input{Event: Stop})
	if !out.Block || out.Reason != "run the tests first" {
		t.Fatalf("stop block: %+v", out)
	}
	out = runner(t, PostToolUse, "", `echo $COMETCLI_PROJECT_DIR > /dev/null; echo '{"systemMessage":"lint ok"}'`).Run(ctx, Input{Event: PostToolUse, ToolName: "edit"})
	if len(out.Messages) != 1 || out.Messages[0] != "lint ok" {
		t.Fatalf("systemMessage: %+v", out)
	}
}

func TestTimeout(t *testing.T) {
	r := runner(t, PreToolUse, "", `sleep 10`)
	r.Matchers[PreToolUse][0].Hooks[0].Timeout = 1
	out := r.Run(context.Background(), Input{Event: PreToolUse, ToolName: "bash"})
	if out.Block || len(out.Messages) != 1 || !strings.Contains(out.Messages[0], "timed out") {
		t.Fatalf("timeout: %+v", out)
	}
}
