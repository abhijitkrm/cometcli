package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/redact"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Agent runs the LLM ↔ tool loop over the shared registry.
type Agent struct {
	Provider Provider
	Model    string
	Reg      *toolkit.Registry
	Ctx      *toolkit.Context
	MaxIter  int

	// UI hooks — the REPL renders these.
	OnText       func(text string)                      // assistant text chunk
	OnToolCall   func(name string, args map[string]any) // before a tool runs
	OnToolResult func(name, summary string, err error)  // after a tool runs

	history []Msg
}

// New builds an agent for a context.
func New(c *toolkit.Context, reg *toolkit.Registry) (*Agent, error) {
	prov, err := NewProvider(c.Profile.Agent)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		Provider: prov,
		Model:    def(c.Profile.Agent.Model, ""),
		Reg:      reg,
		Ctx:      c,
		MaxIter:  16,
	}
	if a.Model == "" {
		// provider defaults already applied inside New(); read back
		a.Model = modelOf(prov)
	}
	return a, nil
}

func modelOf(p Provider) string {
	switch t := p.(type) {
	case *anthropic:
		return t.model
	case *openai:
		return t.model
	}
	return ""
}

// toolDefs converts the registry to provider tool schemas.
func (a *Agent) toolDefs() []ToolDef {
	var out []ToolDef
	for _, t := range a.Reg.All() {
		out = append(out, ToolDef{
			Name:   toolFnName(t.Name()),
			Desc:   fmt.Sprintf("[%s] %s", t.Tier(), t.Desc()),
			Schema: t.Schema(),
		})
	}
	return out
}

func toolFnName(n string) string { return strings.ReplaceAll(n, ".", "__") }

// Run processes one user turn, executing tools until the model finishes.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	a.history = append(a.history, Msg{Role: "user", Text: input})
	if a.Audit() != nil {
		_ = a.Audit().Log(audit.KindPrompt, a.Ctx.Profile.Name, map[string]any{"text": redact.Text(input)})
	}

	for i := 0; i < a.MaxIter; i++ {
		resp, err := a.Provider.Chat(ctx, &Request{
			Model:    a.Model,
			System:   a.system(),
			Messages: a.history,
			Tools:    a.toolDefs(),
			MaxTok:   4096,
		})
		if err != nil {
			return "", err
		}
		// record assistant turn
		a.history = append(a.history, Msg{Role: "assistant", Text: resp.Text, Calls: resp.Calls})
		if resp.Text != "" && a.OnText != nil {
			a.OnText(resp.Text)
		}
		if resp.Done || len(resp.Calls) == 0 {
			return resp.Text, nil
		}
		for _, call := range resp.Calls {
			result := a.execCall(ctx, call)
			a.history = append(a.history, result)
		}
	}
	return "", fmt.Errorf("agent exceeded %d iterations", a.MaxIter)
}

func (a *Agent) execCall(ctx context.Context, call Call) Msg {
	name := toolkit.ResolveName(call.Name)
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return Msg{Role: "tool", CallID: call.ID, ToolName: name,
			Text: "bad args: " + err.Error(), IsError: true}
	}
	if a.OnToolCall != nil {
		a.OnToolCall(name, args)
	}
	t, ok := a.Reg.Get(name)
	if !ok {
		return Msg{Role: "tool", CallID: call.ID, ToolName: name,
			Text: "no such tool: " + name, IsError: true}
	}
	runCtx := a.Ctx
	var cancel context.CancelFunc
	if !toolkit.IsLongRunning(t) {
		runCtx, cancel = toolkit.WithDeadline(a.Ctx, 90*time.Second)
	}
	res, err := t.Run(runCtx, args)
	if cancel != nil {
		runCtx.Close()
		cancel()
	}
	profile := a.Ctx.Profile.Name
	if a.Audit() != nil {
		var data map[string]any
		if res != nil {
			data = res.Data
		}
		a.Audit().Tool(profile, name, t.Tier().String(), redact.Args(args), data, err)
	}
	if err != nil {
		if a.OnToolResult != nil {
			a.OnToolResult(name, "", err)
		}
		return Msg{Role: "tool", CallID: call.ID, ToolName: name,
			Text: "error: " + err.Error(), IsError: true}
	}
	text := res.Text
	if text == "" && res.Data != nil {
		text = res.JSON()
	}
	if len(text) > 8192 { // bound context growth
		text = text[:8192] + "\n…[truncated]"
	}
	if a.OnToolResult != nil {
		a.OnToolResult(name, firstLine(text), nil)
	}
	return Msg{Role: "tool", CallID: call.ID, ToolName: name,
		Text: redact.Text(text)}
}

func (a *Agent) system() string {
	return SystemPrompt(a.Ctx) + SnapshotText(a.Ctx)
}

// Audit exposes the context audit logger.
func (a *Agent) Audit() *audit.Logger { return a.Ctx.Audit }

// Reset clears conversation history.
func (a *Agent) Reset() { a.history = nil }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
