package mcpclient

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// AgentTool adapts one MCP tool to the registry. Its name is
// mcp__<server>__<tool>; read-only tools (readOnlyHint) run without a
// prompt, everything else asks, and rules like "mcp__github" or
// "mcp__github__create_issue" apply.
type AgentTool struct {
	Client *Client
	Tool   Tool
	name   string
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ToolName builds the agent-facing name for a server tool.
func ToolName(server, tool string) string {
	n := "mcp__" + unsafeName.ReplaceAllString(server, "_") + "__" + unsafeName.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// NewAgentTool wraps a server tool.
func NewAgentTool(c *Client, t Tool) *AgentTool {
	return &AgentTool{Client: c, Tool: t, name: ToolName(c.Name, t.Name)}
}

func (t *AgentTool) Name() string     { return t.name }
func (t *AgentTool) AgentOnly() bool  { return true }
func (t *AgentTool) OutputLimit() int { return 30_000 }
func (t *AgentTool) Timeout(toolkit.Args) time.Duration {
	return 5*time.Minute + 15*time.Minute // call + approval
}

func (t *AgentTool) Desc() string {
	d := strings.TrimSpace(t.Tool.Description)
	if i := strings.Index(d, "\n\n"); i > 0 && len(d) > 600 {
		d = d[:i]
	}
	if len(d) > 600 {
		d = d[:600] + "…"
	}
	return fmt.Sprintf("(MCP server %s) %s", t.Client.Name, d)
}

// Schema normalizes the server's input schema for strict validators.
func (t *AgentTool) Schema() map[string]any {
	s := map[string]any{}
	for k, v := range t.Tool.InputSchema {
		s[k] = v
	}
	s["type"] = "object"
	if p, ok := s["properties"].(map[string]any); !ok || p == nil {
		s["properties"] = map[string]any{}
	}
	if r, ok := s["required"].([]any); ok && len(r) == 0 {
		delete(s, "required")
	}
	if s["required"] == nil {
		delete(s, "required")
	}
	return s
}

func (t *AgentTool) Tier() toolkit.Tier {
	if t.Tool.Annotations.ReadOnlyHint && !t.Tool.Annotations.DestructiveHint {
		return toolkit.TierDiagnose
	}
	return toolkit.TierLocalChange
}

func (t *AgentTool) Run(c *toolkit.Context, a toolkit.Args) (*toolkit.Result, error) {
	if err := c.Check(toolkit.Gate{
		Request: toolkit.Request{Tool: t.name},
		Tier:    t.Tier(),
		Prompt:  fmt.Sprintf("call %s on MCP server %s", t.Tool.Name, t.Client.Name),
		Detail:  map[string]any{"arguments": fmt.Sprint(map[string]any(a))},
	}); err != nil {
		return nil, err
	}
	res, err := t.Client.CallTool(c, t.Tool.Name, a)
	if err != nil {
		return nil, err
	}
	text := res.Text()
	if res.IsError {
		return nil, fmt.Errorf("%s: %s", t.Tool.Name, text)
	}
	if text == "" {
		text = "(no output)"
	}
	return &toolkit.Result{Text: text}, nil
}
