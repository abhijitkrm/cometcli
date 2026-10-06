# Command Reference

Every command works identically as a CLI subcommand and as an agent tool.
Tiers: **observe** (read-only) · **diagnose** (host inspection) ·
**local-change** (mutates host, prompts) · **on-chain** (simulate → decode →
approve → broadcast → confirm; always prompts).

Global flags: `--profile <name>` · `--json` · `-y/--yes` (auto-approve
observe/diagnose only — never on-chain) · `COMETCLI_PROFILE`,
`COMETCLI_KEYRING_PASSWORD` env vars.

## Setup

```bash
cometcli init                        # guided wizard — discovers endpoints, ports, home, binary, gas price
cometcli doctor                      # full health checklist (run this first)
cometcli doctor --profile val02      # checklist on another profile
```

## Profiles & keys

```bash
cometcli profile list                                  # all profiles, active marked *
cometcli profile show val01                            # one profile as YAML
cometcli profile use val02                             # switch active profile
cometcli profile add val02 --chain-id mychain-1 \
    --comet tcp://127.0.0.1:26657 --grpc 127.0.0.1:9090 \
    --home ~/.evmd --binary evmd --service docker --unit my-validator
cometcli profile add val02 --valoper cosmosvaloper1... # read-only signing views, no key needed
cometcli profile rm oldval

cometcli keys list                                     # ops keys (never consensus keys)
cometcli keys add --name ops                           # generate new key
cometcli keys add --name ops --recover                 # restore from mnemonic
cometcli keys add --name ops --privkey-hex <hex>       # import raw key
cometcli keys show ops                                 # bech32 + hex addresses
cometcli keys convert cosmos1abc...                    # bech32 ↔ 0x hex
cometcli keys rm ops
```

## Node (host + CometBFT)

```bash
cometcli node status                   # height, version, peers, voting power
cometcli node health                   # rpc /health + service liveness
cometcli node peers                    # peer list with monikers
cometcli node consensus                # height/round/step + prevote/precommit bitmaps
cometcli node logs --lines 100         # tail service logs (docker/journald)
cometcli node logs --lines 500 --grep "error|panic"
cometcli node config                   # dump config.toml/app.toml
cometcli node config diff              # vs hardened baseline
cometcli node version-check            # running binary vs upstream release
cometcli node service status           # docker inspect / systemctl / launchctl
cometcli node service restart          # prompts before restarting
```

## Validator (on-chain + signing)

```bash
cometcli val status                    # bonded/jailed, tokens, commission, signing info
cometcli val status --validator cosmosvaloper1...     # any validator
cometcli val signing                   # missed-block counter + uptime %
cometcli val rewards                   # outstanding rewards + commission
cometcli val votes                     # active proposals vs recorded votes

cometcli val unjail                    # MsgUnjail (fails at simulation if still jailed)
cometcli val vote --proposal 7 --option yes
cometcli val edit --commission-rate 0.05 --moniker new-name
cometcli val withdraw                  # rewards + commission to signer
cometcli val create --amount 1000000uatom \
    --pubkey '{"@type":"/cosmos.crypto.ed25519.PubKey","key":"…"}' \
    --moniker myval --commission-rate 0.05
```

## Transactions

```bash
cometcli tx send --to cosmos1abc... --amount 1000000uatom
cometcli tx delegate --valoper cosmosvaloper1... --amount 5000000uatom
cometcli tx undelegate --valoper cosmosvaloper1... --amount 1000000uatom
cometcli tx redelegate --from cosmosvaloper1... --to cosmosvaloper1... --amount 500000uatom
cometcli tx get A1B2C3...              # look up a committed tx by hash
```

All tx commands accept: `--gas-price` · `--fee-denom` · `--gas-limit` ·
`--memo` · `--seq` / `--acc-num` (offline signing).

## Chain (gRPC queries)

```bash
cometcli chain validators              # bonded set
cometcli chain params                  # slashing window, unbonding, inflation
cometcli chain pool                    # bonded vs unbonded stake
cometcli chain gov                     # proposals in voting period
cometcli chain balance                 # signer balance
cometcli chain balance --address cosmos1abc...
cometcli chain upgrade-plan            # pending on-chain upgrade
```

## EVM (JSON-RPC)

```bash
cometcli evm chainid                   # eth_chainId vs profile's expected id
cometcli evm parity                    # eth_blockNumber vs CometBFT height (lag detector)
cometcli evm gasprice                  # current eth_gasPrice in wei
cometcli evm syncing                   # eth_syncing progress
cometcli evm txpool                    # pending/queued EVM tx counts
```

## Fleet (all profiles at once)

```bash
cometcli fleet status                              # health matrix: height, peers, signing, disk
cometcli fleet exec --tool node.status             # run a read-only tool everywhere
cometcli fleet exec --tool node.logs --args '{"lines":20}'
cometcli fleet exec --tool val.signing --profiles val01,val02
cometcli fleet shell "uptime"                      # shell on every host (one approval)
```

## Monitoring

```bash
cometcli mon watch                     # live dashboard (q to quit)
cometcli mon snapshot                  # one-shot snapshot — scriptable
cometcli mon alerts                    # long-running watcher → Slack/Discord/Telegram webhooks
cometcli mon alerts --once             # evaluate rules once, exit (cron-friendly)
cometcli mon alerts --mute disk,unreachable --repeat-minutes 30
cometcli mon alerts --triage           # AI-diagnose each alert before it pages (needs agent.provider)
```

Webhook sinks: `COMETCLI_ALERT_SLACK`, `COMETCLI_ALERT_DISCORD`,
`COMETCLI_ALERT_TELEGRAM` + `COMETCLI_ALERT_TELEGRAM_CHAT`.

## Snapshots & state sync

```bash
cometcli snap list                     # local snapshots + data-dir size
cometcli snap prune                    # pruning advice from actual growth
cometcli snap statesync                # print proposed [statesync] config
cometcli snap statesync --apply \
    --rpc "tcp://peer1:26657,tcp://peer2:26657"   # write config (needs >=2 RPCs)
```

Note: `rpc_servers` must be reachable **from the node** — docker nodes need
`host.docker.internal` or peer container names, and peers must actually
produce snapshots (`snapshot-interval > 0` in their app.toml).

## Security

```bash
cometcli sec exposure                  # listening sockets vs exposure policy
cometcli sec perms                     # key material + config file permissions
cometcli sec doublesign                # priv_validator_state HRS + risky config
```

## Network (config.toml peers)

```bash
cometcli net add-peer --peer "nodeid@host:26656"
cometcli net rm-peer --peer "nodeid@host:26656"
```

## Upgrades

```bash
cometcli upgrade check                 # on-chain plan + local version + latest release
cometcli upgrade watch --height 123456 # wait until upgrade height
cometcli upgrade prepare --name v2 --repo myorg/mychain \
    --tag v2.0.0 --checksum <sha256>   # download → verify → stage for cosmovisor
```

## Runbooks (guided playbooks)

```bash
cometcli runbook list                  # available playbooks
cometcli runbook show jail-recovery    # audit steps without running
cometcli runbook run jail-recovery     # step-by-step with approvals
cometcli runbook run halt-recovery
cometcli runbook run host-migration    # move to new host without double-signing
cometcli runbook run statesync-bootstrap
cometcli runbook run coordinated-upgrade
```

## Interfaces

```bash
cometcli                               # bare command = chat TUI (when a profile exists)
cometcli ui                            # chat-first TUI (Claude Code style): streamed answers,
                                       # /run tools, approval modals with diffs, txs broadcast
                                       # from the chat. Overview/fleet/logs/send panes on tabs
cometcli serve --open                  # same agent in a local web chat (127.0.0.1 only,
                                       # token link printed at start; approvals as modals)
cometcli agent                         # line-mode REPL (needs ANTHROPIC_API_KEY, Groq, or Ollama)
cometcli agent --task "check fleet"    # headless one-shot task (cron/CI)
cometcli agent --task "audit exposure" --mode readonly --budget 6 --max-iter 4
cometcli ask "is my validator healthy" # one-shot agent question (streams)
cometcli ask "why is disk at 88%"      # chains tools: doctor → df → verdict
cometcli ask "add peer …" --autopilot local-change   # skip local-change confirms (never on-chain)
cometcli mcp                           # serve all tools over MCP (stdio) for external agents
cometcli audit                         # today's audit log (prompts, LLM turns, tools, txs, approvals)
cometcli audit sessions [--all]        # agent sessions recorded in the log
cometcli audit replay <session-id>     # reproduce a session offline: recorded LLM turns +
                                       # tool output re-driven through the agent loop
cometcli sessions [--all]              # saved agent conversations, newest first
cometcli completion zsh                # shell completion
cometcli version
```

Agent flags (`agent`, `ask`, `serve`): `--mode ops|readonly`, `--safe` (=readonly),
`--autopilot local-change`, `--budget N` tool calls per turn, `--max-iter N` (default 50), `--no-stream`.

Session flags (bare `cometcli`, `ui`, `agent`, `ask`): `-c/--continue` resumes the latest
session started in this directory, `-r/--resume <id>` a specific one (an id prefix works),
`--model`, `--effort low|medium|high|xhigh|max`, `--max-tokens N`. Every conversation is
saved after each turn to `~/.cometcli/sessions/` (0600, already redacted).

### Agent session commands (TUI, REPL, web)

| Command | Effect |
|---|---|
| `/mode [ops\|readonly]` | show or set the approval posture; readonly hides mutating tools from the model |
| `/approve local-change on\|off` | autopilot local-change tools; on-chain can never be autopiloted |
| `/model [name \| provider name]` | show or switch the LLM mid-conversation |
| `/runbook [name]` | list runbooks, or have the agent run one through the normal approval gate |
| `/tools` | every tool with its tier and how this session treats it (auto/confirm/deny) |
| `/profile`, `/audit`, `/reset` | profile + policy, audit path + session id, new session |
| `/compact [focus]` | summarize the conversation to free context (also automatic near the limit) |
| `/cost` | session token usage, cache hits, and current context size |
| `/effort [level\|default]` | show or set reasoning depth for the rest of the session |
| `/sessions`, `/resume <id>` | list saved sessions, or load one into the current session |

### Agent config (per profile)

```yaml
agent:
  provider: anthropic            # anthropic | openai | groq | gemini | openai-compat/ollama | off
  model: claude-opus-5-5         # default per provider when omitted
  mode: ops                      # ops (default) | readonly
  autopilot: [local-change]      # optional; on-chain is rejected
  redact_hosts: [val.internal]   # masked before any text reaches the LLM
  redact_endpoints: true         # also mask this profile's endpoint/SSH hosts
  no_stream: false               # set true for endpoints that mishandle streaming
  effort: medium                 # reasoning depth; mapped per provider (see below)
  max_tokens: 0                  # output cap per round; 0 = provider default
  max_turns: 50                  # model rounds per prompt
  context_window: 0              # override the model's context size (tokens)
  compact_at: 0                  # prompt size that triggers auto-compaction; 0 = 80% of window, ≤200k
```

`effort` maps to each provider's own control: Anthropic `output_config.effort`, OpenAI
and Groq `reasoning_effort` (`xhigh`/`max` → `high`; reasoning models only), Gemini
`generation_config.thinking_level`. Leave it unset for models without a reasoning knob.

Long sessions stay healthy on every provider: transient API failures (429, 5xx,
overloaded) are retried with backoff honoring `Retry-After`; a reply cut off at the
output limit is continued automatically; the system prompt is frozen per session (later
node snapshots ride on your messages) so provider prompt caches stay warm; and when
the prompt nears `compact_at`, the conversation is summarized at the next turn boundary.

`COMETCLI_OFFLINE=1` disables the agent entirely (ask/agent/serve chat refuse); every
tool subcommand and `cometcli mcp` keep working. Mnemonics (checked against the BIP-39
wordlist), hex/base64 key material, tokens and URL credentials are scrubbed from user
input, the live snapshot, and tool output before they reach the model.
