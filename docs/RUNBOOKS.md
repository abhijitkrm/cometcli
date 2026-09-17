# Runbooks

Runbooks are codified multi-step operational procedures over the same tool
registry that powers the CLI and the agent. Each step is a tool call, a note,
or a manual instruction with a confirmation gate.

## Running

```
cometcli runbook list
cometcli runbook show jail-recovery
cometcli runbook run jail-recovery --profile val01
```

`runbook run` itself is a `local-change` action — it asks for approval once
up front, then each on-chain or local-change step inside still prompts on its
own tier. Optional steps that fail are reported but don't abort the run.

## Built-in playbooks

| Name | Purpose |
|---|---|
| `jail-recovery` | Diagnose jail cause, wait out the jail period, unjail, verify |
| `halt-recovery` | Stalled chain: consensus state, logs, peer count, upgrade plan |
| `host-migration` | Move a validator without double-signing (records HRS first) |
| `statesync-bootstrap` | Rebuild a node via state-sync (confirms twice — destructive) |
| `coordinated-upgrade` | Governance upgrade: plan → stage binary → watch height |

## Authoring custom runbooks

Drop YAML files in `~/.cometcli/runbooks/`:

```yaml
name: peer-health
desc: quick peer + consensus check
steps:
  - name: peers
    tool: node.peers            # any registered tool name

  - name: consensus round
    tool: node.consensus
    args: {verbose: "true"}      # optional args map
    optional: true               # failure doesn't abort

  - name: look at the dashboard
    manual: "Eyeball Grafana for missed-block spikes."   # human step — pauses

  - name: note-only step
    note: "context printed before the next step"
```

Field reference:

| Field | Required | Effect |
|---|---|---|
| `name` | yes (runbook) | identity used by `runbook run <name>` |
| `desc` | no | shown in `runbook list` |
| `steps[].name` | no | label; defaults to `step N` |
| `steps[].tool` | one of tool/manual | registry tool name (same as `cometcli <domain> <cmd>`) |
| `steps[].args` | no | arguments as a map |
| `steps[].note` | no | printed before the step runs |
| `steps[].optional` | no | failure logs and continues |
| `steps[].manual` | one of tool/manual | instructions for a human; pauses for confirmation |

Rules:

- A step must have either `tool` or `manual` (or be a bare note).
- Tool names are the dotted registry names — `cometcli --help` shows them;
  `node.status` == `cometcli node status`.
- A custom runbook whose `name` collides with a builtin overrides it for
  your install only.
- Runbooks never bypass the tier system: a step that calls an on-chain tool
  still produces the tx doc + approval prompt.

## Agent access

The agent sees runbooks as tools (`runbook.list`, `runbook.show`,
`runbook.run`), so "run the jail recovery playbook" in `cometcli agent`
executes the same gated steps — with the model narrating each result.
