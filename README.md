# cometcli

**An agentic SRE terminal for Cosmos-EVM validators.** Local-first, single
binary — deterministic CLI on the outside, LLM agent on the inside.

```bash
cometcli doctor                    # full validator health checklist
cometcli val signing               # missed blocks + uptime over the window
cometcli node config --action lint # audit config.toml against validator baseline
cometcli val unjail                # simulate → confirm → broadcast MsgUnjail
cometcli agent                     # interactive AI SRE (or `cometcli ask "..."`)
```

Every capability is a deterministic subcommand **and** a tool the agent can
call — one tool registry, two front-ends. See [PLAN.md](PLAN.md) for the
architecture and roadmap.

## Install

```bash
go install github.com/abhijitkrm/cometcli/cmd/cometcli@latest
# or grab a signed release from GitHub Releases
```

## Quickstart

```bash
# 1. register your node
cometcli profile add myval \
  --chain-id mychain-1 --evm-chain-id 262144 --bech32-prefix mychain \
  --role validator --home ~/.mychaind --binary mychaind \
  --comet tcp://127.0.0.1:26657 --grpc 127.0.0.1:9090 \
  --transport ssh --ssh-host val.internal --ssh-user ops \
  --service systemd --unit mychaind.service \
  --signer ops --fee-denom atest
cometcli profile use myval

# 2. add an ops key (consensus keys are never touched)
cometcli keys add --name ops          # generates, or --recover to import

# 3. check health
cometcli doctor
```

## Safety model

- **Tiers**: `observe → diagnose → local-change → on-chain`. Read-only by
  default; every write prompts; every tx shows a decoded simulation and
  requires explicit approval.
- **Consensus keys are radioactive**: `priv_validator_key.json` is never read
  into memory or sent to an LLM. Only `priv_validator_state.json` HRS is
  inspected for double-sign guards.
- **Redaction**: secrets, mnemonics, and key material are scrubbed before any
  context reaches a model.
- **Audit**: every tool call, shell command, prompt, and tx is logged to
  `~/.cometcli/audit/*.jsonl`.

## Layout

See [PLAN.md](PLAN.md). Tool catalog under `internal/tools/<domain>/`,
shared tool interface in `internal/toolkit/`, data-plane clients in
`internal/client/{comet,grpc,evm,host}/`.

## License

Apache-2.0
