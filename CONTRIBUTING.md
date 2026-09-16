# Contributing

cometcli is an agentic SRE terminal for Cosmos-EVM validators. Contributions
welcome — especially new tools in the catalog, runbooks, and chain-specific
fixtures.

## Architecture in one paragraph

Every capability is a `toolkit.Tool`: a name (`domain.verb`), a JSON schema
for args, a risk `Tier`, and a `Run(*Context, Args)` method. The cobra CLI
and the agent loop are both generated from the registry — add a tool and it
appears in both places automatically.

## Adding a tool

1. Create/extend a package under `internal/tools/<domain>/`.
2. Implement the `toolkit.Tool` interface. Declare the correct `Tier`
   honestly — it drives the approval engine.
3. Register it in the package's `Register(*toolkit.Registry)` and call that
   from `internal/tools/registry.go`.
4. Never read `priv_validator_key.json`, never log secrets, and route all
   host commands through `ctx.Host()` + `ctx.LogShell()`.
5. On-chain tools go through `common.BroadcastMsgs` (sim → approve →
   broadcast). Don't broadcast directly.

## Testing

```bash
go test ./...
go vet ./...
```

Unit-test tools against mocked clients. Keep tests hermetic (no network).
For e2e against a local evmd testnet, see `test/e2e`.

## Commit style

Conventional commits. Keep PRs focused.
