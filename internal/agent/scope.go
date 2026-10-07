package agent

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// GeneralPrompt is the system prompt for general mode: an SRE agent in
// the operator's terminal, with no node attached.
func GeneralPrompt(c *toolkit.Context, cwd string) string {
	host, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, `You are cometcli — an expert SRE agent working in the operator's terminal.
You run shell commands and edit files on their machine through tools; they
watch and approve anything that changes the system.

Rules you must follow:
- Questions are about THIS machine unless the operator says otherwise.
  "Summarize container health", "why is disk full", "check nginx" mean:
  inspect it now with your tools (docker ps / docker inspect, df -h,
  systemctl status, logs …) and answer from what you found. Never answer
  such a question with a generic explanation; give one only when asked
  how something works in general.
- Investigate before changing anything; reference exact evidence (log
  lines, versions, exit codes) in answers.
- Keep answers terse, technical, and actionable. Use markdown sparingly.
- NEVER ask for or echo secrets: mnemonics, private keys, tokens.
- For destructive or irreversible steps, say what will happen first.

%s
ENVIRONMENT: %s/%s, host %s, working directory %s, date %s
`, generalToolsText, runtime.GOOS, runtime.GOARCH, host, cwd, time.Now().Format("2006-01-02"))
	if c != nil && c.Cfg != nil && len(c.Cfg.Profiles) > 0 {
		var names []string
		for n, p := range c.Cfg.Profiles {
			names = append(names, fmt.Sprintf("%s (%s, %s)", n, orNone(p.Role), orNone(p.ChainID)))
		}
		sort.Strings(names)
		active := ""
		if ap, err := c.Cfg.ActiveProfile(""); err == nil && ap != nil {
			active = " Active profile: " + ap.Name + "."
		}
		fmt.Fprintf(&b, "NODE PROFILES: %s.%s For any question about a node, validator or chain (jailing, sync, peers, upgrades, governance, txs), call use_node with the right profile yourself and continue — never ask the operator to switch. Any profile on a chain answers chain-wide questions; pick the active one when nothing points elsewhere.\n", strings.Join(names, "; "), active)
	}
	return b.String()
}

// generalToolsText describes the general tools; shared by both modes.
const generalToolsText = `General tools:
- bash: any shell command; the working directory persists between calls.
  Read-only commands run at once; changes wait for the operator's
  approval; transactions always do. stdin is closed: a command or script
  that prompts (read -p, sudo password, y/N) gets EOF — ask the operator
  to run those in their terminal instead.
- read / grep / glob for files — prefer them over cat/grep/find in bash.
  edit and write need a read of the file first; keep edits minimal.
- Before running a script, read it and say what it will do.
- Refusals are final: consensus keys, key ceremonies (keys add/export),
  mnemonics and state resets never pass through you. Don't retry
  variations of a refused or denied command; explain and hand the step
  to the operator with the exact command.
- web_fetch for release notes and docs. todo_write to track work of 3+
  steps (an upgrade, a migration) so the operator sees progress.
`

// SwitchScope moves the session to a node profile (nil = general mode),
// keeping the conversation. The system prompt and tool set are rebuilt,
// so provider-bound replay state is dropped and the cache restarts; the
// model is told about the switch on the next turn.
func (a *Agent) SwitchScope(p *config.Profile) {
	note := "[The operator switched this session to general mode: node tools are gone; the shell runs on their machine.]"
	if p != nil {
		note = fmt.Sprintf("[The operator switched this session to node mode for profile %s (role %s, chain %s, transport %s); node tools, rules and a live snapshot are now in your instructions.]",
			p.Name, orNone(p.Role), orNone(p.ChainID), orNone(p.Transport.Type))
	}
	a.switchScope(p, note)
}

// switchScope moves the session to p (nil = general); note, if any, is
// carried into the next user turn (the model's own use_node needs none).
func (a *Agent) switchScope(p *config.Profile, note string) {
	old := a.Ctx
	c := &toolkit.Context{
		Context: old.Context, Profile: p, Cfg: old.Cfg, Out: old.Out,
		Audit: old.Audit, Approver: old.Approver, AutoApproveBelow: old.AutoApproveBelow,
		Chooser: old.Chooser, Secret: old.Secret,
	}
	if a.ownCtx {
		old.Close()
	}
	a.Ctx, a.ownCtx = c, true
	a.Redact = RedactorFor(p)
	a.sys, a.snap, a.toolSig = "", "", ""
	a.loaded = nil
	a.Tools = toolkit.NewSession("")
	a.dropBoundRaw()
	if note == "" {
		return
	}
	if a.carry != "" {
		a.carry += "\n\n"
	}
	a.carry += note
}

// Close releases a context created by SwitchScope and stops MCP servers.
func (a *Agent) Close() {
	if a.ownCtx {
		a.Ctx.Close()
	}
	if a.parent == nil {
		a.ext.close(a.Reg)
	}
}
