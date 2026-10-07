package agent

import "github.com/abhijitkrm/cometcli/internal/settings"

// builtinCommands ship with cometcli. A user or project command with the
// same name overrides one.
var builtinCommands = map[string]*settings.Command{
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

Operator notes: $ARGUMENTS`,
	},
}

// builtinNeedsNode lists built-ins that only make sense in node mode.
var builtinNeedsNode = map[string]bool{"recover-jail": true}
