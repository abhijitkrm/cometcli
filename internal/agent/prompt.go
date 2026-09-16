package agent

import (
	"fmt"
	"strings"

	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// SystemPrompt builds the agent system prompt with a live context snapshot.
func SystemPrompt(c *toolkit.Context) string {
	p := c.Profile
	var b strings.Builder
	b.WriteString(`You are cometcli — an expert SRE agent for Cosmos-EVM validators
(CometBFT consensus + Ethereum-compatible execution via the cosmos/evm stack).

You operate ONE node via tools. Rules you must follow:
- Prefer read-only tools first; diagnose before proposing changes.
- NEVER ask for or echo mnemonics, private keys, or priv_validator contents.
- For on-chain actions: explain the tx you're about to build, then call the
  tool — the human still has to approve at the tx gate.
- For jail recovery: check val.signing + chain params BEFORE suggesting
  unjail; a tombstoned validator must never be restarted to sign.
- Reference exact numbers (heights, missed counts, drift) in answers.
- Keep answers terse, technical, and actionable. Use markdown sparingly.

`)
	fmt.Fprintf(&b, "NODE PROFILE: %s (role=%s, chain-id=%s, evm-chain-id=%d, binary=%s, home=%s)\n",
		p.Name, p.Role, p.ChainID, p.EVMChainID, p.Binary, p.Home)
	fmt.Fprintf(&b, "ENDPOINTS: comet=%s grpc=%s evm=%s transport=%s service=%s:%s\n\n",
		p.Endpoints.Comet, p.Endpoints.GRPC, orNone(p.Endpoints.EVM),
		p.Transport.Type, p.Service.Type, p.Service.Unit)
	return b.String()
}

// SnapshotText renders a one-paragraph live status line for prompt context.
func SnapshotText(c *toolkit.Context) string {
	var parts []string
	if cc, err := c.Comet(); err == nil {
		if st, err := cc.Status(c); err == nil {
			parts = append(parts,
				fmt.Sprintf("height=%d catching_up=%v", st.SyncInfo.LatestBlockHeight, st.SyncInfo.CatchingUp),
				fmt.Sprintf("voting_power=%d", st.ValidatorInfo.VotingPower))
		}
	}
	if len(parts) == 0 {
		return "(no live snapshot — node unreachable or unconfigured)"
	}
	return "LIVE: " + strings.Join(parts, "  ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
