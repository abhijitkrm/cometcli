# Command Reference

Every command works identically as a CLI subcommand and as an agent tool.
Tiers: **observe** (read-only) · **diagnose** (host inspection) ·
**local-change** (mutates host, prompts) · **on-chain** (simulate → decode →
approve → broadcast → confirm; always prompts).

Global flags: `--profile <name>` · `--json` · `-y/--yes` (auto-approve
observe/diagnose only — never on-chain) · `COMETCLI_PROFILE`,
`COMETCLI_KEYRING_PASSWORD` env vars.

## Chat

```bash
cometcli                               # general mode: SRE agent on this machine (TUI)
cometcli "prompt"                      # same, starting with a prompt
cometcli one [profile] ["prompt"]      # node mode for a profile (active profile if omitted)
cometcli -p "prompt"                   # headless: print the answer and exit
cmd | cometcli -p "prompt"             # piped input is attached as <stdin> context
cometcli -c / -r <id>                  # continue the latest session here / resume one
cometcli config show                   # global agent settings (general mode; profiles override)
cometcli config set agent.provider anthropic   # or openrouter, openai, groq, gemini, openai-compat
cometcli config set agent.model claude-haiku-4-5-20251001
cometcli config set-key ANTHROPIC_API_KEY      # saves a key (read hidden from stdin) to ~/.cometcli/credentials, 0600
cometcli config set agent.max_turns 300        # model steps per request before it pauses (default 200)
cometcli trust                         # let this project's hooks and MCP servers run
cometcli mcp add|list|remove …         # MCP servers the agent connects to
```

Headless flags: `--output-format text|json|stream-json`, `--include-partial-messages`
(token deltas in stream-json), `--verbose` (tool calls on stderr in text mode),
`--append-system-prompt "…"`, `--max-turns N`. Headless runs never read approvals from
stdin: anything that needs approval is denied unless `--allowedTools` or
`--permission-mode` allows it. An explicit `--profile` on bare `cometcli` also means
node mode.

`json` prints one object: `{"type":"result","subtype":"success|error","is_error",
"result","session_id","num_turns","duration_ms","provider","model","mode","usage"}`.
`stream-json` prints a `system/init` line, then `assistant`, `tool_use`, `tool_result`,
`notice` and `todos` events, then the same `result` object.

### The interactive terminal

`cometcli` and `cometcli one` open an inline chat: answers land in your normal
scrollback (scroll, copy, search as usual); only the bottom area — streaming text,
input, approval dialog, status line — redraws.

| Key / prefix | Does |
|---|---|
| `enter` · `\`+`enter` / `ctrl+j` | send · new line |
| `↑` `↓` | prompt history (or move in a menu) |
| `/` | command menu as you type; `tab` completes, `enter` runs |
| `@path` | file completion; the file's content is attached to your message |
| `!cmd` | run `cmd` **in your terminal** — the UI steps aside, so prompts and passwords work (e.g. `!./upgrade/vote-upgrade.sh`). Output is never sent to the model; it's told the command and exit code. In node mode over SSH it runs on the node (`ssh -t`) |
| `#text` | add a line to memory (`/remember`) |
| `shift+tab` | cycle mode: ops → accept-edits → read-only (→ bypass, if started with it) |
| `esc` | interrupt the running turn (or close a menu); a message you typed meanwhile is sent right away |
| `ctrl+r` | expand the last collapsed tool output |
| `ctrl+o` | show the model's last thinking |
| `ctrl+c` | interrupt · clear input · twice to exit |
| `ctrl+d` | exit (empty input) |
| `?` | shortcut help |

A message typed while a turn runs reaches the model at its next step (after the current
tool call), so you can redirect or stop it mid-task; `esc` interrupts and sends it at
once. Tool output is collapsed to a few lines (`ctrl+r` expands it) and thinking to a
"Thought for Ns" line (`ctrl+o` shows it). If a request runs out of steps it pauses —
say "continue". In general mode the agent moves onto a node profile by itself when a
question needs one; `/one <profile>` pins one. Approvals offer
**Yes**, **Yes, and don't ask again for `<rule>`** (saved to the project's
`.cometcli/settings.local.json`, or your user settings outside a project), and **No**
(declines and interrupts so you can redirect). Transactions offer only Yes/No.

`cometcli ui` still opens the full-screen dashboard (overview, fleet, logs, send).

## Incidents

### Triage and the knowledge base

Validators fail in a few hundred known ways. cometcli carries them as a **knowledge base
of cases** (Cosmos SDK, CometBFT and Cosmos-EVM), and starts every incident with one
deterministic sweep instead of the model guessing which tool to call:

```bash
cometcli node triage                 # ~50 signals in parallel + the known cases they match
cometcli node triage --since 2h      # wider log window
cometcli node triage --signals val.  # just one family
cometcli kb search --query "app hash mismatch"
cometcli kb show --id val-jailed-downtime
```

`node.triage` collects from every source at once — CometBFT RPC, chain gRPC (staking,
slashing, upgrade plan, gov), host (disk, inodes, memory, load, NTP), the process (state,
restarts, OOM kill, exit code), logs (counted by category: panic, OOM, disk, app hash, DB
lock/corruption, privval, height regression, double sign, upgrade halt, state sync, port
in use, config error, chain mismatch, EVM JSON-RPC…), config.toml/app.toml invariants,
priv_validator_state, and on Cosmos-EVM chains the JSON-RPC endpoint. A source that's
down is reported, never fatal. Signals are flat names (`val.jailed`,
`host.disk_used_pct`, `logs.apphash`, `node.key_mismatch`…).

Each **case** says which signals point to it, how to confirm it, causes, fix steps tagged
`[read]` / `[change]` / `[tx]`, how to verify, and what never to do. In node mode,
**`/incident [what you see]`** runs the method: triage → confirm the top case → fix the
root cause first (approvals as usual) → wait with `wait.until` → verify → re-triage →
report. When the agent finds a cause no case covered, it offers to record one with
`kb.add` (you approve the write), so the next triage recognizes it.

Chain family comes from the profile (`metadata.chain_type: cosmos-sdk|cosmos-evm`, else any
EVM endpoint/chain id/`*evm*` binary means cosmos-evm); EVM-only cases are skipped on plain
SDK chains. Add or override cases in `~/.cometcli/kb/*.yaml` or `.cometcli/kb/*.yaml` —
format in [EXTENDING.md](EXTENDING.md#knowledge-base-cases).

`wait.until --condition signal` waits on any triage expression:

```bash
cometcli wait until --condition signal --value "host.disk_used_pct < 85 && proc.running == true"
```

### Incident tools

Tools built for running an incident to resolution — usable directly or by the agent:

```bash
cometcli val jail-check                       # why jailed, root-cause evidence from the node's logs,
                                              # restarts/OOM/disk, and every unjail blocker with its fix
cometcli val consensus --window 50            # bonded, in the validator set, signing recent blocks?
cometcli wait until --condition synced        # block until caught up (live progress, ETA)
cometcli wait until --condition unjailable    # until the jail period is over
cometcli wait until --condition in-consensus --window 50 --min_signed_pct 95
cometcli wait until --condition tx-committed --value <hash>
cometcli wait until --condition proposal-status --value 12:PASSED
```

`wait.until` polls in-process, so an agent waiting on it spends no tokens; it returns
`done=false` with the last state on timeout. `val.unjail` refuses on hard blockers
(jail period not over, node not synced, self-delegation below minimum, no fee balance,
tombstoned) instead of paying for a doomed tx, and every rejected transaction comes back
with a cause → next step hint.

In a node-mode session, **`/recover-jail [notes]`** runs the whole procedure: diagnose →
stop if tombstoned → fix the root cause → wait for sync → wait out the jail → unjail
(you approve) → on failure fix the cause and retry → verify it signs → report.

### Signing transactions

Two signers; when both can sign, you pick one first, then approve the tx (the approval shows which signer will sign):

- **cometcli's keyring** — `signer.key` (import with `cometcli keys add --recover`).
- **the node container's own keyring** — the key never leaves the node: cometcli builds
  and simulates the tx, you approve, then `evmd tx sign` runs inside the container (a file
  keyring's password is asked for then, never stored or sent to the model).

```yaml
signer:
  key: ops                      # cometcli keyring (optional)
  mode: ""                      # "" = ask when both work | local | container
  container: primium-validator0 # default: service.unit for docker services
  container_key: val0           # key name in the container's keyring
  container_keyring: ""         # test | file — detected when empty
  container_home: /data/node0/evmd
```

Headless runs (`-p`) must set `signer.mode`; a container file-keyring password comes from
`COMETCLI_CONTAINER_KEYRING_PASSWORD` there.

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

cometcli val jail-check                # why jailed, evidence from the logs, every unjail blocker
cometcli val unjail                    # MsgUnjail — refused while the jail period runs, the node isn't
                                       # synced, the signer's state is ahead, the node runs another
                                       # consensus key, self-delegation is short or fees are missing
cometcli val vote --proposal 7 --option yes
cometcli val edit --commission-rate 0.05 --moniker new-name
cometcli val withdraw                  # rewards + commission to signer
cometcli val create --amount 1000000uatom \
    --pubkey '{"@type":"/cosmos.crypto.ed25519.PubKey","key":"…"}' \
    --moniker myval --commission-rate 0.05
```

## Governance

```bash
cometcli chain gov                     # proposals in voting (--status deposit|passed|rejected|all)
cometcli val votes                     # open proposals and how this validator voted
cometcli gov propose --kind text --title "…" --summary "…"
cometcli gov propose --kind upgrade --title "v2" --summary "…" --name v2 --in_blocks 20000
cometcli gov propose --kind messages --title "…" --summary "…" \
    --messages '[{"@type":"/cosmos.distribution.v1beta1.MsgCommunityPoolSpend", …}]'
cometcli gov deposit --proposal 7      # tops up exactly what's missing to the minimum
cometcli val vote --proposal 7 --option yes
cometcli wait until --condition proposal-status --value 7:PASSED
```

The deposit defaults to the chain's minimum and is checked against its
`min_deposit_ratio` before you approve. An upgrade height the vote can't reach in time
(voting period at the measured block time) is refused, with the earliest height that
works. Messages are signed with whichever signer you pick (see
[Signing transactions](#signing-transactions)).

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
cometcli chain validators              # bonded set (--status jailed|all|unbonding|unbonded)
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
cometcli ui                            # chat-first TUI: streamed answers,
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
| `/mode [ops\|accept-edits\|readonly\|bypass]` | show or set the approval posture; readonly hides mutating tools from the model |
| `/approve local-change on\|off` | autopilot local-change tools; on-chain can never be autopiloted |
| `/model [name \| provider name]` | show or switch the LLM mid-conversation |
| `/runbook [name]` | list runbooks, or have the agent run one through the normal approval gate |
| `/tools` | every tool with its tier and how this session treats it (auto/confirm/deny) |
| `/profile`, `/audit`, `/reset` | profile + policy, audit path + session id, new session |
| `/compact [focus]` | summarize the conversation to free context (also automatic near the limit) |
| `/cost` | session token usage, cache hits, and current context size |
| `/effort [level\|default]` | show or set reasoning depth for the rest of the session |
| `/sessions`, `/resume <id>` | list saved sessions, or load one into the current session |
| `/permissions`, `/allow\|/ask\|/deny <rule>` | show rules; add one for this session |
| `/memory`, `/remember [--user\|--node] <text>` | memory files loaded; add a line to COMET.md |
| `/mcp`, `/agents` | connected MCP servers; subagents for the task tool |
| `/<custom>` | commands from `.cometcli/commands/*.md` — see [EXTENDING.md](EXTENDING.md) |
| `/incident [what you see]` | node mode: triage → known case → fix root cause → wait → verify → report |
| `/recover-jail [notes]` | node mode: run the full jail-recovery procedure |
| `/one [profile\|off]` | switch the session to node mode for a profile, or back to general (conversation kept) |

### General tools and permissions

Besides the node tools, the agent has general SRE tools that run on the profile's host
(local, or over SSH — same tools either way):

| Tool | What it does |
|---|---|
| `bash` | any shell command; the working directory persists, stdin is closed (prompts get EOF), default timeout 2 min (max 10) |
| `read`, `write`, `edit` | files with line numbers; edits need a fresh read and show a diff for approval |
| `glob`, `grep` | find files by pattern, search contents (ripgrep when installed) |
| `web_fetch` | release notes, docs, a node's REST endpoint; each new domain is approved, localhost isn't |
| `todo_write` | a visible checklist for multi-step work |

Every shell command is classified before it runs: reads (`ls`, `journalctl`, `docker
logs`, `systemctl status`, `evmd q …`, JSON-RPC queries via curl, …) run at once;
anything that changes the host asks; a transaction — `evmd tx …` directly, inside
`docker exec`/`docker run`, or inside a script the command runs — **always** asks, in
every mode. Consensus keys, mnemonics, key ceremonies (`keys add/export`, scripts that
run them) and state resets (`unsafe-reset-all`, deleting `priv_validator_state.json`)
are refused outright: the agent hands those steps to you.

Modes (`--permission-mode`, `/mode`): `ops` (default — changes ask), `accept-edits`
(file edits inside the working directory don't ask), `readonly` (nothing that changes
anything can run; the shell is limited to reads), `bypass` (local changes don't ask;
transactions still do).

Rules refine that per session (`/allow`, `/ask`, `/deny`, `--allowedTools`,
`--disallowedTools`) or per profile (`agent.permissions`). Deny beats ask beats allow,
and an allow must cover every part of a compound command:

```yaml
agent:
  permissions:
    allow: ["bash(docker logs:*)", "bash(./info.sh)", "web_fetch(domain:github.com)", "node.*"]
    ask:   ["bash(docker compose:*)"]
    deny:  ["bash(rm:*)", "edit(/etc/**)", "val.unjail"]
```

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
  tools: ""                      # "all" sends every tool schema each request; default loads on demand
```

Each request carries a core tool set (the general tools plus node.status/health/logs and
val.status/signing) and a catalog of the rest by name; the model loads others with
`tool_search` as it needs them. That keeps a request near 2.5k tokens instead of ~7.5k —
small enough for free-tier per-minute limits — and cheaper on every provider.

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
