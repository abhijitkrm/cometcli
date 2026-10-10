package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/egress"
	"github.com/abhijitkrm/cometcli/internal/router"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// forceModel prefixes a prompt that must reach the model (/llm).
const forceModel = "\x1fmodel\x1f"

// RouteStats counts how prompts were answered this session.
type RouteStats struct {
	Local  int // answered from the intent catalog, no model call
	Cached int // of those, served from the answer cache
	Model  int // sent to the model
}

// Routes reports how this session's prompts were answered.
func (a *Agent) Routes() RouteStats { return a.routes }

func (a *Agent) routerOn() bool {
	return a.parent == nil && a.catalog != nil && !strings.EqualFold(a.conf.Router, "off")
}

// routeLocal answers a known question from the intent catalog without a
// model call. It reports false when the model should handle the prompt:
// not a known question, no node to ask, or a tool failed.
func (a *Agent) routeLocal(ctx context.Context, input string) (string, bool) {
	m := a.catalog.Classify(input)
	if m.Intent == nil {
		// "is val2 healthy" asks what "is my node healthy" asks, of val2
		// (routeProfile picks val2); commands keep their own node words
		if g, ok := a.genericNode(input); ok {
			if gm := a.catalog.Classify(g); gm.Intent != nil && gm.Intent.Kind != "command" {
				gm.Why += "; the node's name read as \"node\""
				m = gm
			}
		}
	}
	a.lastRoute = m
	if m.Intent == nil {
		return "", false
	}
	in := m.Intent
	if in.Kind == "command" {
		return a.runCommand(ctx, input, m)
	}
	p := a.routeProfile(input, in)
	if p == nil {
		a.lastRoute.Why += "; no single node to ask"
		return "", false
	}
	key := in.ID + "|" + p.Name
	if len(m.Args) > 0 {
		key += "|" + fmt.Sprint(m.Args)
	}
	now := time.Now()
	if text, ok := a.answers.Get(key, now); ok {
		a.routes.Cached++
		return a.answeredLocally(input, in, text+"\n\n_answered locally from a recent check · /llm to ask the model_"), true
	}
	// read-only steps: nothing here may ask for, or get, an approval
	base := &toolkit.Context{Context: ctx, Profile: p, Cfg: a.Ctx.Cfg, Audit: a.Audit(), Approver: toolkit.DenyApprover, ReadOnly: true}
	results := map[string]router.Result{}
	for _, st := range in.Steps {
		t, ok := a.Reg.Get(st.Tool)
		if !ok || t.Tier() > toolkit.TierDiagnose {
			// the catalog only reads: anything else is the model's call
			a.lastRoute.Why += "; " + st.Tool + " isn't a read-only tool"
			return "", false
		}
		args := router.Fill(st.Args, m.Args)
		if len(args) < len(st.Args) {
			// a placeholder the prompt didn't fill ("which proposal?")
			a.lastRoute.Why += "; the question doesn't say which"
			return "", false
		}
		a.emit(Event{Kind: EvToolStart, Tool: st.Tool, Tier: t.Tier().String(), Args: a.Redact.Args(args)})
		sub, cancel := toolkit.WithDeadline(base, toolkit.CallTimeout(t, args))
		sub.ToolName = st.Tool
		res, err := t.Run(sub, args)
		sub.Close()
		cancel()
		r := router.Result{}
		if err != nil {
			r.Err = err.Error()
			a.emit(Event{Kind: EvToolResult, Tool: st.Tool, Err: a.Redact.Text(firstLine(r.Err))})
			if !st.Optional {
				if unreachableRe.MatchString(r.Err) {
					// a node that doesn't answer is a known answer
					text := fmt.Sprintf("**%s isn't reachable** — %s: %s\n\n`/incident %s` to find out why.", p.Name, st.Tool, firstLine(a.Redact.Text(r.Err)), p.Name)
					a.routes.Local++
					return a.answeredLocally(input, in, text+"\n\n_answered locally · no model call · /llm to ask the model_"), true
				}
				// the model explains other failures better than a template
				a.lastRoute.Why += "; " + st.Tool + " failed"
				return "", false
			}
		} else {
			r.Text, r.Data = ansiRe.ReplaceAllString(res.Text, ""), res.Data
			a.emit(Event{Kind: EvToolResult, Tool: st.Tool, Text: firstLine(a.Redact.Text(r.Text)), Output: preview(a.Redact.Text(r.Text))})
		}
		if lg := a.Audit(); lg != nil {
			var data map[string]any
			if res != nil {
				data = a.Redact.Args(res.Data)
			}
			lg.ToolSeen(p.Name, st.Tool, t.Tier().String(), args, data, err, "")
		}
		results[st.As] = r
	}
	text, err := in.Render(p.Name, results)
	if err != nil {
		a.lastRoute.Why += "; answer template: " + err.Error()
		return "", false
	}
	text = a.Redact.Text(text)
	a.answers.Put(key, text, in.TTL, now)
	a.routes.Local++
	return a.answeredLocally(input, in, text+"\n\n_answered locally · no model call · /llm to ask the model_"), true
}

// answeredLocally records a local answer: shown to the operator, kept in
// the conversation (so a later model turn knows it), and audited.
func (a *Agent) answeredLocally(input string, in *router.Intent, text string) string {
	a.emit(Event{Kind: EvText, Text: text})
	kept := text
	if a.egressMode() == egress.Strict {
		kept = egress.Mask(text)
	}
	a.history = append(a.history,
		Msg{Role: "user", Text: a.turnPrefix() + input},
		Msg{Role: "assistant", Text: "[answered locally by cometcli: " + in.Title + "]\n" + kept})
	if lg := a.Audit(); lg != nil {
		_ = lg.Log(audit.KindPrompt, a.profileName(), map[string]any{"text": input, "route": "local", "intent": in.ID})
	}
	return text
}

// routeProfile picks the node a known question is about: the session's
// node, else the one the prompt names, else the active profile, else the
// only one. Chain-wide questions can use any of these.
func (a *Agent) routeProfile(input string, in *router.Intent) *config.Profile {
	if a.Node() {
		if p, _ := a.nodeFor(input); p != nil && p.Name != a.Ctx.Profile.Name && in.Scope == "node" {
			return p
		}
		return a.Ctx.Profile
	}
	if p, _ := a.nodeFor(input); p != nil {
		return p
	}
	cfg := a.Ctx.Cfg
	if cfg == nil || len(cfg.Profiles) == 0 {
		return nil
	}
	if cfg.Active != "" {
		if p, ok := cfg.Profiles[cfg.Active]; ok {
			p.Name = cfg.Active
			return p
		}
	}
	if len(cfg.Profiles) == 1 {
		for n, p := range cfg.Profiles {
			p.Name = n
			return p
		}
	}
	return nil
}

// RouteWhy explains where the latest prompt went (for /route).
func (a *Agent) RouteWhy() string {
	m := a.lastRoute
	if m.Intent != nil {
		return fmt.Sprintf("answered locally: %s (%s)", m.Intent.ID, m.Why)
	}
	if m.Why == "" {
		return "no prompt classified yet"
	}
	return "sent to the model: " + m.Why
}

// runCommand carries out a direct command ("restart val3", "vote yes on
// 6"): the words are the tool's arguments, so no model is involved. The
// node is the one named, else the session's, else the active profile.
// Every change and transaction goes through the usual approvals; a
// failure is shown as it is, not handed to a model.
func (a *Agent) runCommand(ctx context.Context, input string, m router.Match) (string, bool) {
	in := m.Intent
	var p *config.Profile
	if name := m.Args["node"]; name != "" {
		cfg := a.Ctx.Cfg
		pp, ok := (*config.Profile)(nil), false
		if cfg != nil {
			pp, ok = cfg.Profiles[name]
		}
		if !ok {
			// "stop worrying" names no node: not a command after all
			a.lastRoute = router.Match{Why: fmt.Sprintf("looked like the %s command, but %q isn't one of your nodes", in.ID, name)}
			return "", false
		}
		pp.Name = name
		p = pp
	} else {
		p = a.routeProfile(input, in)
	}
	if p == nil {
		a.lastRoute.Why += "; no node to run it on"
		return "", false
	}
	var out []string
	for _, st := range in.Steps {
		t, ok := a.Reg.Get(st.Tool)
		if !ok {
			a.lastRoute.Why += "; no tool " + st.Tool
			return "", false
		}
		if a.Policy.ReadOnly() && t.Tier() > toolkit.TierDiagnose {
			text := fmt.Sprintf("%s is a %s action and this session is read-only.", in.Title, t.Tier())
			return a.answeredLocally(input, in, text), true
		}
		args := coerceArgs(t, router.Fill(st.Args, m.Args))
		res, err := a.runToolOn(ctx, t, args, "direct command: "+input, p)
		if err != nil {
			text := fmt.Sprintf("**%s** on %s — %v\n\n_ran directly · /llm %s to have the model work it out_", in.Title, p.Name, a.Redact.Text(err.Error()), input)
			a.routes.Local++
			return a.answeredLocally(input, in, text), true
		}
		out = append(out, strings.TrimSpace(res.Text))
	}
	a.routes.Local++
	text := fmt.Sprintf("**%s** on %s\n%s\n\n_ran directly · no model call_", in.Title, p.Name, strings.Join(out, "\n"))
	return a.answeredLocally(input, in, text), true
}

// coerceArgs turns a command's captured words into the tool's types: a
// boolean flag is true when its group matched, numbers are numbers.
func coerceArgs(t toolkit.Tool, args map[string]any) toolkit.Args {
	props, _ := t.Schema()["properties"].(map[string]any)
	out := toolkit.Args{}
	for k, v := range args {
		s, isStr := v.(string)
		spec, _ := props[k].(map[string]any)
		switch {
		case isStr && spec["type"] == "boolean":
			out[k] = s != "" && s != "false"
		case isStr && spec["type"] == "integer":
			if n, err := strconv.ParseFloat(s, 64); err == nil {
				out[k] = n
			}
		default:
			out[k] = v
		}
	}
	return out
}

// unreachableRe matches a node that didn't answer at all.
var unreachableRe = regexp.MustCompile(`(?i)connection refused|timed out|i/o timeout|no route to host|host is down|network is unreachable|deadline exceeded`)

// genericNode reads the one node a prompt names as "node", so a question
// about it classifies like one about "my node".
func (a *Agent) genericNode(input string) (string, bool) {
	p, _ := a.nodeFor(input)
	if p == nil || p.Name == "" {
		return input, false
	}
	re := regexp.MustCompile(`(?i)(^|[^\w.-])` + regexp.QuoteMeta(p.Name) + `($|[^\w.-])`)
	out := re.ReplaceAllString(input, "${1}node${2}")
	return out, out != input
}
