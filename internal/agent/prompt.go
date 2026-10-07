package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/abhijitkrm/cometcli/internal/config"
	"github.com/abhijitkrm/cometcli/internal/monitor"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// SystemPrompt builds the agent system prompt with a live context snapshot.
func SystemPrompt(c *toolkit.Context) string {
	p := c.Profile
	var b strings.Builder
	b.WriteString(`You are cometcli — an expert SRE agent for Cosmos SDK and Cosmos-EVM validators
(CometBFT consensus; on Cosmos-EVM chains also Ethereum-compatible execution via cosmos/evm).

You operate ONE node via tools. Rules you must follow:
- Questions are about THIS node: inspect it with your tools and answer
  from what you find — never with a generic explanation unless the
  operator asks how something works in general.
- Prefer read-only tools first; diagnose before proposing changes.
- NEVER ask for or echo mnemonics, private keys, or priv_validator contents.
- For on-chain actions: explain the tx you're about to build, then call the
  tool — the human still has to approve at the tx gate.
- A tombstoned validator must never be unjailed or restarted to sign.
- Reference exact numbers (heights, missed counts, drift) in answers.
- Keep answers terse, technical, and actionable. Use markdown sparingly.

Incidents and "is something wrong?" questions — work the method:
1. node.triage first: one sweep of every signal, ranked known cases.
2. kb.show the top case; run its confirm steps. Trust evidence over the
   match — if confirmation fails, try the next case or kb.search.
3. Fix the ROOT CAUSE before the symptom (disk before restart, cause
   before unjail). Steps tagged [change]/[tx] go to the operator's
   approval; explain each first. Never do what a case lists under NEVER.
4. Wait for progress with wait.until (synced, signing, in-consensus,
   height, or signal="<triage expr>") instead of guessing; if progress
   stalls, re-triage.
5. Verify with the case's verify steps, then re-run node.triage.
6. Report cause + evidence, actions (tx hashes), final state. If no case
   fit and you found the cause, offer to record it with kb.add.

`)
	b.WriteString(strings.Replace(generalToolsText, "General tools:", "General tools (they run on the node's host — local or over SSH):", 1))
	b.WriteString("\n")
	fmt.Fprintf(&b, "NODE PROFILE: %s (role=%s, chain-id=%s, evm-chain-id=%d, binary=%s, home=%s)\n",
		p.Name, p.Role, p.ChainID, p.EVMChainID, p.Binary, p.Home)
	fmt.Fprintf(&b, "ENDPOINTS: comet=%s grpc=%s evm=%s transport=%s service=%s:%s\n\n",
		p.Endpoints.Comet, p.Endpoints.GRPC, orNone(p.Endpoints.EVM),
		p.Transport.Type, p.Service.Type, p.Service.Unit)
	return b.String()
}

// snapshotMax bounds the live digest injected into every system prompt.
const snapshotMax = 1024

// SnapshotText renders a bounded live digest (chain, node, validator,
// host) for prompt context, gathered with the same collector as the
// dashboards. Never touches key material — monitor.Collect only reads RPC
// status, signing info, and host df/free.
func SnapshotText(c *toolkit.Context) string {
	sub, cancel := toolkit.WithDeadline(c, 8*time.Second)
	defer cancel()
	defer sub.Close()
	return formatSnapshot(c.Profile, monitor.Collect(sub))
}

func formatSnapshot(p *config.Profile, s *monitor.Snapshot) string {
	if s == nil || !s.Reachable {
		msg := "(no live snapshot — node unreachable or unconfigured"
		if s != nil && len(s.Errors) > 0 {
			msg += ": " + s.Errors[0]
		}
		return truncate(msg+")", snapshotMax)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "LIVE SNAPSHOT (%s UTC):\n", s.TS.UTC().Format("15:04:05"))
	if p != nil {
		fmt.Fprintf(&b, "- chain: chain-id=%s evm-chain-id=%d\n", orNone(p.ChainID), p.EVMChainID)
	}
	fmt.Fprintf(&b, "- node: version=%s height=%d catching_up=%v peers=%d voting_power=%d\n",
		orNone(s.Version), s.Height, s.CatchingUp, s.Peers, s.VotingPower)
	if s.Window > 0 || s.Jailed || s.Tombstoned {
		fmt.Fprintf(&b, "- validator: missed=%d/%d uptime=%.2f%% jailed=%v tombstoned=%v\n",
			s.Missed, s.Window, s.UptimePct, s.Jailed, s.Tombstoned)
	}
	if s.DiskUsedPct > 0 || s.MemUsedPct > 0 || s.ServiceUp != nil {
		svc := "unknown"
		if s.ServiceUp != nil {
			svc = fmt.Sprint(*s.ServiceUp)
		}
		host := []string{fmt.Sprintf("disk=%.0f%%", s.DiskUsedPct)}
		if s.MemUsedPct > 0 { // 0 means the probe isn't available (e.g. macOS)
			host = append(host, fmt.Sprintf("mem=%.0f%%", s.MemUsedPct))
		}
		fmt.Fprintf(&b, "- host: %s service_up=%s\n", strings.Join(host, " "), svc)
	}
	if s.EVMHeight > 0 {
		fmt.Fprintf(&b, "- evm: height=%d drift=%d\n", s.EVMHeight, s.EVMDrift)
	}
	for i, e := range s.Errors {
		if i == 3 {
			break
		}
		fmt.Fprintf(&b, "- probe error: %s\n", truncate(e, 160))
	}
	return truncate(b.String(), snapshotMax)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
