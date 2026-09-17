# cometcli

**An agentic SRE terminal for Cosmos-EVM validators.** Local-first, single
binary, deterministic CLI on the outside — optional LLM agent on the inside.

cometcli is for validator operators (companies and independents) who need to
answer "is my node healthy?" and act on it in one terminal — no dashboards to
wire up, no context-switching between `evmd`, `systemctl`, `curl`, and
explorer tabs.

```bash
cometcli doctor                      # 11-point validator health checklist
cometcli val signing                 # missed blocks + uptime over the window
cometcli node config --action lint   # audit config.toml against the hardened baseline
cometcli tx send --to <addr> --amount 1000000atest --gas-price 1e9
cometcli agent                       # interactive AI SRE (or `cometcli ask "...")
```

Every capability is a deterministic subcommand **and** a tool the agent can
call — one tool registry, two front-ends. See [PLAN.md](PLAN.md) for the
architecture.

## Install

```bash
# from source (Go 1.25+)
git clone https://github.com/abhijitkrm/cometcli && cd cometcli
go build -o cometcli ./cmd/cometcli

# or go install
go install github.com/abhijitkrm/cometcli/cmd/cometcli@latest
```

## Quickstart

```bash
# 1. Register a node. `profile add` merges — re-run it to update one field.
cometcli profile add myval \
  --chain-id primium-1 --evm-chain-id 123457 --bech32-prefix cosmos \
  --role validator --home /var/lib/evmd --binary evmd \
  --comet tcp://127.0.0.1:26657 --grpc 127.0.0.1:9090 \
  --evm http://127.0.0.1:8545 \
  --service systemd --unit evmd.service \
  --signer ops --signer-backend file --fee-denom adex
cometcli profile use myval

# 2. Add an ops key (transaction signing only — consensus keys never touched)
cometcli keys add --name ops                 # generates a new key
cometcli keys add --name ops --recover       # import a BIP-39 mnemonic
cometcli keys add --name ops --privkey-hex <hex>   # import a raw key
# file backend reads the password from COMETCLI_KEYRING_PASSWORD

# 3. Check everything
cometcli doctor
```

```
✓  rpc reachable      height 4464
✓  not catching up    catching_up=false
✓  has peers          3 peers
✓  port exposure      clean
✗  key file perms     1 problems          ← config dir is 0755, want 0700
✓  signing health     missed 5/10000
✓  jail status        not jailed
✓  upgrade pending    none
✓  host disk
✓  ntp/clock
✓  evm parity         drift 0
```

## Command surface

Tools are grouped by domain. Required args also bind positionally —
`cometcli tx get <hash>` works like `evmd q tx <hash>`.

| Domain | Highlights |
|---|---|
| `node` | `status`, `peers`, `health`, `consensus`, `logs`, `config --action lint`, `service status\|restart`, `version-check` |
| `val` | `status`, `signing`, `rewards`, `votes` · on-chain: `unjail`, `withdraw`, `vote`, `edit`, `create` |
| `chain` | `validators`, `params`, `gov`, `upgrade-plan`, `pool`, `balance` |
| `evm` | `chainid` (profile-vs-RPC sanity), `parity` (comet↔JSON-RPC drift), `gasprice`, `txpool` |
| `tx` | `send`, `delegate`, `get` — every tx tool takes `--gas-price`, `--gas-limit`, `--fee-denom` |
| `keys` | `add` (`--recover`/`--privkey-hex`), `list`, `show`, `rm`, `convert` (bech32↔0x) — `eth_secp256k1` by default |
| `sec` | `exposure` (listening-port audit), `perms` (key-file permissions), `doublesign` (priv_validator_state HRS check) |
| `mon` | `snapshot`, `watch` (live TUI), `alerts` (rule engine → stdout/webhook) |
| `upgrade` | `check` (plan + binary + upstream release), `prepare` (cosmovisor staging), `watch` |
| `snap` | `list`, `prune`, `statesync` (fetch trust height, write `[statesync]`) |
| `runbook` | `list`, `run` — `jail-recovery`, `halt-recovery`, `host-migration`, `statesync-bootstrap`, `coordinated-upgrade` |
| `net` | `add-peer`, `rm-peer` (persistent peers in config.toml) |
| `fleet` | `status` — health matrix across all configured profiles |

Global flags: `--profile` (override active), `--json` (structured output),
`-y/--yes` (auto-approve observe/diagnose prompts — never on-chain).

## Transactions

Every on-chain op goes through build → simulate → decode → approve →
broadcast → confirm:

```
⚠  [on-chain] broadcast transaction
{
  "chain_id": "primium-1",
  "account": "cosmos10pmprk9…",
  "account_number": 0,
  "sequence": 1,
  "messages": ["/cosmos.bank.v1beta1.MsgSend {…}"],
  "fee": "155401875000000adex",
  "gas_limit": 138135
}
Proceed? [y/N]
```

Nothing hits the wire without an explicit `y`. `tx get <hash>` confirms
inclusion afterwards.

## The agent

`cometcli agent` (or `cometcli ask "why is my validator missing blocks?"`)
runs an LLM in a loop over the same tool registry. Providers are pluggable:

```bash
cometcli profile add myval --agent-provider anthropic --agent-model claude-sonnet-4-20250514
export ANTHROPIC_API_KEY=…          # or OPENAI_API_KEY, OLLAMA_API_KEY;
                                    # COMETCLI_LLM_API_KEY works as a universal fallback
cometcli ask "summarize signing health and flag any exposure risks"
```

Supported providers: `anthropic`, `openai`, `openai-compat` (covers Ollama,
vLLM, LM Studio, any OpenAI-shaped endpoint via `--agent-base-url`), or `off`.

The agent sees tool calls as function calls; every on-chain or local-change
call still goes through the same approval gate — the model can propose, only
you approve.

## Safety model

- **Tiers**: `observe → diagnose → local-change → on-chain`. Read-only by
  default; every mutation prompts.
- **Consensus keys are radioactive**: `priv_validator_key.json` is never read
  into memory or sent to a model. Only `priv_validator_state.json` HRS is
  inspected for double-sign guards.
- **Redaction**: secrets, mnemonics, and key material are scrubbed from all
  text entering and leaving the agent.
- **Audit**: every tool call, shell command, prompt, and tx is logged to
  `~/.cometcli/audit/YYYY-MM-DD.jsonl` (`cometcli audit` to tail it).
- **Bounded execution**: every tool runs under a deadline — a dead endpoint
  fails fast instead of hanging.

## Profiles & config

State lives in `~/.cometcli/` (`$COMETCLI_HOME` to override):

```
config.yaml     # profiles + active pointer
keys/           # ops keyring (file backend)
runbooks/       # your custom YAML playbooks
audit/          # JSONL audit trail
```

Key env vars: `COMETCLI_PROFILE`, `COMETCLI_KEYRING_PASSWORD`,
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`.

## Docs

- [docs/RUNBOOKS.md](docs/RUNBOOKS.md) — builtin playbooks + authoring your own
- [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) — endpoints, keyrings, tx
  errors, SSH, state sync, agent config

## Development

```bash
go build ./...          # build all packages
go vet ./...            # vet
go test ./...           # unit tests (keys KAT, redaction, lint, config…)

# e2e (needs a live node):
COMETCLI_E2E=1 COMETCLI_E2E_GRPC=127.0.0.1:9090 \
COMETCLI_E2E_COMET=tcp://127.0.0.1:26657 go test ./test/e2e/...
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues:
[SECURITY.md](SECURITY.md).

## License

Apache-2.0
