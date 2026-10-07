#!/usr/bin/env bash
# install.sh installs memstate: the memstated daemon and the memstate-mcp
# MCP server. It can also connect the server to your MCP clients and
# install the Claude Code skill and hooks.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/map588/memstate/main/install.sh | bash
#
# Requirements: curl, tar and Node 18 or later. The Claude Code skill also
# needs Python 3.
#
# Environment:
#   MEMSTATE_VERSION        release tag to install, for example v0.7.8 (default: latest)
#   MEMSTATE_INSTALL_DIR    directory for the commands (default: ~/.local/bin)
#   MEMSTATE_SETUP          1 runs `memstate-mcp setup`, 0 skips it
#                           (default: run it when a terminal is present)
#   MEMSTATE_INSTALL_SKILL  1 installs the Claude Code skill and hooks, 0 skips
#                           them (default: ask when a terminal is present)
#   MEMSTATE_DOWNLOAD_URL   directory (http:// or file://) that holds the
#                           release assets; for tests of a local `make release`
#
# The programs go to ${XDG_DATA_HOME:-~/.local/share}/memstate, in the same
# layout as the repository, so the MCP proxy finds the daemon next to it:
#   server/memstated        the daemon
#   client/                 the MCP proxy and its node_modules
#   skill/ skill-precompact/ hooks/ configure-claude-hook.py   the Claude Code skills
# MEMSTATE_INSTALL_DIR gets three links: memstated, memstate (the human CLI)
# and memstate-mcp. Run the script again to update both parts.
set -euo pipefail

REPO="map588/memstate"
BUNDLE="memstate-mcp.tar.gz"
VERSION="${MEMSTATE_VERSION:-latest}"
INSTALL_DIR="${MEMSTATE_INSTALL_DIR:-$HOME/.local/bin}"
ROOT="${XDG_DATA_HOME:-$HOME/.local/share}/memstate"
CLAUDE_HOME="$HOME/.claude"
tmp=""

log() { printf 'install.sh: %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

# has_tty reports whether the user can answer a prompt. Under `curl | bash`
# the script itself is stdin, so prompts read /dev/tty. In CI /dev/tty
# exists but does not open.
has_tty() { (exec </dev/tty) 2>/dev/null; }

# check01 stops the script when the variable named $1 is set to a value
# other than 0 or 1.
check01() {
  case "${!1:-}" in
    ""|0|1) ;;
    *) die "$1 must be 0 or 1, not '${!1}'" ;;
  esac
}

# ask QUESTION DEFAULT reads y or n from the terminal. DEFAULT is y or n.
ask() {
  local hint="[y/N]" ans=""
  [ "$2" = y ] && hint="[Y/n]"
  printf '%s %s ' "$1" "$hint" >/dev/tty
  read -r ans </dev/tty || ans=""
  case "$ans" in
    [Yy]*) return 0 ;;
    [Nn]*) return 1 ;;
    *) [ "$2" = y ] ;;
  esac
}

# fetch URL FILE downloads URL to FILE.
fetch() {
  log "downloading $1"
  curl -fsSL --retry 3 -o "$2" "$1"
}

install_skill() {
  if ! command -v python3 >/dev/null 2>&1; then
    log "warning: python3 not found. The skill scripts and the hook setup need it."
    log "skipped the Claude Code skill."
    return 0
  fi
  local skill_dir="$CLAUDE_HOME/skills/memstate" hooks_dir="$CLAUDE_HOME/hooks"
  local precompact_dir="$CLAUDE_HOME/skills/memstate-precompact"
  mkdir -p "$CLAUDE_HOME/skills" "$hooks_dir"
  rm -rf "$skill_dir" "$precompact_dir"
  cp -R "$ROOT/skill" "$skill_dir"
  cp -R "$ROOT/skill-precompact" "$precompact_dir"
  install -m 0755 "$ROOT/hooks/memstate-persist-reminder.sh" "$hooks_dir/memstate-persist-reminder.sh"
  install -m 0755 "$ROOT/hooks/memstate-recall.sh" "$hooks_dir/memstate-recall.sh"
  python3 "$ROOT/configure-claude-hook.py" install \
    "$hooks_dir/memstate-persist-reminder.sh" "$hooks_dir/memstate-recall.sh" </dev/null
  log "installed the skills in $skill_dir and $precompact_dir"
  log "installed the hooks in $hooks_dir and added them to $CLAUDE_HOME/settings.json"
  log "the recall hook needs a shared daemon: set MEMSTATE_ADDR, or start 'memstated --addr 127.0.0.1:8765'"
}

# The script body is one function that runs on the last line. Bash then
# reads the whole script before it runs a command, so a download that
# stops part way runs nothing.
main() {
  check01 MEMSTATE_SETUP
  check01 MEMSTATE_INSTALL_SKILL
  command -v curl >/dev/null 2>&1 || die "curl is required"
  command -v tar >/dev/null 2>&1 || die "tar is required"

  # Map uname output to the GOOS/GOARCH pair that `make release` builds.
  # Asset names match releaseAssetName() in server/upgrade.go.
  local os arch goos goarch asset
  os="$(uname -s)"
  arch="$(uname -m)"
  case "$os" in
    Linux)  goos=linux ;;
    Darwin) goos=darwin ;;
    MINGW*|MSYS*|CYGWIN*)
      die "on Windows, run this in PowerShell: irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
    *) die "unsupported OS: $os" ;;
  esac
  case "$arch" in
    x86_64|amd64)  goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
  esac
  asset="memstated-$goos-$goarch"

  # The MCP proxy is a Node program. Check for Node before any download.
  command -v node >/dev/null 2>&1 ||
    die "Node 18 or later is required for the MCP server. Install it from https://nodejs.org, then run this script again."
  node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 18 ? 0 : 1)' </dev/null ||
    die "Node 18 or later is required for the MCP server; found $(node --version </dev/null)"

  # Find the tag once, so both assets come from the same release.
  local base tag url
  if [ -n "${MEMSTATE_DOWNLOAD_URL:-}" ]; then
    base="${MEMSTATE_DOWNLOAD_URL%/}"
    tag="from $base"
  else
    case "$VERSION" in
      latest|v*) ;;
      *) VERSION="v$VERSION" ;;
    esac
    tag="$VERSION"
    if [ "$tag" = latest ]; then
      # GitHub redirects /releases/latest to /releases/tag/<tag>.
      url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")" ||
        die "cannot reach https://github.com/$REPO/releases/latest"
      tag="${url##*/}"
      case "$tag" in
        v[0-9]*) ;;
        *) die "cannot find the latest release (GitHub sent $url)" ;;
      esac
    fi
    base="https://github.com/$REPO/releases/download/$tag"
  fi
  log "installing memstate $tag"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  fetch "$base/$asset" "$tmp/memstated" || die "download failed: $base/$asset"
  fetch "$base/$BUNDLE" "$tmp/$BUNDLE" ||
    die "download failed: $base/$BUNDLE (releases before v0.7.8 do not have it)"
  chmod 0755 "$tmp/memstated"
  "$tmp/memstated" --help </dev/null >/dev/null 2>&1 || die "the downloaded memstated does not run"
  mkdir "$tmp/bundle"
  tar -xzf "$tmp/$BUNDLE" -C "$tmp/bundle" || die "cannot unpack $BUNDLE"
  [ -f "$tmp/bundle/client/dist/index.js" ] || die "$BUNDLE has no client/dist/index.js"

  mkdir -p "$ROOT/server" "$INSTALL_DIR"
  # mv replaces the file by rename, so a running daemon keeps its old inode.
  mv -f "$tmp/memstated" "$ROOT/server/memstated"
  local entry name
  for entry in "$tmp/bundle"/*; do
    name="${entry##*/}"
    rm -rf "${ROOT:?}/$name"
    mv "$entry" "$ROOT/$name"
  done
  # memstate-mcp links to index.js, so index.js must be executable.
  chmod 0755 "$ROOT/client/dist/index.js"
  # memstated dispatches on the name it runs as: as memstate it is the CLI.
  ln -sfn "$ROOT/server/memstated" "$INSTALL_DIR/memstated"
  ln -sfn "$ROOT/server/memstated" "$INSTALL_DIR/memstate"
  ln -sfn "$ROOT/client/dist/index.js" "$INSTALL_DIR/memstate-mcp"
  log "installed $ROOT"
  log "linked memstated, memstate and memstate-mcp in $INSTALL_DIR"

  # The proxy starts its own daemon on a scratch DB. The check never uses
  # the DB, the log or the shared daemon of the user.
  local out
  mkdir "$tmp/smoke"
  if ! out="$(env -u MEMSTATE_ADDR -u MEMSTATE_BIN \
      MEMSTATE_DB="$tmp/smoke/memstate.db" MEMSTATE_NO_UPDATE_CHECK=1 \
      node "$ROOT/client/dist/index.js" --test </dev/null 2>&1)"; then
    printf '%s\n' "$out" >&2
    die "memstate-mcp did not start the daemon"
  fi
  log "check passed: memstate-mcp started memstated $(printf '%s' "$out" | sed -n 's/.*"version":"\([^"]*\)".*/v\1/p')"

  local cmd found
  case ":$PATH:" in
    *":$INSTALL_DIR:"*)
      # An older copy earlier on PATH, for example from `make install`.
      for cmd in memstated memstate memstate-mcp; do
        found="$(command -v "$cmd" 2>/dev/null || true)"
        if [ -n "$found" ] && [ "$found" != "$INSTALL_DIR/$cmd" ]; then
          log "warning: '$cmd' runs $found, not $INSTALL_DIR/$cmd. Remove the other copy or put $INSTALL_DIR first on PATH."
        fi
      done
      ;;
    *) log "note: $INSTALL_DIR is not on PATH. Add it first: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac

  # setup finds the MCP clients, asks for the embed model, and asks before
  # it writes a config.
  local run_setup="${MEMSTATE_SETUP:-}"
  if [ -z "$run_setup" ]; then
    run_setup=0
    has_tty && run_setup=1
  fi
  if [ "$run_setup" = 1 ]; then
    local setup_in=/dev/null
    has_tty && setup_in=/dev/tty
    node "$ROOT/client/dist/index.js" setup <"$setup_in" ||
      log "warning: memstate-mcp setup failed. Run it again later: memstate-mcp setup"
  else
    log "to connect memstate to your MCP clients, run: memstate-mcp setup"
  fi

  local skill_default=n
  [ -d "$CLAUDE_HOME" ] && skill_default=y
  case "${MEMSTATE_INSTALL_SKILL:-}" in
    1) install_skill ;;
    0) ;;
    *)
      if has_tty && ask "Install the Claude Code skill and hooks?" "$skill_default"; then
        install_skill
      else
        log "skipped the Claude Code skill. To install it later, run this script with MEMSTATE_INSTALL_SKILL=1."
      fi
      ;;
  esac

  log "done. Later, run this script again to update memstated and memstate-mcp together."
}

main "$@"
