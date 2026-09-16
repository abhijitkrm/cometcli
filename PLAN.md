# cometcli — Build Plan

**An agentic SRE terminal for Cosmos-EVM validators.** Deterministic CLI on the
outside, LLM agent on the inside, one shared tool layer underneath. Runs locally
for independent validators and validator companies managing fleets across
[cosmos/evm](https://github.com/cosmos/evm)-based chains (`evmd` and friends).

Status: greenfield. Language: **Go**. License: Apache-2.0 (matches upstream).

---

## 1. Product definition

cometcli is to a cosmos-evm validator what an agentic coding terminal is to a
developer: you sit in a REPL, describe what you want in plain English
("why is my validator missing blocks?", "get me ready for the v3 upgrade",
"unjail and tell me why I was jailed"), and the agent composes real diagnostic
and operational steps against your node — with strict safety gates.

Crucially, it is **also a normal CLI**. Every capability the agent can call is
exposed as a deterministic subcommand (`cometcli val status`,
`cometcli doctor`, `cometcli tx unjail`), evmd-style. The agent is a layer over
the tool registry, never a separate implementation.

### Non-goals

- Not a wallet / portfolio tool.
- Not a chain-development framework (that's `evmd` itself).
- Not a hosted service — local-first, single binary, user-owned keys.
- Not a replacement for Prometheus/Grafana long-term; it's the on-the-fly SRE.

### Two node topologies we must handle

Per the official cosmos-evm docs, production validators must **not** expose
JSON-RPC/API/gRPC. So cometcli always distinguishes:

- **Validator node** — private, CometBFT RPC on localhost only, consensus keys,
  no `eth_*` surface. Managed via local exec or SSH.
- **RPC/sentry node** — serves `eth_*` JSON-RPC, gRPC, LCD. Used for chain-side
  reads and EVM-plane checks.

Every profile records which role a node has, and tools behave accordingly.

---

## 2. Design principles

1. **One tool registry, two front-ends.** Every capability is a Go `Tool` with a
   name, JSON-schema args, a risk tier, and a `Run()` method. Cobra commands and
   agent function-calls are both thin wrappers. No logic lives in the CLI layer
   or the prompt — it's all in the registry.
2. **Safety tiers over vibes.** Each tool declares a tier:
   `observe → diagnose → local-change → on-chain`. Default mode is read-only;
   every write requires confirmation; every on-chain op requires simulation +
   explicit human approval. `--yes` never bypasses tier-3 confirmation unless
   `--i-know-what-im-doing`-style flags are set per-invocation.
3. **Consensus keys are radioactive.** `priv_validator_key.json` is never read
   into memory, never sent to an LLM, never moved by the tool. Ops tx signing
   uses a separate ops keyring. `priv_validator_state.json` is read only for
   double-sign guards (HRS checks before migrations/restarts).
4. **Redact before you send.** A redaction middleware strips mnemonics, private
   keys, file contents of key material, and (optionally) IPs/hostnames before
   any context reaches a model.
5. **Single static binary, no daemon.** Long-running behavior is opt-in
   (`cometcli watch`); nothing phones home.

---

## 3. Data planes

cometcli speaks to four surfaces:

| Plane | Endpoint | Used for |
|---|---|---|
| CometBFT RPC | `:26657` | `status`, `health`, `net_info`, `dump_consensus_state`, `validators`, `block(_results)`, `abci_info`, WS subscriptions |
| Cosmos gRPC / LCD | `:9090` / `:1317` | staking, slashing (`signing_info` → missed blocks), distribution (rewards/commission), gov, upgrade plan, bank, mint, tx `Simulate`/`Broadcast` |
| Ethereum JSON-RPC | `:8545`/`:8546` | `eth_syncing`, `eth_blockNumber` vs comet height drift, `eth_gasPrice`, `net_peerCount`, `txpool_status`, `web3_clientVersion` — only where exposed (RPC nodes/sentries) |
| Host | local / SSH | systemd units + `journalctl`, docker, `~/.evmd` file layout (`config.toml`, `app.toml`, `priv_validator_state.json`, `data/`), binary version, disk/CPU/mem, NTP sync |

Transport layer: `local` exec or pooled SSH connections, selected per profile.
All host access goes through one `Host` interface so tools are transport-
agnostic.

---

## 4. The tool catalog (heart of the project)

```go
type Tool interface {
    Name() string                    // "val.status"
    Schema() jsonschema.Schema       // args/result for LLM function-calling
    Tier() Tier                      // Observe|Diagnose|LocalChange|OnChain
    Run(ctx Context, args Args) (Result, error)
}
```

Everything below ships as both `cometcli <cmd>` and an agent function.

### `node.*` — node lifecycle & inspection
- `node.status` — sync state, height, catching_up, version, node info.
- `node.health` — comet `/health` + process + service state.
- `node.config.show|diff|lint` — parse `config.toml`/`app.toml`; diff against
  hardened validator baseline; flag dangerous settings (e.g. JSON-RPC enabled
  on a validator profile, `unsafe` peers, `double_sign_check_height` misused).
- `node.logs` — journalctl/docker tail with level filters.
- `node.service.start|stop|restart|status` (local-change).
- `node.peers` — net_info peer quality, persistent-peers drift, addrbook sanity.
- `node.version.check` — binary version vs upstream git releases.

### `val.*` — validator operations
- `val.status` — bonded? jailed? tombstoned? voting power, commission.
- `val.signing` — slashing `signing_info`: missed-block counter, uptime over
  the signed-blocks window, index offset, tombstone check.
- `val.rewards` — outstanding rewards + commission, withdraw-address.
- `val.votes` — active gov proposals vs this validator's votes (missed-vote
  alerts).
- `val.unjail` / `val.edit` (commission, moniker, min-self-delegation) /
  `val.withdraw-rewards` / `val.set-withdraw-addr` — all on-chain tier.

### `chain.*` — network state
- `chain.params`, `chain.validatorset`, `chain.pool`, `chain.inflation`.
- `chain.gov.list|show`, `chain.upgrade.plan` — pending software-upgrade
  proposals and on-chain upgrade plan (height, name, binary needed).

### `evm.*` — EVM-plane checks
- `evm.parity` — `eth_blockNumber` vs comet latest height (drift = indexer/JSON-RPC lag).
- `evm.syncing`, `evm.gasprice`, `evm.txpool`, `evm.clientversion`.
- `evm.chainid` — sanity-check cosmos chain-id ↔ EIP-155 chain-id mapping
  (e.g. `cosmos_262144-1` ↔ `262144`); a classic misconfig.

### `keys.*` — ops keyring only
- `keys.add|list|show|delete` over OS keyring / test backend; `keys.convert`
  bech32↔hex. **No consensus-key import path exists in the codebase.**

### `tx.*` — the transaction pipeline (used by all on-chain tools)
- Build → `Simulate` (gas + result) → render decoded tx JSON → confirm gate →
  sign with ops keyring → `Broadcast` → poll for inclusion.
- `--generate-only` for air-gapped workflows; `--dry-run` prints unsigned tx.

### `upgrade.*`
- `upgrade.check` — on-chain plan + github releases + local binary version.
- `upgrade.prepare` — download binary, verify checksum/signatures, stage into
  cosmovisor `upgrades/<name>` dir (or `cosmovisor add-upgrade`).
- `upgrade.watch` — monitor height, alert before/after switchover.

### `snap.*` — state management
- `snap.list|create|restore` — local snapshots, snapshot-server discovery.
- `snap.statesync` — fetch trust height/hash from RPC, write config, verify.
- `snap.prune` — advisor for pruning strategy (`app.toml` pruning settings)
  based on disk growth rate.

### `mon.*` — monitoring & alerts
- `mon.watch` — live TUI: height, missed blocks (window), peers, disk, mem,
  jail status, upgrade countdown.
- `mon.alerts` — rule engine → webhooks (Slack/Discord/Telegram/PagerDuty):
  missed-block threshold, jailed, height stall, disk watermark, version drift.
- `mon.metrics` — scrape `:26660/metrics` for comet prometheus counters.

### `sec.*` — audits
- `sec.exposure` — port scan of own node: flags 8545/9090/1317/26657 bound to
  public interfaces on a validator profile.
- `sec.perms` — file permissions on `priv_validator_key.json`, keyring dir,
  config dir (0600/0700 checks).
- `sec.doublesign` — priv_validator_state HRS inspection; refuses to run
  destructive ops that could create a second signer.

### `runbook.*` — codified procedures
Multi-step, guarded playbooks the agent (or operator) can invoke by name:
`jail-recovery`, `halt-recovery`, `coordinated-upgrade`, `host-migration`
(with double-sign choreography: stop old → verify state → provision new →
start), `statesync-bootstrap`, `emergency-peer-isolation`. Each step declares
pre/post-conditions checked via tools; aborts loudly on guard failure.

### `fleet.*`
- Multi-node profiles, `fleet status` fan-out over SSH, per-node health matrix,
  grouped by chain. For validator companies managing many chains.

---

## 5. Agent mode

```
┌─ TUI (bubbletea): streaming chat, tool-call cards, diffs, /commands ─┐
│  user → context snapshot → LLM(provider) → tool calls → answer       │
│                    ↑ redaction middleware   ↑ approval engine        │
│                    ↓ audit log (JSONL)                               │
└──────────────────────────────────────────────────────────────────────┘
```

- **Entry**: bare `cometcli` opens the REPL; `cometcli ask "..."` for one-shot.
  Slash commands: `/profile`, `/mode readonly|ops`, `/approve`, `/audit`,
  `/runbook`, `/model`.
- **Context snapshot**: on session start, inject a *bounded* digest — chain-id,
  evm chain-id, node version, validator status, height, peers, disk — refreshed
  lazily, never key material.
- **Tools**: agent functions are generated 1:1 from the tool registry schemas.
  The model cannot reach anything that isn't a registered tool.
- **Approval engine**: tier policy per session. `observe`/`diagnose` auto-run;
  `local-change` shows a diff + confirm; `on-chain` always shows the simulated
  tx and requires explicit approval. Autopilot flags exist per-tier, off by
  default.
- **Redaction**: stream filter on both directions — scrubs mnemonics
  (BIP-39 shape), hex/bech32 key material, `priv_validator*` contents,
  API keys, and profile-flagged hostnames/IPs before context leaves the box.
- **Audit log**: append-only JSONL at `~/.cometcli/audit/` — every prompt,
  tool call + args + result digest, shell command, and tx bytes. Compliance-
  grade trail for validator companies; `cometcli audit replay <id>` reproduces
  a session deterministically.
- **Providers**: Anthropic, OpenAI, and any OpenAI-compatible endpoint
  (Ollama, llama.cpp, vLLM) for air-gapped/local inference. Model + keys per
  profile; `COMETCLI_OFFLINE=1` disables the agent entirely, leaving the pure
  CLI.

---

## 6. Safety model (validator-grade)

- **Double-sign guards**: before `host-migration`, snapshot restore, or any
  restart touching signing state — inspect `priv_validator_state.json`
  (height/round/step), verify the old signer is stopped, refuse ambiguous
  states. `unsafe-reset-all` and state deletion are blocked without
  `--force` + typed confirmation.
- **Tx gate**: simulate always; show decoded messages, fee, chain-id,
  account seq; require confirmation; `--generate-only` supported for
  offline signing elsewhere.
- **Config guards**: warn/block on JSON-RPC enabled on validator profiles,
  `api.enable=true` publicly bound, missing `double_sign_check_height`,
  wildcard CORS, 0 peers persistency.
- **Supply chain**: GoReleaser builds, cosign-signed artifacts, SBOM,
  SLSA provenance, checksums verified on `upgrade.prepare` downloads.
- **Threat model doc** (`SECURITY.md`): what we protect, what we log,
  disclosure process.

---

## 7. Configuration & profiles

`~/.cometcli/config.yaml`:

```yaml
active: dydx-like-chain-val
profiles:
  dydx-like-chain-val:
    chain_id: mychain-1
    evm_chain_id: 262144
    bech32_prefix: mychain
    role: validator            # validator | rpc | sentry
    home: ~/.mychaind
    binary: mychaind
    endpoints:
      comet: tcp://127.0.0.1:26657
      grpc:  127.0.0.1:9090
      lcd:   http://127.0.0.1:1317
      evm:   ""                # empty on validators — enforced by sec.exposure
    transport: {type: ssh, host: val.internal, user: ops}
    service: {type: systemd, unit: mychaind.service}
    signer: {backend: os-keyring, key: ops-hot}
    agent: {provider: anthropic, model: claude-sonnet-4-5}
    alerts: {slack_webhook: env:SLACK_WH}
```

Secrets (LLM API keys, webhooks, passphrases) live in the OS keychain via
`99designs/keyring`, or `env:` indirection — never plaintext in config.

---

## 8. Repository layout

```
cometcli/
├── cmd/cometcli/main.go          # entry: cobra root + agent REPL
├── internal/
│   ├── cli/                      # cobra commands, thin wrappers over registry
│   ├── toolkit/                  # Tool iface, Tier, registry, approval engine
│   ├── tools/                    # catalog impls: node/ val/ chain/ evm/ keys/
│   │   ├── node/  val/  chain/   #   tx/ upgrade/ snap/ mon/ sec/ runbook/
│   │   └── ...                   #   fleet/
│   ├── client/
│   │   ├── comet/                # cometbft RPC + WS
│   │   ├── grpc/                 # cosmos gRPC (staking, slashing, gov, tx svc)
│   │   ├── evm/                  # go-ethereum ethclient / raw JSON-RPC
│   │   └── host/                 # local exec + SSH pool, docker, systemd
│   ├── tx/                       # build/simulate/sign/broadcast, keyring signer
│   ├── keys/                     # ops keyring (99designs/keyring)
│   ├── agent/
│   │   ├── loop.go               # LLM ↔ tool loop
│   │   ├── provider/             # anthropic, openai, openai-compatible
│   │   ├── prompt/               # system prompt + context snapshot builder
│   │   └── session/              # REPL session state, memory
│   ├── redact/                   # secret redaction middleware
│   ├── audit/                    # JSONL audit log + replay
│   ├── monitor/                  # watchers, alert rules, webhook sinks
│   ├── runbook/                  # procedure engine + built-in playbooks
│   ├── tui/                      # bubbletea: REPL, watch dashboard, confirms
│   └── config/                   # profiles, viper, keyring-backed secrets
├── test/
│   ├── e2e/                      # local evmd testnet harness (docker)
│   └── fixtures/                 # genesis, configs, recorded RPC cassettes
├── docs/                         # mkdocs-material site source
├── .goreleaser.yaml
├── go.mod
└── PLAN.md
```

## 9. Dependencies (pinned, vetted)

- CLI/TUI: `spf13/cobra`, `spf13/viper`, `charmbracelet/bubbletea`,
  `glamour`, `lipgloss`.
- Chain: `cometbft/cometbft` rpc client, `cosmos/cosmos-sdk` client types
  (needed for correct tx building/signing — heavy but correct), `cosmos/gogoproto`,
  `ethereum/go-ethereum` (rpc/ethclient only).
- Host: `golang.org/x/crypto/ssh`, `pkg/sftp`.
- LLM: official `anthropics/anthropic-sdk-go`, `openai/openai-go`, raw HTTP for
  OpenAI-compatible endpoints.
- Misc: `99designs/keyring`, `santhosh-tekuri/jsonschema`, `prometheus/common`
  (metrics parse), `rs/zerolog`.

## 10. Milestones

| MS | Deliverable | Gate |
|---|---|---|
| **M0 Scaffold** | repo, CI (lint/test/build), goreleaser, cobra skeleton, profile config, audit log | `cometcli --help`, `cometcli profile add` |
| **M1 Observe** | comet/grpc/evm/host clients; `node.status`, `val.status`, `val.signing`, `chain.*`, `evm.parity`, `sec.exposure`, `doctor` | **v0.1** — useful read-only SRE |
| **M2 Transactions** | ops keyring, tx pipeline (sim→confirm→broadcast), `val.unjail`, `val.withdraw-rewards`, `gov vote`, `val.edit` | **v0.2** |
| **M3 Monitor** | `mon.watch` TUI, alert rules, webhooks, missed-block tracker | **v0.3** |
| **M4 Agent** | provider abstraction, schemas from registry, REPL TUI, approval engine, redaction, `cometcli ask` | **v0.4** — the flagship release |
| **M5 Ops automation** | `upgrade.*` + cosmovisor, `snap.*` + statesync, runbook engine + core playbooks, SSH fleets | **v0.5** |
| **M6 Hardening** | e2e matrix across chains, tx-build fuzzing, external security review, docs site, perf pass | **v1.0** |

## 11. Testing strategy

- **Unit**: every tool with mocked clients; table-driven; golden files for tx
  JSON and generated prompts/schemas.
- **LLM tests**: provider interface mocked; recorded cassette fixtures for the
  agent loop (deterministic replays).
- **Integration**: dockerized local `evmd` testnet (`testnet init-files` /
  repo's local-node script) — real comet RPC + gRPC + JSON-RPC in CI.
- **E2E**: jail→detect→unjail flow on testnet; statesync bootstrap; upgrade
  staging; double-sign guard scenarios (negative tests that must abort).
- **Safety tests**: redaction fuzz corpus; permission audit on fixtures.

## 12. OSS hygiene

README quickstart (90-second demo), docs site (mkdocs-material), CONTRIBUTING,
SECURITY.md, CODE_OF_CONDUCT, issue/PR templates, conventional commits,
semver + CHANGELOG, GitHub Discussions, `oss` topic tags. Demo: asciinema
cast of `doctor` → `agent` recovering a jailed testnet validator.

## 13. Open questions to revisit

1. Live-testnet target(s) for e2e fixtures beyond local evmd?
2. CometBFT API drift across chains (v0.38 vs v1/v2) — version-negotiation
   layer in `client/comet`.
3. Plugin system post-v1 (external tools via gRPC/WASM) vs. compile-in only.
4. Agent memory: per-profile notes vs. cross-profile org memory for fleets.
5. Whether `mon` should emit Prometheus-format metrics for Grafana bridging.
