#!/usr/bin/env bash
# cometcli installer — downloads the latest signed release from GitHub and
# installs it into /usr/local/bin (or ~/.local/bin without sudo).
#
#   curl -fsSL https://raw.githubusercontent.com/abhijitkrm/cometcli/main/scripts/install.sh | bash
#
# Options (env vars):
#   COMETCLI_VERSION   pin a tag (default: latest release)
#   COMETCLI_BIN_DIR   install dir (default: first writable of /opt/homebrew/bin,
#                      /usr/local/bin; fallback ~/.local/bin — auto-added to PATH)
#   COMETCLI_NO_VERIFY set to 1 to skip sha256 verification (not recommended)
set -euo pipefail

REPO="abhijitkrm/cometcli"
BIN="cometcli"

say()  { printf '\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

need curl; need tar

# --- platform -----------------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported arch: $arch" ;;
esac
case "$os" in
  darwin|linux) ;;
  *) die "unsupported os: $os (windows users: use WSL or build from source)" ;;
esac
info "platform: ${os}/${arch}"

# --- version -------------------------------------------------------------------
if [ -z "${COMETCLI_VERSION:-}" ]; then
  COMETCLI_VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | head -1 | cut -d'"' -f4)"
  [ -n "$COMETCLI_VERSION" ] || die "could not resolve latest release tag"
fi
info "version:  ${COMETCLI_VERSION}"

# --- download -------------------------------------------------------------------
ver="${COMETCLI_VERSION#v}"
asset="${BIN}_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${COMETCLI_VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

say "downloading ${asset}"
# --progress-bar keeps piped installs honest; timeouts+retries catch a stuck CDN
curl -fL --progress-bar --connect-timeout 15 --retry 3 --retry-delay 2 \
  "${base}/${asset}" -o "${tmp}/${asset}" \
  || die "download failed — check your connection, or does ${COMETCLI_VERSION} have a ${os}/${arch} asset?"
curl -fsSL --connect-timeout 15 --retry 3 \
  "${base}/checksums.txt" -o "${tmp}/checksums.txt" \
  || die "checksums.txt missing for ${COMETCLI_VERSION}"

# --- verify ----------------------------------------------------------------------
if [ "${COMETCLI_NO_VERIFY:-0}" != "1" ]; then
  want="$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')"
  [ -n "$want" ] || die "no checksum entry for ${asset}"
  if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "${tmp}/${asset}" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    got="$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')"
  else
    die "no sha256sum/shasum — set COMETCLI_NO_VERIFY=1 to skip (not recommended)"
  fi
  [ "$want" = "$got" ] || die "checksum mismatch! want $want got $got"
  info "sha256 verified: ${got:0:16}…"
fi

# --- install ----------------------------------------------------------------------
tar -xzf "${tmp}/${asset}" -C "$tmp" || die "extract failed"
if [ -n "${COMETCLI_BIN_DIR:-}" ]; then
  dest="$COMETCLI_BIN_DIR"
  mkdir -p "$dest"
  install -m 0755 "${tmp}/${BIN}" "${dest}/${BIN}"
else
  # prefer a dir already on PATH: homebrew on macOS, then /usr/local/bin
  dest=""
  for cand in /opt/homebrew/bin /usr/local/bin; do
    if [ -d "$cand" ] && [ -w "$cand" ]; then dest="$cand"; break; fi
  done
  if [ -n "$dest" ]; then
    install -m 0755 "${tmp}/${BIN}" "${dest}/${BIN}"
  elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    dest="/usr/local/bin"
    sudo install -m 0755 "${tmp}/${BIN}" "${dest}/${BIN}"
  else
    dest="${HOME}/.local/bin"
    mkdir -p "$dest"
    install -m 0755 "${tmp}/${BIN}" "${dest}/${BIN}"
  fi
fi

# --- PATH fixup: if dest isn't on PATH, append it to the user's shell rc ----------
path_hint=""
case ":${PATH}:" in
  *":${dest}:"*) ;;
  *)
    rc=""
    case "${SHELL:-}" in
      */zsh)  rc="${HOME}/.zshrc" ;;
      */bash) rc="${HOME}/.bashrc" ;;
      *)      rc="${HOME}/.profile" ;;
    esac
    line="export PATH=\"${dest}:\$PATH\"  # cometcli"
    if [ -n "$rc" ] && ! grep -qF "$dest" "$rc" 2>/dev/null; then
      printf '\n%s\n' "$line" >> "$rc"
      info "added ${dest} to PATH in ${rc}"
      path_hint="run:  source ${rc}    # or open a new terminal"
    else
      path_hint="add ${dest} to your PATH"
    fi ;;
esac

say ""
say "cometcli ${COMETCLI_VERSION} installed → ${dest}/${BIN}"
[ -n "$path_hint" ] && info "$path_hint"
say ""
say "get running in 60 seconds:"
cat <<EOF

  ${BIN} profile add myval \\
    --chain-id <your-chain-id> \\
    --comet tcp://<node>:26657 \\
    --grpc <node>:9090 \\
    --home ~/.evmd \\
    --service systemd --unit evmd.service

  ${BIN} doctor --profile myval      # health checklist
  ${BIN} mon watch                   # live dashboard
  ${BIN} agent                       # AI copilot (optional)

docs: https://github.com/${REPO}
EOF
