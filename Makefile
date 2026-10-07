# memstate — build & install
#
# Targets:
#   make build       — compile Go daemon + TS proxy in-place
#   make install     — put `memstated` on PATH (GOBIN) and link the MCP
#                       proxy as `memstate-mcp` via `npm link`
#   make uninstall   — reverse of install
#   make test        — go test + TS smoke test + MCP regression
#   make release     — memstated for five platforms and the proxy bundle
#                       memstate-mcp.tar.gz under dist/
#   make clean       — remove build artifacts
#
# The Makefile runs on Linux, macOS, WSL, Git Bash and native Windows (GNU
# make started from cmd.exe or PowerShell). Recipes use make built-ins and
# the commands of the host shell only, so Windows needs no bash and no
# coreutils. Every host-specific command is defined in the block below.
# Targets use these macros and never name a shell command directly.

# ---- host ------------------------------------------------------------------
#
# OS=Windows_NT marks every Windows host: binaries get .exe and Python is
# `python`. MSYSTEM marks Git Bash and MSYS2, which have a POSIX shell;
# without it the host is native Windows and the recipes run in cmd.exe.
PYTHON := python3
EXE    :=
ifeq ($(OS),Windows_NT)
EXE    := .exe
PYTHON := python
HOME   ?= $(USERPROFILE)
ifndef MSYSTEM
WINCMD := 1
endif
endif

# The smoke test in `make test` starts its own daemon. It gets a scratch DB
# in the ignored tmp/ directory, so it never opens the DB of the user and
# never writes to the log of the user's daemon (the log is next to the DB).
SMOKE_DIR := tmp/smoke

# `make release` stages the proxy bundle here. The stage is not in dist/,
# because CI publishes each file in dist/ as a release asset.
BUNDLE_DIR := tmp/bundle

# Macro arguments: $1 is the path or the source, $2 the destination.
# A literal # inside a variable value is written \# or make reads a comment.
# P renders a path for the host: cmd.exe built-ins want backslashes.
ifdef WINCMD
SHELL       := cmd.exe
.SHELLFLAGS := /C
P           = $(subst /,\,$1)
RM_RF       = if exist "$(call P,$1)" rmdir /S /Q "$(call P,$1)"
RM_F        = if exist "$(call P,$1)" del /Q "$(call P,$1)"
MKDIR_P     = if not exist "$(call P,$1)" mkdir "$(call P,$1)"
CP_R        = xcopy /E /I /Q /Y "$(call P,$1)" "$(call P,$2)"
CP_FILE     = copy /Y "$(call P,$1)" "$(call P,$2)"
ALIAS_BIN   = copy /Y "$(call P,$1)" "$(call P,$2)"
SAY         = echo $1
BLANK       := echo.
LS          := dir
SMOKE_ENV   := set "MEMSTATE_ADDR=" && set "MEMSTATE_CHILD=1" && set "MEMSTATE_NO_UPDATE_CHECK=1" && set "MEMSTATE_DB=$(call P,$(SMOKE_DIR)/memstate.db)" &&
MATCH_Q     = findstr /R "$1" >NUL
XBUILD      = cd server && set "CGO_ENABLED=0" && set "GOOS=$1" && set "GOARCH=$2" && go build -trimpath -ldflags="-s -w" -o ../$(DIST)/$3 .
VET_OTHER   = cd server && set "GOOS=linux" && go vet ./...
HELP        = findstr /R "^[a-z_-]*:.*\#\#" $(MAKEFILE_LIST)
TAR_CZ      = tar --exclude=__pycache__ -czf "$(call P,$1)" -C "$(call P,$2)" .
else
P           = $1
RM_RF       = rm -rf "$1"
RM_F        = rm -f "$1"
MKDIR_P     = mkdir -p "$1"
CP_R        = cp -R "$1" "$2"
CP_FILE     = install -m 0755 "$1" "$2"
ALIAS_BIN   = ln -sf "$1" "$2"
SAY         = echo '$1'
BLANK       := echo
LS          := ls -l
SMOKE_ENV   := env -u MEMSTATE_ADDR MEMSTATE_CHILD=1 MEMSTATE_NO_UPDATE_CHECK=1 MEMSTATE_DB=$(SMOKE_DIR)/memstate.db
MATCH_Q     = grep -q '$1'
XBUILD      = cd server && CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -ldflags="-s -w" -o ../$(DIST)/$3 .
VET_OTHER   = cd server && GOOS=windows go vet ./...
HELP        = awk 'BEGIN{FS=":.*?\#\#"} /^[a-zA-Z_-]+:.*?\#\#/ {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
# COPYFILE_DISABLE stops macOS tar from adding ._ files for extended attributes.
TAR_CZ      = COPYFILE_DISABLE=1 tar --exclude=__pycache__ -czf "$1" -C "$2" .
endif

GOBIN ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

# Single source of truth for the release version is healthVersion in
# server/main.go. Node reads it, because sed is not on every host. The
# variable expands only where it is used (release).
VERSION = $(shell node -p "require('fs').readFileSync('server/main.go','utf8').match(/healthVersion\s*=\s*.([0-9][0-9.]*)/)[1]")
DIST    := dist

SERVER_BIN  := server/memstated$(EXE)
CLAUDE_HOME := $(HOME)/.claude
SKILL_DIR   := $(CLAUDE_HOME)/skills/memstate
PRECOMPACT_DIR := $(CLAUDE_HOME)/skills/memstate-precompact
HOOK_SCRIPT := $(CLAUDE_HOME)/hooks/memstate-persist-reminder.sh
RECALL_HOOK_SCRIPT := $(CLAUDE_HOME)/hooks/memstate-recall.sh

.PHONY: build install uninstall install-skill uninstall-skill test release clean help

help:
	@$(HELP)

build: $(SERVER_BIN) client/dist/index.js  ## Compile daemon + proxy in-place

# make's own wildcard lists the sources, so no find is needed. Neither
# tree has subdirectories today; the second pattern covers one level.
$(SERVER_BIN): $(wildcard server/*.go server/*/*.go)
	cd server && go build -o memstated$(EXE) .

client/dist/index.js: $(wildcard client/src/*.ts client/src/*/*.ts) client/package.json client/tsconfig.json
	cd client && npm install && npm run build

install: build  ## Install memstated to GOBIN and link memstate-mcp
	@$(call MKDIR_P,$(GOBIN))
	$(call CP_FILE,$(SERVER_BIN),$(GOBIN)/memstated$(EXE))
	$(call ALIAS_BIN,$(GOBIN)/memstated$(EXE),$(GOBIN)/memstate$(EXE))
	cd client && npm link
	@$(BLANK)
	@$(call SAY,Installed:)
	@$(call SAY,  $(call P,$(GOBIN)/memstated$(EXE)))
	@$(call SAY,  $(call P,$(GOBIN)/memstate$(EXE))  (the same binary as the human CLI: memstate --help))
	@$(call SAY,  memstate-mcp  (npm global link to $(call P,$(CURDIR)/client)))
	@$(BLANK)
	@$(call SAY,Add to your MCP client config:)
	@$(call SAY,  { "mcpServers": { "memstate": { "command": "memstate-mcp" } } })
	@$(BLANK)
	@$(call SAY,Or for Claude Code:)
	@$(call SAY,  claude mcp add --scope user -- memstate memstate-mcp)

uninstall:  ## Remove installed binary and unlink proxy
	-$(call RM_F,$(GOBIN)/memstated$(EXE))
	-$(call RM_F,$(GOBIN)/memstate$(EXE))
	-cd client && npm unlink -g @memstate/mcp

install-skill:  ## Install the Claude Code skills + UserPromptSubmit hooks into ~/.claude
	@$(call MKDIR_P,$(CLAUDE_HOME)/skills)
	@$(call MKDIR_P,$(CLAUDE_HOME)/hooks)
	$(call RM_RF,$(SKILL_DIR))
	$(call CP_R,client/skill,$(SKILL_DIR))
	$(call RM_RF,$(PRECOMPACT_DIR))
	$(call CP_R,client/skill-precompact,$(PRECOMPACT_DIR))
	$(call CP_FILE,.claude/hooks/memstate-persist-reminder.sh,$(HOOK_SCRIPT))
	$(call CP_FILE,.claude/hooks/memstate-recall.sh,$(RECALL_HOOK_SCRIPT))
	$(PYTHON) scripts/configure-claude-hook.py install $(HOOK_SCRIPT) $(RECALL_HOOK_SCRIPT)
	@$(BLANK)
	@$(call SAY,Skills installed in $(call P,$(SKILL_DIR)))
	@$(call SAY,                    $(call P,$(PRECOMPACT_DIR)) (run /memstate-precompact before /compact))
	@$(call SAY,Hooks installed in $(call P,$(HOOK_SCRIPT)))
	@$(call SAY,                   $(call P,$(RECALL_HOOK_SCRIPT)) (finds the shared daemon through ~/.memstate/daemon.addr or MEMSTATE_ADDR))
	@$(call SAY,Settings updated: $(call P,$(CLAUDE_HOME)/settings.json) (backup at settings.json.bak))

uninstall-skill:  ## Remove both skills + hooks from ~/.claude
	-$(call RM_RF,$(SKILL_DIR))
	-$(call RM_RF,$(PRECOMPACT_DIR))
	-$(call RM_F,$(HOOK_SCRIPT))
	-$(call RM_F,$(RECALL_HOOK_SCRIPT))
	-$(PYTHON) scripts/configure-claude-hook.py uninstall
	@$(call SAY,Skill + hooks removed from $(call P,$(CLAUDE_HOME)))

test: build  ## Run Go tests + TS end-to-end smoke + MCP regression
	cd server && go test ./... && go vet ./...
	$(VET_OTHER)
	$(call RM_RF,$(SMOKE_DIR))
	$(SMOKE_ENV) node client/dist/index.js --test
	$(SMOKE_ENV) node client/dist/index.js --test --embed-model memstate-smoke-model | $(call MATCH_Q,embed_model.:.memstate-smoke-model)
	node client/test/regression.mjs

# Asset names must match releaseAssetName() in server/upgrade.go:
# `memstated upgrade` downloads them by exact name. install.sh and
# install.ps1 download the same names and memstate-mcp.tar.gz.
#
# memstate-mcp.tar.gz is the MCP proxy with its production node_modules
# (pure JS, so one file serves every platform), the Claude Code skill, the
# hook scripts and the script that registers the hooks. The installers
# unpack it next to server/, so the proxy finds the daemon as a sibling
# (resolveDaemonBin), the same layout as this repository.
release: client/dist/index.js  ## Build memstated for linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/amd64 and the proxy bundle under dist/
	$(call RM_RF,$(DIST))
	$(call MKDIR_P,$(DIST))
	$(call XBUILD,linux,amd64,memstated-linux-amd64)
	$(call XBUILD,linux,arm64,memstated-linux-arm64)
	$(call XBUILD,darwin,amd64,memstated-darwin-amd64)
	$(call XBUILD,darwin,arm64,memstated-darwin-arm64)
	$(call XBUILD,windows,amd64,memstated-windows-amd64.exe)
	$(call RM_RF,$(BUNDLE_DIR))
	$(call MKDIR_P,$(BUNDLE_DIR)/client)
	$(call MKDIR_P,$(BUNDLE_DIR)/hooks)
	$(call CP_FILE,client/package.json,$(BUNDLE_DIR)/client/package.json)
	$(call CP_FILE,client/package-lock.json,$(BUNDLE_DIR)/client/package-lock.json)
	$(call CP_FILE,client/LICENSE,$(BUNDLE_DIR)/client/LICENSE)
	$(call CP_R,client/dist,$(BUNDLE_DIR)/client/dist)
	$(call CP_R,client/skill,$(BUNDLE_DIR)/skill)
	$(call CP_R,client/skill-precompact,$(BUNDLE_DIR)/skill-precompact)
	$(call CP_FILE,.claude/hooks/memstate-persist-reminder.sh,$(BUNDLE_DIR)/hooks/memstate-persist-reminder.sh)
	$(call CP_FILE,.claude/hooks/memstate-recall.sh,$(BUNDLE_DIR)/hooks/memstate-recall.sh)
	$(call CP_FILE,scripts/configure-claude-hook.py,$(BUNDLE_DIR)/configure-claude-hook.py)
	cd $(call P,$(BUNDLE_DIR)/client) && npm ci --omit=dev --ignore-scripts --no-audit --no-fund
	$(call TAR_CZ,$(DIST)/memstate-mcp.tar.gz,$(BUNDLE_DIR))
	@$(BLANK)
	@$(call SAY,Release binaries for v$(VERSION) in $(DIST)/:)
	@$(LS) $(DIST)

clean:  ## Remove build artifacts
	$(call RM_F,$(SERVER_BIN))
	$(call RM_RF,client/dist)
	$(call RM_RF,$(DIST))
	$(call RM_RF,$(SMOKE_DIR))
	$(call RM_RF,$(BUNDLE_DIR))
