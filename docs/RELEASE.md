# Release checklist

Cutting a cometcli release. Current versioning: `0.x` — CLI surface may
change; promote to `v1.0.0` when ready to promise flag stability.

## Before tagging

- [ ] CI green on `main` (`build-test` + `lint` are required checks)
- [ ] `go test ./...`, `go vet ./...`, `golangci-lint run` clean locally
- [ ] `goreleaser release --snapshot --clean` builds all four archives +
      SBOMs + checksums
- [ ] Live smoke against a testnet: `doctor`, `tx send`, `mon alerts --once`
- [ ] CHANGELOG-worthy commits reviewed (goreleaser generates notes from
      commit messages — keep them conventional)

## Tag and release

```bash
git tag -a v0.X.0 -m "v0.X.0"
git push origin v0.X.0
```

The release workflow builds archives (linux/darwin × amd64/arm64), syft
SBOMs, `checksums.txt`, and cosign keyless-signs the checksum.

## Verify the release

- [ ] Release notes read well — the grouped auto-changelog covers commits;
      hand-edit the body for highlights on notable releases
- [ ] Release page shows 4 archives + `checksums.txt` + `checksums.txt.sig`
      + 4 `*.sbom.json`
- [ ] `cosign verify-blob` passes (see docs/INSTALL.md)
- [ ] `curl | bash` install of the new tag works on linux + macOS
- [ ] `cometcli version` reports the tag
- [ ] Homebrew formula updated (requires `abhijitkrm/homebrew-tap` repo +
      `HOMEBREW_TAP_TOKEN` secret — see below)

## One-time setup

**Homebrew tap**: create a public repo `abhijitkrm/homebrew-tap`, then add a
repo secret `HOMEBREW_TAP_TOKEN` (PAT with `repo` scope). The `brews:` block
in `.goreleaser.yaml` publishes `Formula/cometcli.rb` on each release —
remove that block if you skip this step, or releases will fail.

**Branch protection**: `main` requires `build-test` + `lint` green and
rejects force-pushes/deletions (configured via GitHub API).

## If a release goes bad

Do not delete the tag silently — publish a patch (`v0.X.1`) or mark the
release as a GitHub pre-release with a note. Artifacts are immutable once
downloaded.
