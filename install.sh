#!/usr/bin/env bash
# install.sh downloads the memstated release binary for this machine.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/map588/memstate/main/install.sh | bash
#
# Environment:
#   MEMSTATE_VERSION      release tag to install, for example v0.7.0 (default: latest)
#   MEMSTATE_INSTALL_DIR  directory for the binary (default: ~/.local/bin)
#
# The script installs only memstated, the Go daemon. The MCP proxy
# (memstate-mcp) is built from the repository. See README.md, "Install".
set -euo pipefail

REPO="map588/memstate"
VERSION="${MEMSTATE_VERSION:-latest}"
INSTALL_DIR="${MEMSTATE_INSTALL_DIR:-$HOME/.local/bin}"

log() { printf 'install.sh: %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required"

# Map uname output to the GOOS/GOARCH pair that `make release` builds.
# Asset names match releaseAssetName() in server/upgrade.go.
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Linux)  goos=linux ;;
  Darwin) goos=darwin ;;
  MINGW*|MSYS*|CYGWIN*)
    die "on Windows, download memstated-windows-amd64.exe from https://github.com/$REPO/releases" ;;
  *) die "unsupported OS: $os" ;;
esac
case "$arch" in
  x86_64|amd64)  goarch=amd64 ;;
  aarch64|arm64) goarch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
asset="memstated-$goos-$goarch"
case "$asset" in
  memstated-linux-amd64|memstated-darwin-arm64) ;;
  *) die "no release binary for $goos/$goarch; build from source with 'make install'" ;;
esac

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/$REPO/releases/latest/download/$asset"
else
  url="https://github.com/$REPO/releases/download/$VERSION/$asset"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

log "downloading $url"
curl -fsSL --retry 3 -o "$tmp/memstated" "$url" || die "download failed: $url"
chmod 0755 "$tmp/memstated"
"$tmp/memstated" --help >/dev/null 2>&1 || die "downloaded binary does not run"

mkdir -p "$INSTALL_DIR"
# mv replaces the file by rename, so a running daemon keeps its old inode.
mv -f "$tmp/memstated" "$INSTALL_DIR/memstated"
log "installed $INSTALL_DIR/memstated"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) log "note: $INSTALL_DIR is not on PATH. Add it: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac

log "next: 'memstated --addr 127.0.0.1:8765' starts a shared daemon."
log "the MCP proxy (memstate-mcp) is built from https://github.com/$REPO; see README.md, Install."
log "'memstated upgrade' fetches the newest release later."
