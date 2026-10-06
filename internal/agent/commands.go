package agent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/runbook"
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
}

// CommandHelp lists the shared slash commands; front-ends append their own.
const CommandHelp = `  /mode [ops|readonly]          show or set the approval posture
  /approve [local-change on|off] show or toggle autopilot (on-chain can never be autopiloted)
  /model [name | provider name] show or switch the LLM
  /runbook [name]               list runbooks, or have the agent run one
  /tools                        list tools with how this session treats each
  /profile                      active profile
  /audit                        audit log path + session id
  /reset                        clear conversation, start a new audit session`

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
			return fmt.Errorf("no agent configured — set agent.provider in the profile")
		}
		return nil
	}
	switch f[0] {
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
	return CmdResult{}, ErrUnknownCommand
}
