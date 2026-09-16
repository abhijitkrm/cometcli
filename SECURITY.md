# Security Policy

cometcli manages validator infrastructure — we take that seriously.

## Threat model

- **Consensus keys**: `priv_validator_key.json` contents are never read into
  memory, logged, or sent to an LLM. `priv_validator_state.json` is read only
  for double-sign protection (HRS checks).
- **Ops keys**: live in the OS keychain or an encrypted file backend. Never
  printed, never in LLM context — the redaction layer scrubs mnemonics, key
  material, and secrets in both directions.
- **Transactions**: always simulated first, always displayed decoded, always
  require explicit human approval. No auto-broadcast path exists.
- **LLM traffic**: only tool descriptions, redacted tool results, and a
  bounded status snapshot leave the machine. `agent.provider = off` disables
  it entirely.
- **Audit**: append-only JSONL at `~/.cometcli/audit/` records every tool
  call, shell command, prompt (redacted), and tx.

## Reporting

Report vulnerabilities privately via GitHub Security Advisories. Do not open
public issues for exploitable findings.
