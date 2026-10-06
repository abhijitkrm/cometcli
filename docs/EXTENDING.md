# Extending cometcli

Settings, memory, custom commands, hooks, MCP servers and subagents all live in plain
files. The formats follow Claude Code's, so configs written for it mostly carry over.

A **project** is the nearest directory above where you start cometcli that holds a
`.cometcli/` folder, a `COMET.md` or a `.git`.

## Settings — `settings.json`

| File | Scope |
|---|---|
| `~/.cometcli/settings.json` | you, everywhere |
| `<project>/.cometcli/settings.json` | the project — commit it |
| `<project>/.cometcli/settings.local.json` | you, in this project — gitignore it |

Later files override single values (`model`, `effort`, `permissions.defaultMode`);
lists (`permissions.allow/ask/deny`), `env`, `hooks` and `mcpServers` accumulate.
`config.yaml`'s `agent:` section still picks the provider; settings refine it.

```json
{
  "model": "openai/gpt-oss-120b",
  "effort": "medium",
  "permissions": {
    "allow": ["bash(docker logs:*)", "bash(./info.sh)", "web_fetch(domain:github.com)"],
    "ask":   ["bash(docker compose:*)"],
    "deny":  ["bash(rm:*)", "edit(/etc/**)", "val.unjail"],
    "defaultMode": "acceptEdits"
  },
  "env": { "GROQ_API_KEY": "…" },
  "hooks": { … },
  "mcpServers": { … }
}
```

`env` is exported before the provider starts, without overriding variables already set.
`defaultMode` takes `default`/`ops`, `acceptEdits`, `plan`/`readonly`, `bypassPermissions`.

## Trust

Project hooks and project MCP servers run commands on your machine — on a validator
host, that matters. They are **ignored until you trust the project**:

```bash
cometcli trust              # trust the project you're in (covers subfolders)
cometcli trust --off        # revoke
cometcli trust --list
```

Untrusted items are listed when a session starts. Your user-level settings are always
trusted. A project command's `allowed-tools` also needs trust.

## Memory — `COMET.md`

Standing instructions loaded into every session's system prompt:

1. `~/.cometcli/COMET.md` — yours, everywhere
2. `COMET.md` in each directory from the project root down to where you started
   (`AGENTS.md` is read where a directory has no `COMET.md`), plus `COMET.local.md`
3. In node mode, `~/.cometcli/memory/<profile>.md` — notes about that node

A line `@path/to/file.md` imports another file (key material is never imported).

```markdown
# primium node-setup
- Validators run in docker; containers are primium-validator0..3.
- Never restart validator0 without asking — it's the genesis proposer.
- Upgrades follow upgrade/README.md.
@docs/ports.md
```

In a session: `/memory` lists what's loaded; `/remember <text>` appends to the
project's `COMET.md` (`--user`, `--node` for the others) and tells the model right away.

## Custom slash commands

Markdown files in `<project>/.cometcli/commands/` or `~/.cometcli/commands/` become
commands. `ops/vote.md` is `/ops:vote`. A project command shadows a user one.

```markdown
---
description: Vote on an upgrade proposal
argument-hint: <proposal-id> <yes|no>
allowed-tools: bash(./upgrade/vote-upgrade.sh:*), read
---
Vote $2 on proposal $1.

Current proposals: !`docker exec primium-validator0 evmd q gov proposals -o json | jq -r '.proposals[] | "\(.id) \(.status)"'`
Network config: @run-genesis/network-config.env

Read upgrade/vote-upgrade.sh first, confirm the proposal is in its voting period,
then run it. Report the tx hash.
```

- `$ARGUMENTS` is everything after the command; `$1`…`$9` are the words (quotes group).
- `` !`cmd` `` runs while expanding and inlines the output — **read-only commands only**
  (the same classifier as the bash tool); anything else is left for the agent to run
  with approval.
- `@path` inlines a file (never key material).
- `allowed-tools` adds allow rules for that one turn. Transactions still always ask.

Commands work headlessly too: `cometcli -p "/ops:vote 7 yes"`.

## Hooks

Commands run at points in the loop. Events: `PreToolUse`, `PostToolUse`,
`UserPromptSubmit`, `Stop`, `SessionStart`.

```json
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "bash", "hooks": [{ "type": "command", "command": ".cometcli/hooks/guard.sh", "timeout": 10 }] }
    ],
    "PostToolUse": [
      { "matcher": "edit|write", "hooks": [{ "type": "command", "command": "evmd validate-genesis 2>&1 | tail -3" }] }
    ],
    "UserPromptSubmit": [
      { "hooks": [{ "type": "command", "command": "echo \"on-call: $(cat ~/.oncall)\"" }] }
    ]
  }
}
```

`matcher` is a case-insensitive regex over tool names (`bash`, `edit`, `node.status`,
`mcp__grafana__.*`; Claude Code's `Bash` works); empty or `*` matches all. Hooks run
from the project root with `COMETCLI_PROJECT_DIR` (and `CLAUDE_PROJECT_DIR`) set, and
get the event as JSON on stdin:

```json
{"session_id":"…","transcript_path":"…","cwd":"…","hook_event_name":"PreToolUse",
 "permission_mode":"ops","tool_name":"bash","tool_input":{"command":"…"}}
```

PostToolUse adds `tool_response`, UserPromptSubmit `prompt`, Stop `stop_hook_active`,
SessionStart `source` (`startup`|`resume`).

| Exit | Effect |
|---|---|
| 0 | pass. UserPromptSubmit/SessionStart stdout is added as context for the model |
| 2 | **block** — PreToolUse: the call doesn't run and stderr goes to the model; PostToolUse: stderr goes to the model; UserPromptSubmit: the prompt is rejected; Stop: the agent keeps working on stderr (at most 3 times) |
| other | non-blocking error, shown to you |

Exit 0 with JSON on stdout can decide too: `{"hookSpecificOutput":{"permissionDecision":
"allow|deny|ask","permissionDecisionReason":"…","additionalContext":"…"}}`,
`{"decision":"block","reason":"…"}`, `{"systemMessage":"…"}`, `{"continue":false}`.
A hook `allow` skips the prompt for a host change — **never for a transaction**, and
it can't unlock key material or state resets.

## MCP servers

```bash
cometcli mcp add grafana -- npx -y @grafana/mcp-grafana
cometcli mcp add k8s -e KUBECONFIG=~/.kube/config -- kubectl-mcp-server
cometcli mcp add pd --transport http https://mcp.example.com/mcp -H "Authorization: Bearer ${PD_TOKEN}"
cometcli mcp add tools --scope project -- ./scripts/mcp.sh     # writes .mcp.json, shared
cometcli mcp list --check                                     # connect and count tools
cometcli mcp remove grafana
```

Scopes: `user` (default, `~/.cometcli/settings.json`), `project` (`.mcp.json` — the
same file Claude Code reads), `local` (`.cometcli/settings.local.json`). `${VAR}` and
`${VAR:-default}` expand in commands, args, env, URLs and headers.

Servers start with each session; their tools are named `mcp__<server>__<tool>` and,
like node tools, load on demand — the system prompt lists them, the model calls
`tool_search` to load the ones it needs. Tools marked `readOnlyHint` run without a
prompt; others ask. Rules: `mcp__grafana` covers a whole server. `/mcp` shows what's
connected. (`cometcli mcp` with no subcommand still *serves* cometcli's own tools.)

## Subagents

The agent can hand a self-contained investigation to a subagent with the `task` tool:
it gets a fresh context, the same tools and permissions, works through the task and
returns only its report — so long log trawls don't fill the main conversation (or a
small per-minute token budget). Subagents can't spawn subagents.

Define specialized ones in `<project>/.cometcli/agents/<name>.md` or
`~/.cometcli/agents/`:

```markdown
---
name: log-digger
description: Trawls container and journal logs for errors around a time window
tools: bash, grep, read
---
You read logs. Start with docker logs --since for each validator container,
then journalctl. Report each distinct error with first/last timestamp and count.
```

`tools` restricts what it can use. `/agents` lists them.
