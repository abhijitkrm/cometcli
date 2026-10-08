// Package hooks runs operator-defined commands at points in the agent
// loop, with the common agent-hook contract: the event arrives as JSON on stdin;
// exit 0 passes (stdout may carry JSON decisions or extra context); exit
// 2 blocks, with stderr as the reason; any other exit is a non-blocking
// error shown to the operator.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/settings"
)

// Hook events.
const (
	PreToolUse       = "PreToolUse"
	PostToolUse      = "PostToolUse"
	UserPromptSubmit = "UserPromptSubmit"
	Stop             = "Stop"
	SessionStart     = "SessionStart"
)

// Events lists the supported events.
var Events = []string{PreToolUse, PostToolUse, UserPromptSubmit, Stop, SessionStart}

// Runner executes the hooks configured for a session.
type Runner struct {
	Matchers map[string][]settings.HookMatcher
	// Dir is where hooks run and what COMETCLI_PROJECT_DIR points at.
	Dir string
}

// Input is the JSON document a hook reads on stdin.
type Input struct {
	SessionID      string         `json:"session_id"`
	TranscriptPath string         `json:"transcript_path,omitempty"`
	Cwd            string         `json:"cwd"`
	Event          string         `json:"hook_event_name"`
	PermissionMode string         `json:"permission_mode,omitempty"`
	ToolName       string         `json:"tool_name,omitempty"`
	ToolInput      map[string]any `json:"tool_input,omitempty"`
	ToolResponse   map[string]any `json:"tool_response,omitempty"`
	Prompt         string         `json:"prompt,omitempty"`
	StopHookActive bool           `json:"stop_hook_active,omitempty"`
	Source         string         `json:"source,omitempty"`
}

// Outcome is what the hooks for one event decided, combined.
type Outcome struct {
	// Block stops the action (tool call, prompt) or, for Stop, makes the
	// agent continue; Reason says why (fed to the model for tool events).
	Block  bool
	Reason string
	// Decision is a PreToolUse permission decision: allow | deny | ask.
	Decision string
	// Context is extra text for the model (UserPromptSubmit, SessionStart
	// stdout; additionalContext).
	Context string
	// Messages are shown to the operator (non-blocking errors, systemMessage).
	Messages []string
}

// Has reports whether any hook is configured for the event.
func (r *Runner) Has(event string) bool {
	return r != nil && len(r.Matchers[event]) > 0
}

// Run executes every hook for event whose matcher fits in.ToolName.
// Decisions combine conservatively: deny > ask > allow, any block blocks.
func (r *Runner) Run(ctx context.Context, in Input) Outcome {
	var out Outcome
	if !r.Has(in.Event) {
		return out
	}
	in.Cwd = def(in.Cwd, r.Dir)
	payload, _ := json.Marshal(in)
	var reasons, contexts []string
	for _, m := range r.Matchers[in.Event] {
		if !matches(m.Matcher, in.ToolName) {
			continue
		}
		for _, h := range m.Hooks {
			if h.Type != "" && h.Type != "command" || strings.TrimSpace(h.Command) == "" {
				continue
			}
			res := r.exec(ctx, h, payload)
			switch {
			case res.err != nil && res.code < 0:
				out.Messages = append(out.Messages, fmt.Sprintf("%s hook %q failed: %v", in.Event, h.Command, res.err))
			case res.code == 2:
				out.Block = true
				reasons = append(reasons, firstNonEmpty(strings.TrimSpace(res.stderr), "blocked by "+in.Event+" hook"))
			case res.code != 0:
				msg := strings.TrimSpace(res.stderr)
				out.Messages = append(out.Messages, fmt.Sprintf("%s hook %q exited %d: %s", in.Event, h.Command, res.code, msg))
			default:
				r.parseStdout(in.Event, res.stdout, &out, &reasons, &contexts)
			}
		}
	}
	out.Reason = strings.Join(reasons, "\n")
	out.Context = strings.Join(contexts, "\n")
	return out
}

func (r *Runner) parseStdout(event, stdout string, out *Outcome, reasons, contexts *[]string) {
	s := strings.TrimSpace(stdout)
	if s == "" {
		return
	}
	if !strings.HasPrefix(s, "{") {
		if event == UserPromptSubmit || event == SessionStart {
			*contexts = append(*contexts, s)
		}
		return
	}
	var j struct {
		Continue      *bool  `json:"continue"`
		StopReason    string `json:"stopReason"`
		Decision      string `json:"decision"`
		Reason        string `json:"reason"`
		SystemMessage string `json:"systemMessage"`
		Specific      struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
			AdditionalContext        string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(s), &j) != nil {
		if event == UserPromptSubmit || event == SessionStart {
			*contexts = append(*contexts, s)
		}
		return
	}
	if j.SystemMessage != "" {
		out.Messages = append(out.Messages, j.SystemMessage)
	}
	if j.Continue != nil && !*j.Continue {
		out.Block = true
		*reasons = append(*reasons, firstNonEmpty(j.StopReason, j.Reason, "stopped by hook"))
	}
	switch j.Decision {
	case "block":
		out.Block = true
		*reasons = append(*reasons, firstNonEmpty(j.Reason, "blocked by hook"))
	case "approve":
		out.Decision = stricter(out.Decision, "allow")
	}
	if d := j.Specific.PermissionDecision; d == "allow" || d == "deny" || d == "ask" {
		out.Decision = stricter(out.Decision, d)
		if d == "deny" {
			*reasons = append(*reasons, firstNonEmpty(j.Specific.PermissionDecisionReason, "denied by PreToolUse hook"))
		}
	}
	if j.Specific.AdditionalContext != "" {
		*contexts = append(*contexts, j.Specific.AdditionalContext)
	}
}

// stricter keeps the more restrictive permission decision.
func stricter(a, b string) string {
	rank := map[string]int{"": 0, "allow": 1, "ask": 2, "deny": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

type execResult struct {
	code           int
	stdout, stderr string
	err            error
}

const maxHookOutput = 64 << 10

func (r *Runner) exec(ctx context.Context, h settings.HookCommand, payload []byte) execResult {
	to := time.Duration(h.Timeout) * time.Second
	if to <= 0 {
		to = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", h.Command)
	c.Dir = r.Dir
	c.Env = append(os.Environ(), "COMETCLI_PROJECT_DIR="+r.Dir, "CLAUDE_PROJECT_DIR="+r.Dir)
	c.Stdin = bytes.NewReader(payload)
	isolate(c)
	c.WaitDelay = time.Second // don't wait on pipes a killed hook left open
	var so, se bytes.Buffer
	c.Stdout, c.Stderr = &so, &se
	err := c.Run()
	res := execResult{stdout: trim(so.String()), stderr: trim(se.String())}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.code, res.err = -1, fmt.Errorf("timed out after %s", to)
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
	default:
		res.code, res.err = -1, err
	}
	return res
}

func matches(matcher, tool string) bool {
	m := strings.TrimSpace(matcher)
	if m == "" || m == "*" || tool == "" {
		return true
	}
	re, err := regexp.Compile("(?i)^(?:" + m + ")$")
	if err != nil {
		return strings.EqualFold(m, tool)
	}
	return re.MatchString(tool)
}

func trim(s string) string {
	if len(s) > maxHookOutput {
		return s[:maxHookOutput] + "…[truncated]"
	}
	return s
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
