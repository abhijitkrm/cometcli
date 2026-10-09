package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/egress"
	"github.com/abhijitkrm/cometcli/internal/kb"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
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
	handoff := func(context string) (string, error) {
		prompt := procedure
		if context != "" {
			prompt += "\n\n" + context
		}
		return a.run(ctx, forceModel+prompt)
	}
	t, ok := a.Reg.Get("node.triage")
	if !ok {
		return handoff("")
	}
	res, err := a.runTool(ctx, t, toolkit.Args{})
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
	case c == nil || len(c.Playbook) == 0:
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
		if !st.Applies(sig) {
			done = append(done, fmt.Sprintf("skipped %s (%s doesn't hold)", st.Do, st.When))
			continue
		}
		tool, ok := a.Reg.Get(st.Do)
		if !ok {
			return handoff(triageNote + "\n\n" + playbookNote(c, done, fmt.Sprintf("step %d: no tool %s", i+1, st.Do)))
		}
		r, err := a.runTool(ctx, tool, playArgs(st.Args, a.Ctx.Profile))
		if err != nil {
			if strings.Contains(err.Error(), "denied") || ctx.Err() != nil {
				// the operator said no: stop here, don't let a model push on
				text := fmt.Sprintf("Stopped at step %d (%s): %v\n\nDone so far:\n%s", i+1, label, err, bullets(done))
				return a.localIncidentDone(what, c, text), nil
			}
			return handoff(triageNote + "\n\n" + playbookNote(c, done, fmt.Sprintf("step %d (%s) failed: %v", i+1, label, err)))
		}
		done = append(done, fmt.Sprintf("%s: %s", label, firstLine(a.Redact.Text(r.Text))))
	}

	_, matched := c.Matches(sig)
	outcome := "resolved — " + c.ID + " playbook completed"
	recorded := ""
	if rt, ok := a.Reg.Get("incident.record"); ok {
		rr, err := a.runTool(ctx, rt, toolkit.Args{
			"title": c.Title, "case": c.ID, "root_cause": c.Title + " (matched " + strings.Join(matched, ", ") + ")",
			"evidence": strings.Join(matched, "\n"), "actions": strings.Join(done, "\n"), "outcome": outcome,
		})
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

// playArgs fills {unit}, {home}, {binary} and {container_home} in string
// arguments from the profile.
func playArgs(args map[string]any, p *config.Profile) toolkit.Args {
	r := strings.NewReplacer("{unit}", shellWord(p.Service.Unit), "{home}", shellWord(p.Home),
		"{binary}", shellWord(p.Binary), "{container_home}", shellWord(p.Signer.ContainerHome))
	out := toolkit.Args{}
	for k, v := range args {
		if s, ok := v.(string); ok {
			v = r.Replace(s)
		}
		out[k] = v
	}
	return out
}

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
func (a *Agent) runTool(ctx context.Context, t toolkit.Tool, args toolkit.Args) (*toolkit.Result, error) {
	name := t.Name()
	shown := a.Redact.Args(args)
	a.emit(Event{Kind: EvToolStart, Tool: name, Tier: t.Tier().String(), Args: shown})
	runCtx, cancel := a.toolCtx(ctx, t, args)
	defer cancel()
	defer runCtx.Close()
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
		a.emit(Event{Kind: EvToolResult, Tool: name, Tier: t.Tier().String(), Text: "error: " + a.Redact.Text(firstLine(err.Error()))})
		return nil, err
	}
	text := a.Redact.Text(ansiRe.ReplaceAllString(res.Text, ""))
	res.Text = text
	a.emit(Event{Kind: EvToolResult, Tool: name, Tier: t.Tier().String(), Text: firstLine(text), Output: preview(text)})
	return res, nil
}
