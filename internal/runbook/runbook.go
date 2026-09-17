// Package runbook implements codified multi-step procedures (playbooks) over
// the tool registry — e.g. jail recovery, host migration, statesync bootstrap.
package runbook

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Step is one runbook action.
type Step struct {
	Name     string
	Tool     string       // registry tool name; "" = note only
	Args     toolkit.Args // args passed to the tool
	Note     string       // printed before running
	Optional bool         // failure doesn't abort
	Manual   string       // human instruction (no tool) — pauses for approval
}

// Runbook is an ordered procedure.
type Runbook struct {
	Name  string
	Desc  string
	Steps []Step
}

// Builtins returns the shipped playbooks.
func Builtins() []Runbook {
	return []Runbook{
		{
			Name: "jail-recovery",
			Desc: "Diagnose jail cause, wait out jail period, unjail, verify",
			Steps: []Step{
				{Name: "validator status", Tool: "val.status"},
				{Name: "signing info", Tool: "val.signing"},
				{Name: "recent errors", Tool: "node.logs", Args: toolkit.Args{"lines": 200, "grep": "error|jail|slash"}, Optional: true},
				{Name: "unjail tx", Tool: "val.unjail", Note: "Review the simulated tx; it will prompt for approval."},
				{Name: "verify unjailed", Tool: "val.status"},
			},
		},
		{
			Name: "halt-recovery",
			Desc: "Diagnose a stalled chain: consensus state, logs, peer count",
			Steps: []Step{
				{Name: "node status", Tool: "node.status"},
				{Name: "peers", Tool: "node.peers"},
				{Name: "consensus errors", Tool: "node.logs", Args: toolkit.Args{"lines": 300, "grep": "CONSENSUS|error|panic|halt"}, Optional: true},
				{Name: "upgrade plan", Tool: "chain.upgrade-plan", Optional: true},
			},
		},
		{
			Name: "host-migration",
			Desc: "Move a validator to a new host WITHOUT double-signing",
			Steps: []Step{
				{Name: "record signing state", Tool: "sec.doublesign",
					Note: "Captures last signed height/round/step — verify the new host starts ABOVE this."},
				{Name: "stop old signer", Tool: "node.service", Args: toolkit.Args{"action": "stop"}},
				{Name: "confirm stopped", Tool: "node.service", Args: toolkit.Args{"action": "status"}},
				{Name: "manual provision", Manual: "On the NEW host: install binary, sync state (statesync/snapshot), copy priv_validator_key.json, set priv_validator_state.json to a height >= recorded HRS, then start."},
				{Name: "verify new signer", Tool: "val.signing", Note: "Run against the NEW host profile once it's up."},
			},
		},
		{
			Name: "statesync-bootstrap",
			Desc: "Rebuild a node via state-sync (wipes local state — confirms twice)",
			Steps: []Step{
				{Name: "node status", Tool: "node.status"},
				{Name: "stop service", Tool: "node.service", Args: toolkit.Args{"action": "stop"}},
				{Name: "wipe data", Manual: "Run: <binary> comet unsafe-reset-all --home <home> --keep-addrbook (DESTRUCTIVE — clears chain state, keeps addrbook). Confirm manually before proceeding."},
				{Name: "write statesync config", Tool: "snap.statesync", Args: toolkit.Args{"apply": true}},
				{Name: "start service", Tool: "node.service", Args: toolkit.Args{"action": "start"}},
				{Name: "verify sync", Tool: "mon.snapshot"},
			},
		},
		{
			Name: "coordinated-upgrade",
			Desc: "Prepare for a governance-gated chain upgrade",
			Steps: []Step{
				{Name: "upgrade plan", Tool: "chain.upgrade-plan"},
				{Name: "current version", Tool: "upgrade.check"},
				{Name: "stage binary", Tool: "upgrade.prepare", Note: "Downloads + stages into cosmovisor; prompts for approval."},
				{Name: "watch height", Tool: "upgrade.watch", Optional: true},
			},
		},
	}
}

// Get finds a playbook by name.
func Get(name string) (*Runbook, error) {
	for _, rb := range Builtins() {
		if rb.Name == name {
			return &rb, nil
		}
	}
	return nil, fmt.Errorf("no runbook %q (see `cometcli runbook list`)", name)
}

// Run executes a runbook against a context, printing progress.
func Run(rb *Runbook, c *toolkit.Context, reg *toolkit.Registry, out io.Writer) error {
	fmt.Fprintf(out, "▶ runbook: %s — %s\n\n", rb.Name, rb.Desc)
	for i, s := range rb.Steps {
		fmt.Fprintf(out, "── step %d/%d: %s\n", i+1, len(rb.Steps), s.Name)
		if s.Note != "" {
			fmt.Fprintf(out, "   %s\n", s.Note)
		}
		if s.Manual != "" {
			fmt.Fprintf(out, "   MANUAL: %s\n", s.Manual)
			if err := c.Approve("step completed manually? confirm to continue", toolkit.TierLocalChange, nil); err != nil {
				return err
			}
			continue
		}
		t, ok := reg.Get(s.Tool)
		if !ok {
			return fmt.Errorf("runbook references unknown tool %q", s.Tool)
		}
		res, err := t.Run(c, s.Args)
		if err != nil {
			if s.Optional {
				fmt.Fprintf(out, "   (optional) failed: %v\n", err)
				continue
			}
			return fmt.Errorf("step %q failed: %w", s.Name, err)
		}
		fmt.Fprintf(out, "%s\n\n", indent(res.Text))
	}
	fmt.Fprintln(out, "✓ runbook complete")
	return nil
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("   " + l + "\n")
	}
	return b.String()
}

// Names lists playbook names.
func Names() []string {
	var out []string
	for _, rb := range Builtins() {
		out = append(out, rb.Name)
	}
	sort.Strings(out)
	return out
}
