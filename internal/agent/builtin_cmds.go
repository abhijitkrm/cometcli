package agent

import "github.com/abhijitkrm/cometcli/internal/settings"

// builtinCommands ship with cometcli. A user or project command with the
// same name overrides one.
var builtinCommands = map[string]*settings.Command{
	"incident": {
		Name: "incident", Scope: "builtin", ArgHint: "[what you see]",
		Description: "Work an incident to resolution: triage, match a known case, fix the root cause, wait, verify, report",
		Body: `Work this incident on the node to resolution. Keep the operator's checklist current with todo_write; don't stop between steps unless something needs their decision.

1. node.triage (since=2h if the problem started earlier). Note which sources were unavailable, what changed since the last check, and past incidents on this node — a repeat means the last fix missed something (incident.show it). For "since when?", use node.history.
2. Take the top matched case: kb.show it, run its confirm steps. If confirmation fails, move to the next case or kb.search the symptoms/log lines. No case at all → investigate from the signals and logs (node.logs, bash read-only).
3. If several cases match, fix the one that CAUSES the others first (disk full → crash → behind → jailed: disk first).
4. Apply the fix steps by CALLING the tools. [change] and [tx] steps are approved at the tool's own prompt: state in one line what you're doing and why, then make the call in the same step — never stop to ask for approval in text. Never do anything a case lists under NEVER.
5. Wait for the effect with wait.until (synced, signing, in-consensus, or condition=signal value="<expr>"). If progress stalls, re-triage instead of waiting longer.
6. Verify with the case's verify steps, then node.triage again: the case must no longer match.
7. Record it with incident.record (title, root cause, case id, evidence, actions, outcome) — the timeline and tx hashes are filled in for you.
8. Report: root cause with evidence, actions taken (tx hashes), final state. If you found a cause no case described, propose a kb.add case (with a test fixture) for the operator to approve.

What the operator sees: $ARGUMENTS`,
	},
	"recover-jail": {
		Name: "recover-jail", Scope: "builtin", ArgHint: "[notes]",
		Description: "Recover a jailed validator end to end: diagnose, fix the cause, sync, unjail, verify it signs",
		Body: `The validator on this node is jailed (or may be). Run the recovery to completion — keep the operator's checklist current with todo_write and don't stop between steps unless something needs their decision.

1. Diagnose with val.jail-check. If it is TOMBSTONED, stop: explain, and do not unjail or restart the signer.
2. Root cause: from its evidence (jail line, log errors, restarts, OOM kills, disk, peers) name the most likely cause. If it is still present — no peers, disk full, signer errors, crash loop — fix it first (propose the exact change; the operator approves). Otherwise the validator will be jailed again.
3. Sync: if the node isn't synced, wait.until condition=synced timeout=3600. If the progress says it is NOT advancing, investigate (node.peers, node.logs) instead of waiting longer.
4. If the jail period hasn't ended, wait.until condition=unjailable.
5. val.unjail. If it fails, read the reason and the → hint, fix that cause (raise gas-price, self-delegate to the minimum, fund fees, wait longer…) and retry — at most 3 attempts.
6. Confirm: wait.until condition=in-consensus window=50 min_signed_pct=95 timeout=1800, then val.consensus.
7. Report: why it was jailed and the root cause (with the evidence), what you did (tx hashes), and the final consensus numbers.

(This is the knowledge-base case val-jailed-downtime as a ready procedure; for anything else use /incident.)

Operator notes: $ARGUMENTS`,
	},
}

// builtinNeedsNode lists built-ins that only make sense in node mode.
var builtinNeedsNode = map[string]bool{"recover-jail": true, "incident": true}
