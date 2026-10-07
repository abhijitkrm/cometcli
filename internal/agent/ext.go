package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abhijitkrm/cometcli/internal/client/host"
	"github.com/abhijitkrm/cometcli/internal/hooks"
	"github.com/abhijitkrm/cometcli/internal/mcpclient"
	"github.com/abhijitkrm/cometcli/internal/settings"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/shell"
)

// ext holds what settings files contribute to a session.
type ext struct {
	Settings *settings.Settings
	Hooks    *hooks.Runner
	Commands map[string]*settings.Command
	Agents   map[string]*settings.AgentDef
	mcp      []*mcpclient.Client
	notes    []string // startup notices (MCP failures, untrusted items)
	memory   []settings.Memory
	resumed  bool     // SessionStart source
	turnRule []string // allow rules granted for the current turn only
}

// loadExtensions reads settings, memory and commands for cwd, connects
// MCP servers (registering their tools) and builds the hook runner.
// Settings env vars are exported before this returns, so the provider
// sees API keys set there.
func loadExtensions(cwd string, reg *toolkit.Registry) (*ext, error) {
	st, err := settings.Load(cwd)
	if err != nil {
		return nil, err
	}
	st.ApplyEnv()
	dir := st.Root
	if dir == "" {
		dir = cwd
	}
	e := &ext{
		Settings: st,
		Hooks:    &hooks.Runner{Matchers: st.Hooks, Dir: dir},
		Commands: settings.LoadCommands(st.Root),
		Agents:   settings.LoadAgents(st.Root),
	}
	if len(st.Ignored) > 0 {
		e.notes = append(e.notes, fmt.Sprintf("project settings in %s are not trusted, so these were skipped: %s — run `cometcli trust` there to enable them",
			st.Root, strings.Join(st.Ignored, ", ")))
	}
	e.connectMCP(reg)
	return e, nil
}

func (e *ext) connectMCP(reg *toolkit.Registry) {
	names := make([]string, 0, len(e.Settings.MCPServers))
	for n := range e.Settings.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string, cfg settings.MCPServer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c, err := mcpclient.Connect(ctx, n, cfg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				e.notes = append(e.notes, err.Error())
				return
			}
			e.mcp = append(e.mcp, c)
		}(n, e.Settings.MCPServers[n])
	}
	wg.Wait()
	for _, c := range e.mcp {
		for _, t := range c.Tools {
			reg.Upsert(mcpclient.NewAgentTool(c, t))
		}
	}
}

// close stops MCP servers.
func (e *ext) close(reg *toolkit.Registry) {
	if e == nil {
		return
	}
	for _, c := range e.mcp {
		for _, t := range c.Tools {
			reg.Remove(mcpclient.ToolName(c.Name, t.Name))
		}
		c.Close()
	}
	e.mcp = nil
}

// isMCP reports whether t came from an MCP server.
func isMCP(t toolkit.Tool) bool { return strings.HasPrefix(t.Name(), "mcp__") }

// MCPServers lists connected servers with their tool counts.
func (a *Agent) MCPServers() []string {
	if a.ext == nil {
		return nil
	}
	var out []string
	for _, c := range a.ext.mcp {
		out = append(out, fmt.Sprintf("%s (%d tools)", c.Name, len(c.Tools)))
	}
	return out
}

// memoryText loads COMET.md memory for the current scope.
func (a *Agent) memoryText() string {
	if a.ext == nil {
		return ""
	}
	cwd := a.WorkRoot
	profile := ""
	if a.Node() {
		profile = a.Ctx.Profile.Name
	}
	a.ext.memory = settings.LoadMemory(a.ext.Settings.Root, cwd, profile)
	return a.Redact.Text(settings.RenderMemory(a.ext.memory))
}

// --- hooks -----------------------------------------------------------------

func (a *Agent) hookInput(event string) hooks.Input {
	in := hooks.Input{SessionID: a.ID(), Cwd: a.Tools.Cwd(a.WorkRoot), Event: event, PermissionMode: string(a.Policy.Mode)}
	if p, err := sessionPath(a.ID()); err == nil {
		in.TranscriptPath = p
	}
	return in
}

// runHook runs an event's hooks and surfaces their operator messages.
func (a *Agent) runHook(ctx context.Context, in hooks.Input) hooks.Outcome {
	if a.ext == nil || !a.ext.Hooks.Has(in.Event) {
		return hooks.Outcome{}
	}
	out := a.ext.Hooks.Run(ctx, in)
	for _, m := range out.Messages {
		a.emit(Event{Kind: EvNotice, Text: a.Redact.Text(m)})
	}
	if lg := a.Audit(); lg != nil && (out.Block || out.Decision != "") {
		_ = lg.Log("hook", a.profileName(), map[string]any{"event": in.Event, "tool": in.ToolName, "block": out.Block, "decision": out.Decision, "reason": a.Redact.Text(out.Reason)})
	}
	return out
}

// --- custom slash commands ---------------------------------------------------

// runCustomCommand expands a markdown command into a prompt.
func (a *Agent) runCustomCommand(c *toolkit.Context, cmd *settings.Command, args string) (CmdResult, error) {
	h, herr := a.Ctx.Host()
	cwd := a.Tools.Cwd(a.WorkRoot)
	run := func(command string) string {
		if herr != nil {
			return "[" + herr.Error() + "]"
		}
		binary := ""
		if a.Node() {
			binary = a.Ctx.Profile.Binary
		}
		v := shell.Classify(command, shell.Opts{Binary: binary})
		if v.Forbidden != "" || v.Tier > toolkit.TierDiagnose {
			return fmt.Sprintf("[!`%s` not run: only read-only commands expand inline — the agent can run it with approval]", command)
		}
		ctx, cancel := context.WithTimeout(c, 30*time.Second)
		defer cancel()
		script := command
		if cwd != "" {
			script = "cd '" + strings.ReplaceAll(cwd, "'", `'\''`) + "' && " + command
		}
		res, err := host.Exec(ctx, h, script, 16_000)
		if err != nil {
			return "[" + err.Error() + "]"
		}
		return strings.TrimRight(res.Output, "\n")
	}
	read := func(p string) (string, bool) {
		if strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, p[2:])
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if _, local := h.(*host.Local); !local || shell.Classify("cat "+p, shell.Opts{}).Forbidden != "" {
			return "", false
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 100_000 {
			return "", false
		}
		return string(b), true
	}
	prompt := a.Redact.Text(cmd.Expand(args, run, read))
	if prompt == "" {
		return CmdResult{}, fmt.Errorf("/%s expanded to nothing", cmd.Name)
	}
	text := fmt.Sprintf("/%s (%s)", cmd.Name, cmd.Scope)
	if len(cmd.AllowedTools) > 0 {
		if cmd.Scope == "project" && !a.ext.Settings.Trusted {
			text += " — allowed-tools ignored: project not trusted (`cometcli trust`)"
		} else {
			a.ext.turnRule = append(a.ext.turnRule, cmd.AllowedTools...)
			text += " — allowing for this turn: " + strings.Join(cmd.AllowedTools, ", ")
		}
	}
	return CmdResult{Text: text, Prompt: prompt}, nil
}

// grantTurnRules applies a command's allowed-tools for one turn and
// returns the cleanup.
func (a *Agent) grantTurnRules() func() {
	if a.ext == nil || len(a.ext.turnRule) == 0 {
		return func() {}
	}
	rules := a.ext.turnRule
	a.ext.turnRule = nil
	if a.Rules == nil {
		a.Rules, _ = toolkit.NewRules(nil, nil, nil)
	}
	var added []string
	for _, r := range rules {
		if a.Rules.Add("allow", r) == nil {
			added = append(added, r)
		}
	}
	return func() {
		for _, r := range added {
			a.Rules.Remove("allow", r)
		}
	}
}

// HelpText is the slash-command help, including custom commands.
func HelpText(a *Agent) string {
	s := CommandHelp + "\n  /memory, /remember <text>     show memory files; add a line to COMET.md\n  /mcp                          connected MCP servers\n  /agents                       subagents available to the task tool"
	cmds := map[string]*settings.Command{}
	for n, c := range builtinCommands {
		cmds[n] = c
	}
	if a != nil && a.ext != nil {
		for n, c := range a.ext.Commands {
			cmds[n] = c
		}
	}
	var b strings.Builder
	b.WriteString(s + "\nprocedures and custom commands:")
	for _, c := range settings.SortedCommands(cmds) {
		name := "/" + c.Name
		if c.ArgHint != "" {
			name += " " + c.ArgHint
		}
		fmt.Fprintf(&b, "\n  %-29s %s (%s)", name, c.Description, c.Scope)
	}
	return b.String()
}

// --- subagents ---------------------------------------------------------------

const taskToolName = "task"

func (a *Agent) taskToolDef() ToolDef {
	desc := "Delegate a self-contained investigation to a subagent with its own fresh context — log trawls, multi-file reads, comparing hosts. It has the same tools and permissions, works through the task, and returns only its final report, keeping your context small. Give it everything it needs in prompt."
	props := map[string]any{
		"description": toolkit.Str("3-5 word label shown to the operator"),
		"prompt":      toolkit.Str("the complete task: goal, where to look, what to report"),
	}
	if a.ext != nil && len(a.ext.Agents) > 0 {
		var names []string
		var lines []string
		for _, d := range a.ext.Agents {
			names = append(names, d.Name)
			lines = append(lines, d.Name+": "+d.Description)
		}
		sort.Strings(names)
		sort.Strings(lines)
		props["agent"] = toolkit.Enum("a specialized subagent (optional): "+strings.Join(lines, "; "), names...)
	}
	return ToolDef{Name: taskToolName, Desc: desc, Schema: toolkit.ObjSchema(props, "description", "prompt")}
}

// runTask runs a subagent to completion and returns its report.
func (a *Agent) runTask(ctx context.Context, call Call, args map[string]any) Msg {
	prompt, _ := args["prompt"].(string)
	label, _ := args["description"].(string)
	if strings.TrimSpace(prompt) == "" {
		return a.toolErr(call, taskToolName, "prompt is required")
	}
	var def *settings.AgentDef
	if n, _ := args["agent"].(string); n != "" && a.ext != nil {
		if def = a.ext.Agents[n]; def == nil {
			return a.toolErr(call, taskToolName, "no subagent "+n)
		}
	}
	child := &Agent{
		Provider: a.Provider, Model: a.Model, Reg: a.Reg, Ctx: a.Ctx,
		MaxIter: 25, Policy: a.Policy, Redact: a.Redact, Stream: a.Stream,
		SnapshotFn: a.SnapshotFn, MaxTokens: a.MaxTokens, Effort: a.Effort,
		Rules: a.Rules, Tools: toolkit.NewSession(a.Tools.Cwd("")), WorkRoot: a.WorkRoot,
		conf: a.conf, ext: a.ext, parent: a, def: def, id: a.ID(), audit: a.audit,
	}
	child.OnEvent = func(e Event) {
		switch e.Kind {
		case EvToolStart, EvToolResult, EvNotice:
			e.Tool = "↳ " + e.Tool
			a.emit(e)
		}
	}
	a.emit(Event{Kind: EvToolStart, Tool: taskToolName, Args: map[string]any{"description": label}})
	out, err := child.Run(ctx, prompt)
	a.total.Add(child.total)
	if err != nil {
		return a.toolErr(call, taskToolName, "subagent failed: "+err.Error())
	}
	a.emit(Event{Kind: EvToolResult, Tool: taskToolName, Text: firstLine(out)})
	return Msg{Role: "tool", CallID: call.ID, ToolName: taskToolName, Text: clip(out, 30_000)}
}

// subagentPrompt is appended to a subagent's system prompt.
func (a *Agent) subagentPrompt() string {
	s := "\nYOU ARE A SUBAGENT: another agent delegated one task to you. Work through it with your tools, then reply with a concise, complete report — findings with exact evidence, what you changed, what remains. Only your final message is returned.\n"
	if a.def != nil && a.def.Prompt != "" {
		s += "\n" + a.def.Prompt + "\n"
	}
	return s
}

// allowedByDef restricts a custom subagent to its declared tools.
func (a *Agent) allowedByDef(name string) bool {
	if a.def == nil || len(a.def.Tools) == 0 {
		return true
	}
	for _, t := range a.def.Tools {
		if r, err := toolkit.ParseRule(t); err == nil && strings.EqualFold(r.Tool, name) {
			return true
		}
		if strings.HasSuffix(t, "*") && strings.HasPrefix(name, strings.TrimSuffix(t, "*")) {
			return true
		}
	}
	return false
}
