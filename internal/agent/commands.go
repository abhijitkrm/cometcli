package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/runbook"
	"github.com/abhijitkrm/cometcli/internal/settings"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// ErrUnknownCommand means the slash command isn't a shared one; the front-
// end may handle it itself (/exit, /run, …) or report it.
var ErrUnknownCommand = errors.New("unknown command")

// CmdResult is the outcome of a shared slash command.
type CmdResult struct {
	// Text is shown to the operator.
	Text string
	// Prompt, when set, is submitted to the agent as a user turn — used by
	// /runbook <name> so the run goes through the normal approval gate and
	// streams like any other request.
	Prompt string
	// Job, when set, is slow work (a model call) the front-end runs off
	// its UI loop; its returned text is shown like Text.
	Job func(context.Context) (string, error)
}

// CommandHelp lists the shared slash commands; front-ends append their own.
const CommandHelp = `  /mode [ops|accept-edits|readonly|bypass]  show or set the approval posture
  /approve [local-change on|off] show or toggle autopilot (on-chain can never be autopiloted)
  /model [name | provider name] show or switch the LLM
  /runbook [name]               list runbooks, or have the agent run one
  /tools                        list tools with how this session treats each
  /one [profile|off]            switch to node mode for a profile, or back to general
  /profile                      active profile
  /audit                        audit log path + session id
  /reset, /clear                clear conversation, start a new audit session
  /status                       current setup: model, mode, scope, memory, MCP (+ live node snapshot in node mode)
  /compact [focus]              summarize the conversation to free context
  /cost                         token usage for this session
  /effort [low|medium|high|xhigh|max|default]  show or set reasoning depth
  /permissions                  show permission rules
  /allow|/ask|/deny <rule>      add a rule for this session, e.g. /allow bash(docker logs:*)
  /sessions                     list saved sessions
  /resume <id>                  load a saved session into this one`

// RunCommand executes a shared slash command. a may be nil when no LLM
// provider is configured — commands that need the agent say so.
func RunCommand(a *Agent, c *toolkit.Context, reg *toolkit.Registry, line string) (CmdResult, error) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return CmdResult{}, ErrUnknownCommand
	}
	needAgent := func() error {
		if a == nil {
			if Offline() {
				return ErrOffline
			}
			return fmt.Errorf("no agent provider — run `cometcli config set agent.provider <name>`")
		}
		return nil
	}
	switch f[0] {
	case "/help":
		return CmdResult{Text: HelpText(a)}, nil

	case "/clear":
		return RunCommand(a, c, reg, "/reset")

	case "/status":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		// the current setup; in node mode also
		// a live snapshot of the node (as a background job: never block)
		a.loadMemory()
		var b strings.Builder
		scope := "general (this machine)"
		if a.Node() {
			p := a.Ctx.Profile
			scope = fmt.Sprintf("node %s — %s, %s via %s", p.Name, p.ChainID, p.Role, orNone(p.Transport.Type))
		}
		fmt.Fprintf(&b, "scope:    %s\n", scope)
		fmt.Fprintf(&b, "model:    %s/%s", a.Provider.Name(), a.Model)
		if a.Effort != "" {
			fmt.Fprintf(&b, " · effort %s", a.Effort)
		}
		fmt.Fprintf(&b, "\nmode:     %s\n", strings.TrimPrefix(a.Policy.String(), "mode "))
		fmt.Fprintf(&b, "cwd:      %s\n", a.WorkRoot)
		fmt.Fprintf(&b, "session:  %s", a.ID())
		if pct := a.ContextPercent(); pct > 0 {
			fmt.Fprintf(&b, " · context %d%% used", pct)
		}
		var mem []string
		if a.ext != nil {
			for _, m := range a.ext.memory {
				mem = append(mem, m.Path)
			}
		}
		fmt.Fprintf(&b, "\nmemory:   %s\n", orNone(strings.Join(mem, ", ")))
		fmt.Fprintf(&b, "mcp:      %s", orNone(strings.Join(a.MCPServers(), ", ")))
		res := CmdResult{Text: b.String()}
		if a.Node() {
			res.Job = func(ctx context.Context) (string, error) {
				return strings.TrimSpace(a.Redact.Text(SnapshotText(a.Ctx))), nil
			}
		}
		return res, nil

	case "/profile":
		if c == nil || c.Profile == nil {
			return CmdResult{Text: "no active profile"}, nil
		}
		p := c.Profile
		txt := fmt.Sprintf("%s (role %s, chain %s, transport %s)", p.Name, p.Role, p.ChainID, p.Transport.Type)
		if a != nil {
			txt += fmt.Sprintf("\nagent: %s/%s · %s", a.Provider.Name(), a.Model, a.Policy)
		}
		return CmdResult{Text: txt}, nil

	case "/tools":
		var b strings.Builder
		for _, t := range reg.All() {
			dec := "—"
			if a != nil {
				dec = a.Policy.Decision(t.Tier())
			}
			fmt.Fprintf(&b, "%-22s %-13s %-8s %s\n", t.Name(), "["+t.Tier().String()+"]", dec, t.Desc())
		}
		return CmdResult{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "/audit":
		var lgPath, sess string
		if a != nil && a.Audit() != nil {
			lgPath, sess = a.Audit().Path(), a.Audit().Session()
		} else if c != nil && c.Audit != nil {
			lgPath = c.Audit.Path()
		}
		if lgPath == "" {
			return CmdResult{Text: "audit disabled"}, nil
		}
		txt := lgPath
		if sess != "" {
			txt += "\nsession " + sess + " — replay with: cometcli audit replay " + sess
		}
		return CmdResult{Text: txt}, nil

	case "/reset":
		if a != nil {
			a.Reset()
			return CmdResult{Text: "cleared — new session " + a.ID()}, nil
		}
		return CmdResult{Text: "cleared"}, nil

	case "/mode", "/safe":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if f[0] == "/safe" { // legacy toggle
			if a.Policy.ReadOnly() {
				a.Policy.Mode = ModeOps
			} else {
				a.Policy.Mode = ModeReadOnly
			}
			return CmdResult{Text: a.Policy.String()}, nil
		}
		if len(f) > 1 {
			m, err := ParseMode(f[1])
			if err != nil {
				return CmdResult{}, err
			}
			a.Policy.Mode = m
		}
		return CmdResult{Text: a.Policy.String()}, nil

	case "/approve":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if len(f) == 1 {
			return CmdResult{Text: a.Policy.String() + "\nusage: /approve local-change on|off"}, nil
		}
		on := true
		if len(f) > 2 {
			switch strings.ToLower(f[2]) {
			case "on", "true", "yes":
			case "off", "false", "no":
				on = false
			default:
				return CmdResult{}, fmt.Errorf("want on|off, got %q", f[2])
			}
		}
		if err := a.Policy.SetAutopilot(f[1], on); err != nil {
			return CmdResult{}, err
		}
		return CmdResult{Text: a.Policy.String()}, nil

	case "/model":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if len(f) == 1 {
			return CmdResult{Text: fmt.Sprintf("%s/%s", a.Provider.Name(), a.Model)}, nil
		}
		conf := a.conf
		if conf.Provider == "" {
			conf.Provider = a.Provider.Name()
		}
		if len(f) > 2 {
			if conf.Provider != f[1] {
				conf.BaseURL, conf.APIKeyEnv = "", "" // endpoint/key belong to the old provider
			}
			conf.Provider, conf.Model = f[1], f[2]
		} else {
			conf.Model = f[1]
		}
		p, err := NewProvider(conf)
		if err != nil {
			return CmdResult{}, err
		}
		a.Provider, a.Model, a.conf = p, modelOf(p), conf
		return CmdResult{Text: fmt.Sprintf("switched to %s/%s (conversation kept)", p.Name(), a.Model)}, nil

	case "/one", "/node":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if len(f) == 1 {
			txt := "general mode — shell and files on this machine; ask about a node by name and the agent switches to it (/one <profile> pins one)"
			if a.Node() {
				txt = "node mode: " + a.Ctx.Profile.Name + " — /one off for general mode"
			}
			if cfg := a.Ctx.Cfg; cfg != nil && len(cfg.Profiles) > 0 {
				var names []string
				for n := range cfg.Profiles {
					names = append(names, n)
				}
				sort.Strings(names)
				txt += "\nprofiles: " + strings.Join(names, ", ")
			}
			return CmdResult{Text: txt}, nil
		}
		if f[1] == "off" || f[1] == "general" {
			a.SwitchScope(nil)
			return CmdResult{Text: "general mode — node tools off; the shell runs on this machine"}, nil
		}
		if a.Ctx.Cfg == nil {
			return CmdResult{}, fmt.Errorf("no config loaded")
		}
		p, ok := a.Ctx.Cfg.Profiles[f[1]]
		if !ok {
			return CmdResult{}, fmt.Errorf("no profile %q — /one lists them", f[1])
		}
		p.Name = f[1]
		a.SwitchScope(p)
		return CmdResult{Text: fmt.Sprintf("node mode: %s (%s, %s via %s) — conversation kept", p.Name, p.Role, p.ChainID, orNone(p.Transport.Type))}, nil

	case "/compact":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		focus := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/compact"))
		return CmdResult{Text: "compacting…", Job: func(ctx context.Context) (string, error) {
			if err := a.Compact(ctx, focus); err != nil {
				return "", err
			}
			return "compacted — the summary is attached to your next message", nil
		}}, nil

	case "/cost", "/usage":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		u, last := a.Usage()
		txt := fmt.Sprintf("input %s (cached %s) · output %s", humanTok(u.Input), humanTok(u.CacheRead), humanTok(u.Output))
		if last > 0 {
			txt += fmt.Sprintf("\ncontext %s of %s — auto-compacts at %s", humanTok(last), humanTok(a.contextWindow()), humanTok(a.compactThreshold()))
		}
		return CmdResult{Text: txt}, nil

	case "/effort":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if len(f) > 1 {
			e := strings.ToLower(f[1])
			if e == "default" || e == "auto" {
				e = ""
			}
			if err := ValidEffort(e); err != nil {
				return CmdResult{}, err
			}
			a.Effort = e
		}
		if a.Effort == "" {
			return CmdResult{Text: "effort: model default"}, nil
		}
		return CmdResult{Text: "effort: " + a.Effort}, nil

	case "/permissions", "/rules":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		allow, ask, deny := a.Rules.Lists()
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n", a.Policy)
		for _, l := range []struct {
			name  string
			rules []string
		}{{"deny", deny}, {"ask", ask}, {"allow", allow}} {
			if len(l.rules) > 0 {
				fmt.Fprintf(&b, "%-5s %s\n", l.name, strings.Join(l.rules, ", "))
			}
		}
		b.WriteString("always: transactions ask; key material and state resets are refused")
		return CmdResult{Text: b.String()}, nil

	case "/allow", "/ask", "/deny":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		rule := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0]))
		if rule == "" {
			return CmdResult{}, fmt.Errorf("usage: %s <rule> — e.g. bash(systemctl status:*), edit(./config/**), val.unjail", f[0])
		}
		if a.Rules == nil {
			a.Rules, _ = toolkit.NewRules(nil, nil, nil)
		}
		if err := a.Rules.Add(f[0][1:], rule); err != nil {
			return CmdResult{}, err
		}
		return CmdResult{Text: fmt.Sprintf("%s %s (this session; add it to agent.permissions in the profile to keep it)", f[0][1:], rule)}, nil

	case "/sessions":
		all, err := ListSessions()
		if err != nil {
			return CmdResult{}, err
		}
		if len(all) == 0 {
			return CmdResult{Text: "no saved sessions"}, nil
		}
		var b strings.Builder
		for i, s := range all {
			if i == 20 {
				fmt.Fprintf(&b, "… %d more\n", len(all)-20)
				break
			}
			fmt.Fprintf(&b, "%s  %s  %-10s %s\n", s.ID, s.Updated.Local().Format("Jan 02 15:04"), s.Profile, s.Title)
		}
		b.WriteString("resume with /resume <id> (a prefix is enough)")
		return CmdResult{Text: b.String()}, nil

	case "/resume":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if len(f) < 2 {
			return RunCommand(a, c, reg, "/sessions")
		}
		sf, err := LoadSession(f[1])
		if err != nil {
			return CmdResult{}, err
		}
		a.Restore(sf)
		return CmdResult{Text: fmt.Sprintf("resumed %s — %s (%d messages)", sf.ID, sf.Title, len(sf.History))}, nil

	case "/memory":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		a.loadMemory()
		var b strings.Builder
		if a.ext == nil || len(a.ext.memory) == 0 {
			b.WriteString("no memory files loaded\n")
		}
		if a.ext != nil {
			for _, m := range a.ext.memory {
				fmt.Fprintf(&b, "%-8s %s (%d lines)\n", m.Scope, m.Path, strings.Count(m.Content, "\n")+1)
			}
		}
		b.WriteString("add with /remember <text> (project COMET.md), /remember --user|--node <text>")
		return CmdResult{Text: b.String()}, nil

	case "/remember", "/#":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0]))
		path, err := a.memoryTarget(&rest)
		if err != nil {
			return CmdResult{}, err
		}
		if rest == "" {
			return CmdResult{}, fmt.Errorf("usage: /remember [--user|--node] <text>")
		}
		if err := settings.AppendMemory(path, rest); err != nil {
			return CmdResult{}, err
		}
		// the system prompt is frozen: tell the model now, the file
		// carries it into future sessions
		a.carry = strings.TrimSpace(a.carry + "\n\n[Operator added to memory (" + path + "): " + a.Redact.Text(rest) + "]")
		return CmdResult{Text: "remembered in " + path}, nil

	case "/mcp":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		srv := a.MCPServers()
		if len(srv) == 0 {
			return CmdResult{Text: "no MCP servers connected — add one with `cometcli mcp add <name> -- <command> [args…]`"}, nil
		}
		return CmdResult{Text: "connected: " + strings.Join(srv, ", ") + "\ntheir tools load on demand (tool_search)"}, nil

	case "/agents":
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		if a.ext == nil || len(a.ext.Agents) == 0 {
			return CmdResult{Text: "only the general subagent — define more in .cometcli/agents/<name>.md (frontmatter: name, description, tools; body: its instructions)"}, nil
		}
		var b strings.Builder
		for _, d := range a.ext.Agents {
			fmt.Fprintf(&b, "%-16s %s (%s)\n", d.Name, d.Description, d.Path)
		}
		return CmdResult{Text: strings.TrimRight(b.String(), "\n")}, nil

	case "/runbook", "/runbooks":
		if len(f) == 1 {
			var b strings.Builder
			for _, rb := range runbook.All() {
				fmt.Fprintf(&b, "%-20s %s (%d steps)\n", rb.Name, rb.Desc, len(rb.Steps))
			}
			b.WriteString("run one with /runbook <name>")
			return CmdResult{Text: b.String()}, nil
		}
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		rb, err := runbook.GetAll(f[1])
		if err != nil {
			return CmdResult{}, err
		}
		if a.Policy.ReadOnly() {
			return CmdResult{
				Text:   "readonly mode — the agent will walk the read-only steps and report instead of executing the runbook",
				Prompt: fmt.Sprintf("Walk through runbook %q step by step using only read-only tools (runbook.show first), and report what executing it would do and whether it is safe right now.", rb.Name),
			}, nil
		}
		return CmdResult{
			Text:   fmt.Sprintf("running runbook %s — %s", rb.Name, rb.Desc),
			Prompt: fmt.Sprintf("Run runbook %q with runbook.run, then summarize each step's outcome and anything that needs my attention.", rb.Name),
		}, nil
	}
	name := strings.TrimPrefix(f[0], "/")
	args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0]))
	if a != nil && a.ext != nil {
		if cmd, ok := a.ext.Commands[name]; ok {
			return a.runCustomCommand(c, cmd, args)
		}
	}
	if cmd, ok := builtinCommands[name]; ok {
		if err := needAgent(); err != nil {
			return CmdResult{}, err
		}
		prompt, text := cmd.Expand(args, nil, nil), "/"+name
		if builtinNeedsNode[name] && !a.Node() {
			p, err := a.nodeFor(args)
			switch {
			case err != nil:
				return CmdResult{}, err
			case p != nil:
				// the operator named the node: work on it
				a.SwitchScope(p)
				text += " — working on " + p.Name
			default:
				// let the model pick the profile itself
				prompt = "This is about one of the operator's nodes: call use_node with the right profile first.\n\n" + prompt
			}
		}
		return CmdResult{Text: text, Prompt: prompt}, nil
	}
	return CmdResult{}, ErrUnknownCommand
}

// nodeFor finds the profile a node-only command is about: one named in
// its arguments, or nil when none is (the model then picks with use_node).
// It errors only when there are no profiles at all.
func (a *Agent) nodeFor(args string) (*config.Profile, error) {
	if a.Ctx == nil || a.Ctx.Cfg == nil || len(a.Ctx.Cfg.Profiles) == 0 {
		return nil, fmt.Errorf("no node profiles yet — add one with `cometcli init` or `cometcli profile add`")
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(args), func(r rune) bool {
		return r != '-' && r != '_' && r != '.' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		words[strings.Trim(w, ".")] = true
	}
	var names []string
	for n := range a.Ctx.Cfg.Profiles {
		if words[strings.ToLower(n)] {
			names = append(names, n)
		}
	}
	if len(names) != 1 { // none, or ambiguous: the model decides
		return nil, nil
	}
	p := a.Ctx.Cfg.Profiles[names[0]]
	p.Name = names[0]
	return p, nil
}

// memoryTarget picks the file /remember writes: --user, --node, or the
// project's COMET.md (the user file without a project).
func (a *Agent) memoryTarget(rest *string) (string, error) {
	switch {
	case strings.HasPrefix(*rest, "--user"):
		*rest = strings.TrimSpace(strings.TrimPrefix(*rest, "--user"))
		d, err := config.Dir()
		return filepath.Join(d, "COMET.md"), err
	case strings.HasPrefix(*rest, "--node"):
		*rest = strings.TrimSpace(strings.TrimPrefix(*rest, "--node"))
		if !a.Node() {
			return "", fmt.Errorf("--node needs node mode (/one <profile>)")
		}
		return settings.NodeMemoryPath(a.Ctx.Profile.Name)
	}
	if a.ext != nil && a.ext.Settings.Root != "" {
		return filepath.Join(a.ext.Settings.Root, "COMET.md"), nil
	}
	d, err := config.Dir()
	return filepath.Join(d, "COMET.md"), err
}

// humanTok renders a token count compactly: 950, 12.3k, 1.2M.
func humanTok(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
