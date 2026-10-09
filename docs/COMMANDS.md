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
`[read]` / `[change]` / `[tx]`, how to verify, and what never to do. In any session
(name the node — `/incident val01 is down` — and cometcli switches to it),
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

### History and incident records

Every `node.triage` run is saved to the node's signal history
(`~/.cometcli/history/<profile>/`, kept 30 days), and triage starts by saying what
changed since the previous run (`node.peers 8→1, val.jailed false→true`). For trends:

```bash
cometcli node history                              # key health signals, last 24h
cometcli node history --signals val.,node.peers --since 7d
cometcli mon record --interval 5m                  # sample regularly, not only when someone triages
```

Each answer gives first → last, min/max, a trend line and when the value changed.

When an incident is finished, the agent saves it with `incident.record`: title, root
cause, matching case, evidence, actions and outcome. The timeline (prompts, tool calls,
approvals) and transaction hashes are filled in from the audit log. Records live in
`~/.cometcli/incidents/<profile>/` as JSON plus a Markdown postmortem. Triage
mentions a node's past incidents and flags a case that keeps coming back.

```bash
cometcli incident list                             # last 30 days (--since 90d)
cometcli incident show --id 20261007-0533-val-jailed-downtime
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

Every signature is a **key access** and is approved on its own.
- **What the approval shows**: the account, which key and where it lives, the decoded
  messages, the fee, chain and sequence, and the agent's stated reason.
- **No shortcuts**: no rule, mode, autopilot or earlier approval covers it, and the next
  signature asks again.
- **Gas**: estimated with an unsigned transaction, so the key isn't touched before you
  approve.
- **Remote approvals**: in `watch`, a Telegram approval of a key use expires after
  5 minutes.

Two signers; when both can sign, you pick one first, then approve the key use:

- **cometcli's keyring** — `signer.key` (import with `cometcli keys add --recover`).
- **the node container's own keyring** — the key never leaves the node: cometcli builds
  and simulates the tx, you approve, then `evmd tx sign` runs inside the container (a file
  keyring's password is asked for then, never stored or sent to the model).

```yaml
signer:
  key: ops                      # cometcli keyring (optional)
  mode: ""                      # "" = ask when both work | local | container
  container: validator0 # default: service.unit for docker services
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

## Networks (spec, roles, checks)

A network spec, `~/.cometcli/networks/<chain-id>.yaml`, describes one network:
- chain and EVM ids, denoms
- genesis gov, staking, slashing, feemarket, EVM, mint and distribution parameters
- consensus timing and node services
- DB backend, image, and how images are named

Every node of the chain is checked against it according to its role.

```bash
cometcli network import run-genesis/network-config.env run-validator/network-config.env \
    run-archive/network-config.env --image-naming 'primium-{tag}'    # from node-setup's env files
cometcli network show primium-1
cometcli network check --network primium-1          # spec sanity, chain drift, every node of the chain
cometcli network check --network primium-1 --prod   # plus public-network hardening
cometcli network check --network primium-1 --nodes val1,archive1
```

`network check` reports at three levels:
- **Spec:** mistakes such as an expedited voting period that isn't shorter than the voting
  period. With `--prod` it also flags test-only values: voting period, unbonding,
  LAN-tuned timeouts, `ws_origins *`, the default min deposit.
- **Chain:** the live gov, staking and slashing parameters compared with the spec.
- **Nodes:** each node's `config.toml`, `app.toml` and container command, checked
  against its role (the profile's `role`):

| Role | Checked |
|---|---|
| all | `db_backend` = `app-db-backend` (empty = same) · mempool: `type = "app"` with `max-txs ≥ 0` on cosmos/evm v0.7+, never `"app"` on v0.6 · `minimum-gas-prices` · `evm-chain-id` · enabled services · `--json-rpc.ws-origins` passed as a flag (ws-origins from app.toml is broken in cosmos-evm) |
| validator | consensus timeouts match the spec · `external_address` set · with `--prod`: no CORS, no unsafe-cors, no insecure unlock, swagger off, no `debug`/`personal` |
| archive | `pruning = nothing` (app.toml or `--pruning nothing`) · `tx_index = kv` · with `--prod`: CORS/unlock hardening |
| rpc | `tx_index = kv` · with `--prod`: CORS/unlock hardening |

### Creating a new network

```bash
cometcli network import run-genesis/network-config.env --chain-id mychain-1     # the new network's spec
cometcli profile add v1 --ssh-host 203.0.113.1 --service docker --unit primium-validator --role validator
#   … one profile per genesis validator (build the image on each: upgrade build)
cometcli genesis create mychain-1 --validators v1,v2,v3,v4 --keyring file --accounts treasury.txt
```

`genesis create` runs node-setup's distributed genesis workflow, with cometcli as the
coordinator:

1. **On each validator's own host:** `init` in the image, so the node and consensus keys
   stay there, and the operator key goes into the node's own keyring.
2. **Base genesis**, built from the spec:
   - staking, gov, slashing, mint, distribution and feemarket parameters
   - EVM precompiles (by name, sorted), access control and erc20
   - denom metadata and block gas
   - every validator funded, plus `--accounts` (lines of `<address> <amount>`, bech32 or 0x)
3. **Each gentx signed on its own host.** Only the gentx comes back.
4. **`collect-gentxs` and strict `validate-genesis`.**
5. **Start every node:**
   - the final genesis is installed everywhere, and also saved for later joins
   - configs are rendered for the validator role, with every other validator as a peer
   - compose files are written and every node starts
6. **Wait for blocks.**

Mnemonics are shown only on your terminal, or written to `--mnemonics-to <dir>`
(mode 0600). Existing node homes are never overwritten. Several validators on one machine:
`--docker-network <net> --port-offset N --port-step 10 --home '~/nodes/{profile}'`.
Back up each validator's `priv_validator_key.json` off-machine afterwards.

### Adding nodes to a network

```bash
cometcli network import --from-node val1                   # spec + genesis.json from a running node
cometcli profile add arch1 --ssh-host 203.0.113.9 --service docker --unit primium-archive --role archive
cometcli --profile arch1 node provision --peers_from val1,val2         # init, genesis, configs, compose, start
cometcli --profile arch1 node provision --reconfigure                  # re-render configs of an existing node
```

`node provision` sets up a node on the profile's host, local or over SSH, following the
spec:
1. Checks the image is on the host. If not, build it first with `upgrade build`.
2. Checks the node home is writable. If not, it prints the one-time `sudo chown`.
3. Runs `init` in the image as the home's owner.
4. Installs the network's genesis (from `--from-node`).
5. Renders `config.toml`, `app.toml` and `client.toml` for the role. Only the needed keys
   change; comments stay. The result passes `network check`.
6. Writes `docker-compose.yml` into the node home and starts it:
   - the command includes `--pruning nothing` for archives and the ws-origins flag
   - validators publish RPC, REST, gRPC and EVM on 127.0.0.1 only; P2P is public
   - the stop grace period is 60s
7. Updates the profile.

Peers come from `--peers` or from running nodes via `--peers_from`. For several nodes on
one machine, use `--docker_network <net> --port_offset N`.

**It refuses a home that already holds a validator key**, since re-initialising would replace
its keys and state (a double-sign risk). `--reconfigure` re-renders configs only.

For a validator, once it's synced:
1. The operator creates its key in the node's own keyring, in their terminal.
2. Fund it.
3. Run `cometcli --profile <p> val create --amount <stake>`. The consensus key and moniker come
   from the node, and commission is encoded the way the chain's cosmos-sdk version expects.

## Remote nodes over SSH

Run cometcli on your laptop and manage a node on another machine. Every host
operation (service control, logs, files, `docker exec`, the agent's `bash`) runs over
one SSH connection, and endpoints that listen on the node's own localhost — RPC
`127.0.0.1:26657`, gRPC `127.0.0.1:9090`, JSON-RPC `127.0.0.1:8545` — are reached
through that connection. Nothing has to be opened in the firewall.

```bash
cometcli init                                          # pick "ssh": connects, trusts the host key, discovers on the node
cometcli profile add val01 --ssh-host val01            # a ~/.ssh/config alias: HostName, User, Port, IdentityFile, ProxyJump
cometcli profile add val01 --ssh-host 203.0.113.7 --ssh-user ec2-user --ssh-key ~/keys/val.pem
cometcli profile add val01 --ssh-jump ops@bastion:22   # through a bastion (ssh -J)
cometcli ssh test val01                                # connect, trust the host key, check docker/systemd/logs/home/ports
```

- **Host and settings**: `--ssh-host` takes an address or an alias from
  `~/.ssh/config`. User, port, key and ProxyJump come from there unless the profile sets
  them. If nothing names a user, `$USER` is used.
- **Keys**: tried in this order:
  1. the profile's `--ssh-key`
  2. `IdentityFile` from `~/.ssh/config`
  3. `~/.ssh/id_ed25519`, `id_ecdsa` and `id_rsa`
  4. ssh-agent

  A passphrase-protected key is asked for in `init` and `ssh test`. Everywhere else
  (the terminal UI, `watch`), load it with `ssh-add`.
- **Host keys**: checked against `~/.ssh/known_hosts`, or `UserKnownHostsFile` when
  `~/.ssh/config` sets it.
  - A new host must be trusted once, with `cometcli init`, `cometcli ssh test` or plain
    `ssh`.
  - A changed key is refused. Remove the old entry with `ssh-keygen -R <host>` only
    after checking the new key.
- **Connection**: kept alive, and redialed once if it drops (reboot, NAT timeout).
- **Endpoints**: an endpoint naming the SSH host itself (e.g. its public IP) is also
  reached as the node's localhost.

The SSH user needs:
- **docker nodes**: membership in the `docker` group.
- **systemd nodes**: membership in `systemd-journal` (or `adm`) for logs, and
  passwordless sudo for `systemctl`.

`cometcli ssh test` checks each of these.

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
cometcli fleet triage                              # every node triaged at once, then compared
cometcli fleet triage --chain mychain-1            # one chain only
cometcli fleet status                              # health matrix: height, peers, signing, disk
cometcli fleet exec --tool node.status             # run a read-only tool everywhere
cometcli fleet exec --tool node.logs --args '{"lines":20}'
cometcli fleet exec --tool val.signing --profiles val01,val02
cometcli fleet shell "uptime"                      # shell on every host (one approval)
```

`fleet triage` gives one row per node (height, peers, signing, jailed, top case), then
what only a comparison shows:

- a node behind the others on its chain
- a chain-wide halt: every node stalled, so the problem is the chain, not one node
- version mismatches between nodes
- two nodes running the same consensus key (double-sign risk)
- validators sharing a host
- a validator whose sentries are down

Tell cometcli which sentries a validator peers through so it can check them:
`cometcli profile add val01 --sentries sentry1,sentry2`. In a session, questions about
several nodes ("how is my network?") start with `fleet.triage`, in general mode too.

## Watching on your behalf

```bash
cometcli watch                                     # every node, every 5m, diagnose mode
cometcli watch --mode fix --interval 2m            # may act — approvals go to Telegram
cometcli watch --once --mode notify                # one sweep, report only (cron-friendly)
```

Each sweep is a fleet triage. A problem that newly appears (a critical or high case on
a node, or a chain halt, double-sign risk or cut-off validator) becomes an incident:

| mode | what happens |
|---|---|
| `notify` | the finding is reported |
| `diagnose` (default) | the agent works the incident read-only and reports its findings |
| `fix` | the agent may act; **each approval is sent to Telegram with Approve / Deny buttons** |

The watcher reports findings, agent reports and recoveries. It doesn't work the same
problem again within the cooldown (default 1h), and remembers what it saw across
restarts.

```yaml
# ~/.cometcli/config.yaml
watch:
  interval: 5m
  mode: diagnose            # notify | diagnose | fix
  profiles: [val01, val02]  # default: all
  cooldown: 1h
  approval_timeout: 15m     # an unanswered approval is a "no"
  allow_tx: false           # transactions are refused in watch mode unless true
  telegram_users: [123456]  # only these Telegram users' presses count
  alerts:
    slack_webhook: https://hooks.slack.com/…     # reports only
    discord_webhook: https://discord.com/api/webhooks/…
    telegram_chat_id: "-100123456"               # reports and approvals
```

Save the Telegram bot token with `cometcli config set-key TELEGRAM_BOT_TOKEN`, or put it
in `watch.alerts.telegram_token`. Only presses from that chat, and from
`telegram_users` if set, count. Slack and Discord get reports but can't answer
approvals.

## Incident drills

Check how well cometcli handles real faults, on a throwaway network:

```bash
cometcli drill testnet up --image <evmd image>     # 4 local validators in docker, fast slashing
cometcli drill list                                # the scenarios
cometcli drill run                                 # all of them (--scenario oom,partition to pick)
cometcli drill testnet down                        # remove containers, data and drill profiles
```

Each scenario breaks a drill node for real:

- stopped until it's jailed for downtime
- `config.toml` broken (crash loop)
- `halt-height` set
- container memory limit too low (OOM)
- network detached

For each one, the drill checks whether triage named the right root cause, then lets the
agent work it with `/incident`, verifies the fix on the node itself, and puts the node
back to health. The scoreboard shows triage x/y and fixed x/y, with steps, tokens and
time per scenario. Results are saved in `~/.cometcli/drills/`, so model or prompt
changes can be compared run to run. Approvals are granted automatically, **but only on
drill profiles** (`metadata.drill: "true"`); anything else is refused.

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

### Local answers and the model

Every prompt is classified on your machine first. The model is used only when cometcli
can't work out what to do.

| What you type | Who handles it |
|---|---|
| **A direct command:** "restart val3", "stop the node", "unjail", "vote yes on 6", "send 100adex to cosmos1…", "delegate 1000adex to cosmosvaloper1…", "withdraw rewards", "show last 200 logs of val4", "add peer id@host:26656", "set app mempool.max-txs 0", "build v0.7.3", "switch to primium-v0.7.3", "check the network", "triage" | cometcli runs the tool with the words as its arguments. Changes and transactions still ask, and key use always does. A name that isn't one of your nodes ("stop worrying") goes to the model. |
| **A known question:** jailed validators, validator count, peers, sync and lag, health (from triage), fleet status, version, upgrade readiness, recent log errors, spec conformance, uptime, consensus, proposals, votes, rewards, balance, upgrade plan, slashing params | Read-only tools and a fixed answer. Repeats within 30s come from a cache. |
| **`/incident` where triage finds one clear root case with a playbook** (32 of 68 cases) | cometcli runs the playbook (see below). |
| **Anything else:** why/how questions, ambiguous requests, judgement calls (config parse errors, app hash, double sign, corruption) | The model. For `/incident` it starts from cometcli's triage. |

The playbooks behind `/incident`:
- **Fixes:**
  - mempool type for the node's version
  - DB backend set from the data on disk
  - gRPC disabled
  - network detached (recreate the container)
  - no or low peers (your other nodes of the chain)
  - home permissions, NTP
  - EVM drift or indexer behind
  - catching up, jail periods, signer state ahead, recent restarts
  - nodes that are down, killed, jailed, halted or OOM-limited
- **Reports:** for votes, fees, the active set, self-delegation, disk, load and upgrades,
  cometcli gathers the facts and tells you what to decide. It never votes or spends for you.

**It learns.** When the model fixes a known case that has no playbook yet, cometcli shows the
steps it took and offers to save them as that case's playbook in `~/.cometcli/kb/`. The next
identical incident then runs without the model.

`cometcli route stats [--since 7d]` shows the share answered locally and what the model was
asked most, which is the list of what to teach cometcli next. Drills report a
"without the model" score. `/llm <question>` asks the model anyway, `/route` explains the
last decision, and `agent.router: off` turns routing off. With no model configured, known
questions, commands and playbooks still work.

**What the model sees.** On validator profiles, `agent.egress: strict` is the default:
- Output of `bash`, `read`, `grep`, `node.logs` and `fleet.exec` reaches the model only
  as a local summary.
- Addresses, IPs, hashes, hostnames and blobs are masked, and repeated lines are folded
  with a count.
- Errors are kept first when output is long.
- Your screen and the audit log still show the raw output.

RPC/sentry profiles and general mode default to `filtered` (raw output after redaction).
In every mode, anything carrying key material is withheld whole.

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
| `/incident [what you see]` | triage → known case → fix root cause → wait → verify → record → report; name the node to switch to it |
| `/recover-jail [notes]` | the full jail-recovery procedure (name the node, or the agent picks it) |
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
  egress: ""                     # strict | filtered — default strict on validators (raw output only as local summaries)
  router: on                     # answer known questions and known incidents locally (off = always the model)
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
