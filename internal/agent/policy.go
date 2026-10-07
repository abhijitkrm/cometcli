package agent

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// Mode is the session's approval posture.
type Mode string

const (
	// ModeOps: observe/diagnose auto-run, local-change confirms (or
	// autopilots if enabled), on-chain always confirms.
	ModeOps Mode = "ops"
	// ModeReadOnly: only observe/diagnose tools exist for the model;
	// anything mutating is neither advertised nor runnable (the shell
	// stays, limited to read-only commands).
	ModeReadOnly Mode = "readonly"
	// ModeAcceptEdits: like ops, but file writes and edits inside the
	// working root run without a prompt.
	ModeAcceptEdits Mode = "accept-edits"
	// ModeBypass: every local-change runs without a prompt. Transactions
	// still always ask; key material and state resets stay refused.
	ModeBypass Mode = "bypass"
)

// Modes lists the approval postures in shift+tab cycling order.
var Modes = []Mode{ModeOps, ModeAcceptEdits, ModeReadOnly, ModeBypass}

// ParseMode accepts ops | readonly | accept-edits | bypass, plus Claude
// Code's names (default, plan, acceptEdits, bypassPermissions).
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "ops", "rw", "default":
		return ModeOps, nil
	case "readonly", "read-only", "ro", "safe", "plan":
		return ModeReadOnly, nil
	case "accept-edits", "acceptedits", "edits":
		return ModeAcceptEdits, nil
	case "bypass", "bypasspermissions", "yolo":
		return ModeBypass, nil
	}
	return "", fmt.Errorf("unknown mode %q — want ops, readonly, accept-edits or bypass", s)
}

// Policy maps tool tiers to auto | confirm | deny for one session.
// The zero value is ModeOps with no autopilot.
type Policy struct {
	Mode Mode
	// AutoLocal autopilots local-change tools (no confirm prompt).
	// There is deliberately no equivalent for on-chain.
	AutoLocal bool
}

// PolicyFrom reads the profile's agent defaults.
func PolicyFrom(ac config.AgentConf) (Policy, error) {
	m, err := ParseMode(ac.Mode)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{Mode: m}
	for _, t := range ac.Autopilot {
		if err := p.SetAutopilot(t, true); err != nil {
			return Policy{}, err
		}
	}
	return p, nil
}

// ReadOnly reports whether mutating tiers are refused.
func (p Policy) ReadOnly() bool { return p.Mode == ModeReadOnly }

// Allows reports whether a tool of tier t may be advertised and run.
func (p Policy) Allows(t toolkit.Tier) bool {
	return !p.ReadOnly() || t < toolkit.TierLocalChange
}

// AutoApproveBelow is the toolkit threshold this policy implies. on-chain
// is never below it, so a tx always reaches the human.
func (p Policy) AutoApproveBelow() toolkit.Tier {
	if (p.AutoLocal || p.Mode == ModeBypass) && !p.ReadOnly() {
		return toolkit.TierOnChain
	}
	return toolkit.TierLocalChange
}

// AcceptEdits reports whether in-root file edits skip the prompt.
func (p Policy) AcceptEdits() bool { return p.Mode == ModeAcceptEdits }

// Decision describes how a tier is handled: auto | confirm | deny.
func (p Policy) Decision(t toolkit.Tier) string {
	switch {
	case !p.Allows(t):
		return "deny"
	case t < p.AutoApproveBelow():
		return "auto"
	case t == toolkit.TierLocalChange && p.AcceptEdits():
		return "confirm (edits in root: auto)"
	default:
		return "confirm"
	}
}

// SetAutopilot toggles autopilot for a tier. Only local-change can be
// autopiloted; observe/diagnose are always auto and on-chain never is.
func (p *Policy) SetAutopilot(tier string, on bool) error {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "local-change", "local", "localchange":
		p.AutoLocal = on
		return nil
	case "observe", "diagnose":
		if !on {
			return fmt.Errorf("%s tools are read-only and always auto-run — use /mode readonly to restrict writes instead", tier)
		}
		return nil
	case "on-chain", "onchain", "tx":
		if on {
			return fmt.Errorf("on-chain actions can never be autopiloted — every transaction needs explicit approval")
		}
		return nil
	}
	return fmt.Errorf("unknown tier %q — want local-change", tier)
}

// String renders the per-tier table, e.g. for /mode.
func (p Policy) String() string {
	mode := p.Mode
	if mode == "" {
		mode = ModeOps
	}
	var parts []string
	for _, t := range []toolkit.Tier{toolkit.TierObserve, toolkit.TierDiagnose, toolkit.TierLocalChange, toolkit.TierOnChain} {
		parts = append(parts, fmt.Sprintf("%s=%s", t, p.Decision(t)))
	}
	return fmt.Sprintf("mode %s · %s", mode, strings.Join(parts, " · "))
}

// parseTier maps a Tier.String() back to the Tier (unknown → on-chain, the
// most restrictive, so a corrupt record can never relax a gate).
func parseTier(s string) toolkit.Tier {
	for _, t := range []toolkit.Tier{toolkit.TierObserve, toolkit.TierDiagnose, toolkit.TierLocalChange, toolkit.TierOnChain} {
		if t.String() == s {
			return t
		}
	}
	return toolkit.TierOnChain
}
