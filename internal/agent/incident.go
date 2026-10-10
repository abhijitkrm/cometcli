package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/egress"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/networktool"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
	"gopkg.in/yaml.v3"
)

// incidentMarker prefixes an /incident that cometcli works itself first.
const incidentMarker = "\x1fincident\x1f"

// runIncident works /incident without the model when it can: triage runs
// locally, and when one known case is clearly the root cause and has a
// playbook, cometcli runs it — every change and transaction still asks
// the operator. Anything else goes to the model with the triage attached,
// so it starts from the evidence instead of collecting it again.
func (a *Agent) runIncident(ctx context.Context, what string) (string, error) {
	procedure := builtinCommands["incident"].Expand(what, nil, nil)
	var learnFor *kb.Case // a known case without a playbook: learn one from the model's fix
	handoff := func(context string) (string, error) {
		prompt := procedure
		if context != "" {
			prompt += "\n\n" + context
		}
		start := len(a.history)
		out, err := a.run(ctx, forceModel+prompt)
		if err == nil && learnFor != nil {
			a.offerPlaybook(learnFor, start)
		}
		return out, err
	}
	t, ok := a.Reg.Get("node.triage")
	if !ok {
		return handoff("")
	}
	res, err := a.runTool(ctx, t, toolkit.Args{}, "/incident: triage")
	if err != nil {
		return handoff("")
	}
	triageNote := "[cometcli already ran node.triage for this incident — start from it; run it again only after something changed]\n" +
		egress.ForModel(a.egressMode(), "node.triage", res.Text)
	sig, _ := res.Data["signals"].(kb.Signals)
	roots, _ := res.Data["roots"].([]string)
	base := triage.LoadKB(a.Ctx)
	if len(roots) == 0 {
		return handoff(triageNote)
	}
	c := base.Cases[roots[0]]
	switch {
	case c == nil:
		return handoff(triageNote)
	case len(c.Playbook) == 0:
		learnFor = c
		return handoff(triageNote)
	case len(roots) > 1 && base.Cases[roots[1]] != nil && base.Cases[roots[1]].Severity == c.Severity:
		// two equally serious root causes: that's a judgement call
		return handoff(triageNote)
	}

	a.emit(Event{Kind: EvText, Text: fmt.Sprintf("Known case **%s** — %s.\nRunning its playbook locally, no model call; changes and transactions still ask you.", c.ID, c.Title)})
	var done []string
	for i, st := range c.Playbook {
		label := st.Do
		if st.Note != "" {
			label += " — " + st.Note
		}
		if st.When != "" {
			// conditions are about now, not about the triage minutes ago:
			// a halted node gets jailed while the playbook runs
			a.refreshSignals(ctx, sig, st.When)
		}
		if !st.Applies(sig) {
			done = append(done, fmt.Sprintf("skipped %s (%s doesn't hold)", st.Do, st.When))
			continue
		}
		tool, ok := a.Reg.Get(st.Do)
		if !ok {
			return handoff(triageNote + "\n\n" + playbookNote(c, done, fmt.Sprintf("step %d: no tool %s", i+1, st.Do)))
		}
		why := fmt.Sprintf("/incident %s — known case %s (%s), playbook step %d: %s", what, c.ID, c.Title, i+1, st.Note)
		args, err := a.playArgs(st.Args, sig)
		if err != nil {
			return handoff(triageNote + "\n\n" + playbookNote(c, done, fmt.Sprintf("step %d (%s): %v", i+1, label, err)))
		}
		r, err := a.runTool(ctx, tool, args, why)
		if err != nil {
			if strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "not approved") || ctx.Err() != nil {
				// the operator said no: stop here, don't let a model push on
				text := fmt.Sprintf("Stopped at step %d (%s): %v\n\nDone so far:\n%s", i+1, label, err, bullets(done))
				return a.localIncidentDone(what, c, text), nil
			}
			return handoff(triageNote + "\n\n" + playbookNote(c, done, fmt.Sprintf("step %d (%s) failed: %v", i+1, label, err)))
		}
		done = append(done, fmt.Sprintf("%s: %s", label, stepOutcome(st.Do, r.Text)))
	}

	_, matched := c.Matches(sig)
	if c.PlaybookKind == "report" {
		return a.reportIncident(ctx, what, c, matched, done), nil
	}
	outcome := "resolved — " + c.ID + " playbook completed"
	recorded := ""
	if rt, ok := a.Reg.Get("incident.record"); ok {
		rr, err := a.runTool(ctx, rt, toolkit.Args{
			"title": c.Title, "case": c.ID, "root_cause": c.Title + " (matched " + strings.Join(matched, ", ") + ")",
			"evidence": strings.Join(matched, "\n"), "actions": strings.Join(done, "\n"), "outcome": outcome,
		}, "/incident: record")
		if err == nil {
			recorded = "\n\n" + firstLine(rr.Text)
		}
	}
	text := fmt.Sprintf("**Resolved: %s** (known case `%s`)\n\nEvidence: %s\n\nWhat was done:\n%s%s",
		c.Title, c.ID, strings.Join(matched, ", "), bullets(done), recorded)
	return a.localIncidentDone(what, c, text), nil
}

func (a *Agent) localIncidentDone(what string, c *kb.Case, text string) string {
	text += "\n\n_worked locally from the knowledge base · no model call_"
	a.emit(Event{Kind: EvText, Text: text})
	kept := text
	if a.egressMode() == egress.Strict {
		kept = egress.Mask(text)
	}
	a.history = append(a.history,
		Msg{Role: "user", Text: a.turnPrefix() + "/incident " + what},
		Msg{Role: "assistant", Text: "[worked locally by cometcli from case " + c.ID + "]\n" + kept})
	a.routes.Local++
	if lg := a.Audit(); lg != nil {
		_ = lg.Log(audit.KindPrompt, a.profileName(), map[string]any{"text": "/incident " + what, "route": "local", "case": c.ID})
	}
	return text
}

// playbookNote tells the model what the playbook already did.
func playbookNote(c *kb.Case, done []string, failure string) string {
	return fmt.Sprintf("[cometcli matched case %s and ran its playbook until a step failed — continue from here, don't repeat the steps that worked]\ndone:\n%s\n%s",
		c.ID, bullets(done), failure)
}

func bullets(items []string) string {
	if len(items) == 0 {
		return "- (nothing yet)"
	}
	return "- " + strings.Join(items, "\n- ")
}

// playArgs fills a step's string arguments: {unit}, {home}, {binary} and
// {container_home} from the profile, {sig:<name>} from the triage
// signals, {peers} with the other running nodes of the chain.
func (a *Agent) playArgs(args map[string]any, sig kb.Signals) (toolkit.Args, error) {
	p := a.Ctx.Profile
	r := strings.NewReplacer("{unit}", shellWord(p.Service.Unit), "{home}", shellWord(p.Home),
		"{binary}", shellWord(p.Binary), "{container_home}", shellWord(p.Signer.ContainerHome))
	out := toolkit.Args{}
	for k, v := range args {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		s = r.Replace(s)
		for _, m := range sigRefRe.FindAllStringSubmatch(s, -1) {
			val, ok := sig[m[1]]
			if !ok || fmt.Sprint(val) == "" {
				return nil, fmt.Errorf("signal %s isn't known for this node", m[1])
			}
			s = strings.ReplaceAll(s, m[0], fmt.Sprint(val))
		}
		if strings.Contains(s, "{peers}") {
			peers := networktool.PeersOf(a.Ctx, p.ChainID, p.Name)
			if len(peers) == 0 {
				return nil, fmt.Errorf("no other running node of %s among the profiles to peer with", p.ChainID)
			}
			s = strings.ReplaceAll(s, "{peers}", strings.Join(peers, ","))
		}
		out[k] = s
	}
	return out, nil
}

var sigRefRe = regexp.MustCompile(`\{sig:([a-z0-9_.]+)\}`)

// reportIncident ends a report playbook: what cometcli found, and what
// the operator decides — cometcli doesn't decide for them.
func (a *Agent) reportIncident(ctx context.Context, what string, c *kb.Case, matched, done []string) string {
	var todo []string
	for _, f := range c.Fix {
		todo = append(todo, strings.TrimSpace(fixTagRe.ReplaceAllString(f, "")))
	}
	if rt, ok := a.Reg.Get("incident.record"); ok {
		_, _ = a.runTool(ctx, rt, toolkit.Args{
			"title": c.Title, "case": c.ID, "root_cause": c.Title + " (matched " + strings.Join(matched, ", ") + ")",
			"evidence": strings.Join(matched, "\n"), "actions": strings.Join(done, "\n"), "outcome": "reported — waiting on the operator's decision",
		}, "/incident: record")
	}
	text := fmt.Sprintf("**%s** (known case `%s`)\n\nWhat cometcli found:\n%s\n\nYour decision — cometcli won't make it for you:\n%s",
		c.Title, c.ID, bullets(done), bullets(todo))
	return a.localIncidentDone(what, c, text)
}

var fixTagRe = regexp.MustCompile(`^\[(read|change|tx)\]\s*`)

// shellWord keeps a profile value usable in a command: plain paths and
// names pass through (so ~ still expands), anything else is quoted.
func shellWord(s string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-~+:@", r)
	}
	for _, r := range s {
		if !plain(r) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// runTool runs one registry tool for cometcli's own procedures, through
// the same gates as the model's calls: permission rules, approvals for
// changes and key use, the audit log and the operator's UI.
func (a *Agent) runTool(ctx context.Context, t toolkit.Tool, args toolkit.Args, purpose string) (*toolkit.Result, error) {
	return a.runToolOn(ctx, t, args, purpose, a.Ctx.Profile)
}

// runToolOn is runTool on a given node.
func (a *Agent) runToolOn(ctx context.Context, t toolkit.Tool, args toolkit.Args, purpose string, p *config.Profile) (*toolkit.Result, error) {
	name := t.Name()
	shown := a.Redact.Args(args)
	a.emit(Event{Kind: EvToolStart, Tool: name, Tier: t.Tier().String(), Args: shown})
	runCtx, cancel := a.toolCtxFor(ctx, t, args, p)
	defer cancel()
	defer runCtx.Close()
	runCtx.Purpose = purpose
	err := a.ruleGate(runCtx, t, shown)
	var res *toolkit.Result
	if err == nil {
		res, err = t.Run(runCtx, args)
	}
	if lg := a.Audit(); lg != nil {
		var data map[string]any
		if res != nil {
			data = a.Redact.Args(res.Data)
		}
		lg.ToolSeen(a.profileName(), name, t.Tier().String(), shown, data, err, "")
	}
	if err != nil {
		a.emit(Event{Kind: EvToolResult, Tool: name, Tier: t.Tier().String(), Err: a.Redact.Text(firstLine(err.Error()))})
		return nil, err
	}
	text := a.Redact.Text(ansiRe.ReplaceAllString(res.Text, ""))
	res.Text = text
	a.emit(Event{Kind: EvToolResult, Tool: name, Tier: t.Tier().String(), Text: firstLine(text), Output: preview(text)})
	return res, nil
}

// stepOutcome is a step's result in one line for the report: the first
// line for most tools, a line count for raw output (a log's first line
// says nothing about the incident).
func stepOutcome(tool, text string) string {
	text = strings.TrimSpace(text)
	switch tool {
	case "node.logs", "bash", "read", "grep":
		return fmt.Sprintf("%d lines of output", len(strings.Split(text, "\n")))
	}
	return firstLine(text)
}

// refreshSignals re-collects the signal families a condition names
// ("val.jailed == true" → val.*) into sig. Collection failures keep the
// old values.
func (a *Agent) refreshSignals(ctx context.Context, sig kb.Signals, cond string) {
	if sig == nil || a.Ctx.Profile == nil {
		return
	}
	name, _, _ := strings.Cut(strings.TrimSpace(cond), " ")
	family, _, ok := strings.Cut(name, ".")
	if !ok {
		return
	}
	c, cancel := toolkit.WithDeadline(a.Ctx.Derive(ctx), time.Minute)
	defer cancel()
	defer c.Close()
	for k, v := range triage.CollectFor(c, 5*time.Minute, []string{family + "."}).Signals {
		sig[k] = v
	}
}

// metaTools are how the model looks around, not steps of a fix.
var metaTools = map[string]bool{"node.triage": true, "tool_search": true, "todo_write": true, "use_node": true, "task": true}

// learnedSteps are the tool calls the model made since history[from]
// that worked, as playbook steps (lookups and bookkeeping left out).
func (a *Agent) learnedSteps(from int) []kb.PlayStep {
	failed := map[string]bool{}
	for _, m := range a.history[from:] {
		if m.Role == "tool" && m.IsError {
			failed[m.CallID] = true
		}
	}
	var steps []kb.PlayStep
	for _, m := range a.history[from:] {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.Calls {
			name := toolkit.ResolveName(call.Name)
			if failed[call.ID] || metaTools[name] || strings.HasPrefix(name, "kb.") || strings.HasPrefix(name, "incident.") {
				continue
			}
			if _, ok := a.Reg.Get(name); !ok {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal(call.Args, &args)
			st := kb.PlayStep{Do: name, Args: args, Note: "learned from the model's fix"}
			if n := len(steps); n > 0 && steps[n-1].Do == st.Do && fmt.Sprint(steps[n-1].Args) == fmt.Sprint(st.Args) {
				continue
			}
			steps = append(steps, st)
		}
	}
	if len(steps) > 15 {
		steps = steps[:15]
	}
	return steps
}

// offerPlaybook asks the operator whether the steps the model just took
// should become the case's playbook, so the next identical incident runs
// without the model. Saved to the operator's own KB (it overrides the
// built-in case); nothing is saved without a yes.
func (a *Agent) offerPlaybook(c *kb.Case, from int) {
	steps := a.learnedSteps(from)
	if len(steps) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The model worked case %s (%s) with these steps:\n", c.ID, c.Title)
	for i, st := range steps {
		args, _ := json.Marshal(st.Args)
		fmt.Fprintf(&b, "  %d. %s %s\n", i+1, st.Do, a.Redact.Text(string(args)))
	}
	b.WriteString("Save them as this case's playbook? Next time cometcli runs them itself (changes and transactions still ask).")
	if a.Ctx.Chooser == nil {
		a.emit(Event{Kind: EvNotice, Text: "the model's steps for " + c.ID + " could become its playbook — run /incident interactively to review and save them"})
		return
	}
	i, err := a.Ctx.Chooser(a.Ctx, b.String(), []string{"Save as the playbook", "Don't save"})
	if err != nil || i != 0 {
		return
	}
	learned := *c
	learned.Playbook = steps
	learned.PlaybookKind = ""
	raw, err := yaml.Marshal([]*kb.Case{&learned})
	if err != nil {
		return
	}
	p, err := config.Path("kb", c.ID+".yaml")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(p), 0o700)
	}
	if err == nil {
		err = os.WriteFile(p, raw, 0o600)
	}
	if err != nil {
		a.emit(Event{Kind: EvNotice, Text: "couldn't save the playbook: " + err.Error()})
		return
	}
	a.emit(Event{Kind: EvNotice, Text: fmt.Sprintf("saved %d steps as the playbook for %s → %s (edit or delete it there)", len(steps), c.ID, p)})
}
