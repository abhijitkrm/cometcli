# Testing cometcli locally

A step-by-step guide to running cometcli against a local Docker validator
network, the deterministic CLI first and then the AI agent with a free LLM.
Every step is read-only unless it says otherwise.

---

## 0. Prerequisites

- Go 1.25+
- A running local network (this guide uses an evmd docker-compose
  setup: containers `validator0` … `validator3`)
- Optional, for the agent: a free LLM key (see [§3](#3-pick-a-free-llm))

Check the nodes are up and producing blocks:

```bash
docker ps --format 'table {{.Names}}\t{{.Status}}'
curl -s localhost:26657/status | grep -o '"latest_block_height":"[0-9]*"'
```

## 1. Build

```bash
git clone <this repo> && cd cometcli
go build -o ./bin/cometcli ./cmd/cometcli
export PATH="$PWD/bin:$PATH"
cometcli version
```

Run the test suite once to make sure your checkout is healthy:

```bash
go test ./...
```

## 2. Connect the validators (no LLM needed)

### Option A: wizard

```bash
cometcli init        # detects docker containers, port bindings, and mounted homes
```

### Option B: one command per node

Each container maps its ports to different host ports. These match the
evmd compose file; check yours with `docker port validator0`.

| profile | container | comet | gRPC | REST | EVM | home (host mount) |
|---|---|---|---|---|---|---|
| val0 | validator0 | 26657 | 9090 | 1317 | 8545 | `~/.node_1` |
| val1 | validator1 | 26667 | 9100 | 1327 | 8555 | `~/.node_2` |
| val2 | validator2 | 26677 | 9110 | 1337 | 8565 | `~/.node_3` |
| val3 | validator3 | 26687 | 9120 | 1347 | 8575 | `~/.node_4` |

```bash
for i in 0 1 2 3; do
  o=$((i*10))
  cometcli profile add val$i \
    --chain-id mychain-1 --evm-chain-id 123457 --bech32-prefix cosmos --fee-denom adex \
    --role validator --binary evmd --home "$HOME/.node_$((i+1))" \
    --comet tcp://127.0.0.1:$((26657+o)) --grpc 127.0.0.1:$((9090+o)) \
    --lcd http://127.0.0.1:$((1317+o)) --evm http://127.0.0.1:$((8545+o)) \
    --transport local --service docker --unit validator$i
done
cometcli profile use val0
```

To see signing and jail info without an ops key, add each validator's
operator address. You can list them with
`curl -s localhost:1317/cosmos/staking/v1beta1/validators`.

```bash
cometcli profile set val0 --valoper cosmosvaloper1...
```

### Smoke-test the tool layer

| Command | Expect |
|---|---|
| `cometcli node status` | moniker, height increasing, `syncing: false` |
| `cometcli val status` | `BONDED`, `jailed: false` (needs `--valoper`) |
| `cometcli val signing` | missed blocks / uptime |
| `cometcli node logs --lines 20` | container logs via `docker logs` |
| `cometcli fleet status` | one row per profile, same height |
| `cometcli doctor` | checklist (see note below) |
| `cometcli sec exposure` | port audit from `docker port` |
| `cometcli --profile val2 node peers` | any profile, without switching |

> **Expected findings on a dev network.** `doctor` and `sec exposure`
> report **6 critical exposures** per validator, because the compose file
> publishes RPC, gRPC, REST, EVM JSON-RPC and metrics on `0.0.0.0`. They may
> also flag `config/` as `0755`. These are correct findings, so leave them
> as they are for a local dev network and fix them before production.

## 3. Pick a free LLM

The agent needs a model that supports **tool calling**. Each request sends
the system prompt, a live node snapshot, and about 55 tool schemas, roughly
**6–7K tokens**. A typical question with 2–3 tool calls uses **20–25K
tokens**, which decides which free tier is usable.

| Option | Cost | Daily budget | Best for |
|---|---|---|---|
| **Groq** + `openai/gpt-oss-120b` | free key | ~200K tokens/day, 30 req/min | ✅ recommended: fast, good tool use |
| Groq + `llama-3.3-70b-versatile` (default) | free key | ~100K tokens/day | ~4–5 questions/day |
| **Ollama** (local) | free, no key | unlimited | offline, private, unlimited; needs RAM |
| OpenRouter `:free` models | free key | ~50 requests/day | trying other models |

Free-tier limits change; check the provider's console for current numbers.

### Groq (recommended)

1. Sign up at <https://console.groq.com> → **API Keys** → *Create API Key*.
2. Export it and point the profiles at Groq:

```bash
export GROQ_API_KEY=gsk_...
for i in 0 1 2 3; do
  cometcli profile set val$i --agent-provider groq --agent-model openai/gpt-oss-120b
done
```

### Ollama (fully local, no key)

```bash
brew install ollama && ollama serve &      # or the macOS app
ollama pull qwen3:8b                       # ~5 GB; tool-capable. qwen3:32b if you have 32 GB+ RAM
cometcli profile set val0 --agent-provider ollama --agent-model qwen3:8b
```

The default base URL is `http://localhost:11434`. Small models make more
tool-calling mistakes, so expect rougher answers than Groq.

### OpenRouter

```bash
export COMETCLI_LLM_API_KEY=sk-or-...      # from https://openrouter.ai/keys
cometcli profile set val0 --agent-provider openai-compat \
  --agent-base-url https://openrouter.ai/api \
  --agent-model meta-llama/llama-3.3-70b-instruct:free
```

### Save tokens while testing

- `--mode readonly` hides all mutating tools from the model: fewer
  schemas, smaller requests, and the model can't change anything.
- `--budget 4` caps tool calls per question.
- If a provider errors mid-stream, add `--no-stream` (or `no_stream: true`
  under `agent:` in the profile).

Keys are read from the environment: `GROQ_API_KEY`, `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, or the catch-all `COMETCLI_LLM_API_KEY`. Keys are never
stored in `config.yaml`.

## 4. Test the agent

### One-shot (`ask`)

```bash
cometcli ask --mode readonly "is my validator healthy? cite height and missed blocks"
cometcli ask --mode readonly --profile val2 "compare peers and signing across the fleet"
cometcli ask --mode readonly "why might the EVM height drift from the comet height?"
```

You should see text streaming in, `◐ tool` lines as the agent calls tools,
and `✓` results with real numbers from your nodes.

### Interactive terminal

```bash
cometcli                 # chat TUI on the active profile (same as `cometcli ui`)
```

Try these in the chat:

| Input | What it tests |
|---|---|
| `what's the state of my validator?` | snapshot + tool calls + streaming |
| `/tools` | each tool's tier and how this session treats it |
| `/mode readonly` then `restart the node` | the model is refused; nothing runs |
| `/mode ops` then `add persistent peer abc@1.2.3.4:26656` → press **n** | approval modal with a red/green `config.toml` diff; denying writes nothing |
| `/approve on-chain on` | refused: on-chain can never be autopiloted |
| `/runbook` then `/runbook jail-recovery` | runbook list, then the agent walks it |
| `/model llama-3.1-8b-instant` | switch model mid-conversation |
| `/audit` | audit file + session id |
| `Esc` while it's working | cancels the turn |

`Tab` switches to the Overview, Fleet, Logs, and Send panes.

### Web chat

```bash
cometcli serve --open --mode readonly
```

This opens `http://127.0.0.1:8765/?token=…` in your browser: the same agent
with a node status bar, streamed answers, tool cards, and approval dialogs.
It accepts connections from this machine only, and only with the printed
token link. To reach it from another machine:
`ssh -L 8765:127.0.0.1:8765 <host>`.

### Line REPL (for logs and screen recordings)

```bash
cometcli agent --mode readonly
```

## 5. Audit and replay

Everything the agent does is appended to `~/.cometcli/audit/<date>.jsonl`.

```bash
cometcli audit --tail 20                 # prompts, llm turns, tool calls, approvals
cometcli audit sessions                  # session ids + first prompt
cometcli audit replay <session-id>       # reproduces the session offline
```

Replay feeds the recorded LLM turns and tool outputs back through the agent
loop. It needs no key and doesn't touch the network, so you can turn off
Wi-Fi to prove it.

## 6. Safety checks worth doing once

```bash
# Secrets never reach the LLM: the audit log shows what was sent
cometcli ask --mode readonly "my seed is legal winner thank year wave sausage worth useful legal winner thank yellow, is that ok?"
grep -o 'REDACTED_[A-Z]*' ~/.cometcli/audit/$(date +%F).jsonl | sort | uniq -c

# Kill switch: agent off, CLI on
COMETCLI_OFFLINE=1 cometcli ask hi        # refuses
COMETCLI_OFFLINE=1 cometcli node status   # still works
```

To also mask your hostnames and IPs, add this under the profile's `agent:`
in `~/.cometcli/config.yaml`:

```yaml
agent:
  redact_hosts: [val.internal, 10.0.4.7]
  redact_endpoints: true    # also mask this profile's own endpoint/SSH hosts
```

## 7. On-chain actions (optional, testnet funds only)

Profiles start read-only. To let the agent build transactions, for example
`unjail`, `withdraw rewards`, or `vote`, attach an ops key from the **test**
keyring. `keys` commands use the active profile's keyring backend, so set it
first:

```bash
cometcli profile set val0 --signer ops --signer-backend test
cometcli keys add --name ops                          # new key; fund it from a dev account
# or: cometcli keys add --name ops --recover          # import a funded dev mnemonic
```

Every transaction is simulated and shown with its messages, fee, chain-id,
and sequence, and it always needs an explicit approval (`y` in the terminal,
**Sign & broadcast** on the web). `--autopilot` cannot skip it.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `agent disabled — set agent.provider` | `cometcli profile set <p> --agent-provider groq` |
| `HTTP 401` / `invalid api key` | key not exported in this shell (`echo $GROQ_API_KEY`) |
| `HTTP 429` / `rate limit` | free-tier limit hit: wait, use `--mode readonly --budget 4`, or switch model |
| `HTTP 413` / `request too large` | model's per-minute token cap is below one request; use `openai/gpt-oss-120b` or Ollama |
| model replies without calling tools | weak tool-calling model; use gpt-oss-120b, llama-3.3-70b, or qwen3 |
| garbled or missing streamed text | `--no-stream` |
| `no live snapshot — node unreachable` | check the profile's `--comet` port matches `docker port <container>` |
| `node logs` fails | profile needs `--service docker --unit <container>` |
| `cometcli` prints help instead of the chat | no active profile (`cometcli profile use …`) or not in a real terminal |
