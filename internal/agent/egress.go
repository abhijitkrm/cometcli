package agent

import (
	"strings"

	"github.com/abhijitkrm/cometcli/internal/audit"
	"github.com/abhijitkrm/cometcli/internal/egress"
)

// egressMode is how much raw output the model may see in the current
// scope (it changes when the session moves onto a node).
func (a *Agent) egressMode() egress.Mode {
	p := a.Ctx.Profile
	if p == nil {
		return egress.ModeFor(a.conf.Egress, "", false)
	}
	setting := a.conf.Egress
	if p.Agent.Egress != "" {
		setting = p.Agent.Egress
	}
	return egress.ModeFor(setting, p.Role, true)
}

// guard is the last check before a request leaves the machine: any part
// of it that still carries key material is replaced whole — in the
// session history too, so it's never offered again.
func (a *Agent) guard(req *Request) {
	if f := egress.Scan(req.System); len(f) > 0 {
		a.withheld("system prompt", f)
		req.System = egress.Withheld(f)
		a.sys = req.System
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if f := egress.Scan(m.Text); len(f) > 0 {
			a.withheld(m.Role+" message", f)
			m.Text = egress.Withheld(f)
		}
		if len(m.RawSteps) > 0 {
			if f := egress.Scan(string(m.RawSteps)); len(f) > 0 {
				a.withheld("raw model output", f)
				m.RawSteps, m.RawProvider = nil, ""
			}
		}
		for j := range m.Calls {
			if f := egress.Scan(string(m.Calls[j].Args)); len(f) > 0 {
				a.withheld("tool call arguments", f)
				m.Calls[j].Args = []byte(`{}`)
			}
		}
		if f := egress.Scan(string(m.JSONArgs)); len(f) > 0 {
			m.JSONArgs = nil
		}
	}
	// req.Messages aliases a.history: the withheld forms are now the history
}

func (a *Agent) withheld(what string, f []egress.Finding) {
	a.emit(Event{Kind: EvNotice, Text: "withheld " + what + " from the model: it contained key material (" + egress.Kinds(f) + ")"})
	if lg := a.Audit(); lg != nil {
		_ = lg.Log(audit.KindEgress, a.profileName(), map[string]any{"what": what, "kinds": egress.Kinds(f)})
	}
}

// purpose is the agent's latest stated reason (its text before the tool
// call), shown when a key is about to be used.
func (a *Agent) purpose() string {
	for i := len(a.history) - 1; i >= 0; i-- {
		m := a.history[i]
		if m.Role == "user" {
			return ""
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
			return clip(strings.Join(strings.Fields(a.Redact.Text(m.Text)), " "), 300)
		}
	}
	return ""
}
