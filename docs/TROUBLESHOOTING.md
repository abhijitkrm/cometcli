# Troubleshooting

## Profiles

**`no active profile`** — set one: `cometcli profile use <name>` or
`--profile <name>` / `COMETCLI_PROFILE=<name>`.

**`profile add` wiped my endpoints** — fixed in current builds: `profile add`
now merges; only flags you actually pass are overwritten.

**Endpoints**: `comet` is the CometBFT RPC (`tcp://host:26657`), `grpc` the
Cosmos gRPC (`host:9090`), `evm` the JSON-RPC (`http://host:8545`).

## Dead endpoints / hangs

Every tool call has a built-in deadline (90s CLI, 30s per doctor check).
If `node status` still sits there, the endpoint is firewalled-open but
non-responsive — check `curl -m 5 http://host:26657/status` directly.

## Keyring backends

| Backend | Behavior |
|---|---|
| `test` | in-memory, password `test`, **lost on exit** — dev only |
| `file` | encrypted file in `~/.cometcli/keys/`, needs `COMETCLI_KEYRING_PASSWORD` for non-interactive use |
| `os` | macOS Keychain / platform store — may pop a GUI prompt |

**`file backend requires COMETCLI_KEYRING_PASSWORD`** — export it for
scripts/CI: `export COMETCLI_KEYRING_PASSWORD=...`.

**Imported keys**: `keys add --privkey-hex` (eth_secp256k1 raw hex) or
`--recover` (mnemonic). Verify with `keys show <name>` — the address must
match the account you expect.

## Transactions

**`code: 0` is not success** — cometcli now polls `tx get` for the committed
result. A printed `code: 0` means deliver_tx actually succeeded. Errors like
`tx failed in block (code N): <log>` are real chain rejections — the raw log
has the reason.

**`account sequence mismatch`** — another tx from the same account is pending.
cometcli rebuilds+retries once automatically; if it still fails, wait a block
or pass `--seq <n> --acc-num <n>` explicitly.

**`insufficient fee`** — pass `--gas-price <min>` matching the chain's
`minimum-gas-prices` (e.g. `--gas-price 25000000000uatom`).

**`tx already seen`** — you re-broadcast byte-identical txs; harmless dedup.

**Signing algo** — `eth_secp256k1` keys sign keccak256 digests; standard
`secp256k1` signs sha256. cometcli picks by key algo automatically. A
"signature verification failed" on a cosmos-evm chain usually means the key
was created with the wrong `--algo`.

## SSH transport

- Profile needs `transport ssh`, `ssh-host`, `ssh-user`, `ssh-key` (defaults
  to `~/.ssh/id_ed25519`/`id_rsa`), `ssh-port`.
- Only publickey auth; `known_hosts` pinning is not enforced yet.
- **"account is locked"** on alpine-style images: `passwd -u <user>`.
- Every remote command is audited — check `cometcli audit`.

## EVM endpoints

- `evm` tools target **RPC/sentry nodes**, not validators — a validator should
  never expose `:8545` publicly (use `sec exposure` to check).
- `evm parity` drift > ~10 blocks means the JSON-RPC node is behind or the
  endpoint serves a different chain — `evm chainid` prints the real EIP-155 id.

## Validator vs RPC node

Set `--role validator|rpc|sentry` on the profile. Doctor treats a public
EVM/API/gRPC port as a finding on validators, normal on RPC nodes.

## State sync

`snap statesync` without `--apply` only prints the proposed `[statesync]`
block — safe to run anywhere. `--apply` patches `config.toml` after approval.
The `statesync-bootstrap` runbook sequences the destructive parts (stop →
reset → configure → start) with manual gates.

## Upgrades

`upgrade prepare` stages into `<home>/cosmovisor/upgrades/<name>/bin/` —
verify with `upgrade check`. A checksum mismatch aborts before staging; the
empty dir left behind is harmless.

## Agent

- Provider order: `agent.provider` in profile; key from `agent.api_key_env`,
  then `COMETCLI_LLM_API_KEY`, then provider-specific env
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OLLAMA_API_KEY`).
- `off`/`none` disables the agent entirely.
- Local models: `provider: ollama` + `base_url: http://localhost:11434`.
- Everything the model sees is redacted (keys, mnemonics, JWTs, URL creds);
  every tool call is audited.

## Audit trail

`~/.cometcli/audit/YYYY-MM-DD.jsonl` — every tool call, approval decision,
shell command, and agent prompt. `cometcli audit` tails it.

## Getting more signal

- `cometcli <cmd> --json` — structured output for scripting.
- `cometcli doctor --profile <p>` — one-shot full checklist.
- `cometcli node logs --lines 500 --grep 'error|panic'` — first place to look
  on any failure.
