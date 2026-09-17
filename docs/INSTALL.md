# Installing cometcli

## One-liner (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/abhijitkrm/cometcli/main/scripts/install.sh | bash
```

Downloads the latest release for your platform (Linux/macOS, amd64/arm64),
verifies the published `checksums.txt` against the archive, and installs the
binary.

Defaults and knobs:

```bash
# pick a version (default: latest release)
COMETCLI_VERSION=v0.1.0 bash install.sh

# custom binary directory (created if missing)
COMETCLI_BIN_DIR=$HOME/bin bash install.sh

# install destination order:
#   $COMETCLI_BIN_DIR → /usr/local/bin (or /opt/homebrew/bin on Apple Silicon,
#   or via sudo) → ~/.local/bin
```

The script prints the download URL, resolved version, and destination before
writing anything. Inspect it first:

```bash
curl -fsSL https://raw.githubusercontent.com/abhijitkrm/cometcli/main/scripts/install.sh -o install.sh
less install.sh
bash install.sh
```

## From source

```bash
git clone https://github.com/abhijitkrm/cometcli && cd cometcli
go build -o cometcli ./cmd/cometcli          # Go 1.25+
# or: go install github.com/abhijitkrm/cometcli/cmd/cometcli@latest
```

## Manual binary install

Grab the tarball for your platform from
[Releases](https://github.com/abhijitkrm/cometcli/releases), verify, install:

```bash
curl -fLO https://github.com/abhijitkrm/cometcli/releases/download/v0.1.0/cometcli_0.1.0_darwin_arm64.tar.gz
curl -fLO https://github.com/abhijitkrm/cometcli/releases/download/v0.1.0/checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing   # or: sha256sum -c on Linux
tar xzf cometcli_0.1.0_darwin_arm64.tar.gz cometcli
install cometcli /usr/local/bin/
```

## Verifying release signatures

Releases are signed with cosign (keyless, GitHub OIDC). With
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
installed:

```bash
curl -fLO https://github.com/abhijitkrm/cometcli/releases/download/v0.1.0/checksums.txt.sig
cosign verify-blob --signature checksums.txt.sig \
  --certificate-identity-regexp 'github.com/abhijitkrm/cometcli' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

Every release also ships a per-architecture SBOM (`*.sbom.json`, generated
with syft).

## After install

```bash
cometcli version
cometcli profile add myval --chain-id <chain> --comet tcp://host:26657 ...
cometcli doctor
```

See the [Quick start](../README.md#quick-start) and
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).
