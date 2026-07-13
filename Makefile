# memstate — build & install
#
# Targets:
#   make build       — compile Go daemon + TS proxy in-place
#   make install     — put `memstated` on PATH (GOBIN) and symlink the MCP
#                       proxy as `memstate-mcp` via `npm link`
#   make uninstall   — reverse of install
#   make test        — go test + TS smoke test
#   make release     — static linux/amd64 memstated + tarball under dist/
#   make clean       — remove build artifacts

GOBIN ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

# Single source of truth for the release version is healthVersion in server/main.go.
VERSION := $(shell sed -n 's/.*healthVersion[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' server/main.go)
DIST    := dist

SERVER_BIN  := server/memstated
CLAUDE_HOME := $(HOME)/.claude
SKILL_DIR   := $(CLAUDE_HOME)/skills/memstate
HOOK_SCRIPT := $(CLAUDE_HOME)/hooks/memstate-persist-reminder.sh

.PHONY: build install uninstall install-skill uninstall-skill test release clean help

help:
	@awk 'BEGIN{FS=":.*?##"} /^[a-zA-Z_-]+:.*?##/ {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: $(SERVER_BIN) client/dist/index.js  ## Compile daemon + proxy in-place

$(SERVER_BIN): $(shell find server -name '*.go')
	cd server && go build -o memstated .

client/dist/index.js: client/src/index.ts client/package.json
	cd client && npm install && npm run build

install: build  ## Install memstated to GOBIN and link memstate-mcp
	@mkdir -p $(GOBIN)
	install -m 0755 $(SERVER_BIN) $(GOBIN)/memstated
	cd client && npm link
	@echo
	@echo "Installed:"
	@echo "  $(GOBIN)/memstated"
	@echo "  memstate-mcp (npm global link → $(PWD)/client)"
	@echo
	@echo "Add to your MCP client config:"
	@echo '  { "mcpServers": { "memstate": { "command": "memstate-mcp" } } }'
	@echo
	@echo "Or for Claude Code:"
	@echo "  claude mcp add --scope user -- memstate memstate-mcp"

uninstall:  ## Remove installed binary and unlink proxy
	-rm -f $(GOBIN)/memstated
	-cd client && npm unlink -g @memstate/mcp

install-skill:  ## Install Claude Code skill + UserPromptSubmit hook into ~/.claude
	@mkdir -p $(CLAUDE_HOME)/skills $(CLAUDE_HOME)/hooks
	rm -rf $(SKILL_DIR)
	cp -R client/skill $(SKILL_DIR)
	install -m 0755 .claude/hooks/memstate-persist-reminder.sh $(HOOK_SCRIPT)
	python3 scripts/configure-claude-hook.py install $(HOOK_SCRIPT)
	@echo
	@echo "Skill installed → $(SKILL_DIR)"
	@echo "Hook installed  → $(HOOK_SCRIPT)"
	@echo "Settings updated: $(CLAUDE_HOME)/settings.json (backup at settings.json.bak)"

uninstall-skill:  ## Remove skill + hook from ~/.claude
	-rm -rf $(SKILL_DIR)
	-rm -f $(HOOK_SCRIPT)
	-python3 scripts/configure-claude-hook.py uninstall
	@echo "Skill + hook removed from $(CLAUDE_HOME)"

test: build  ## Run Go tests + TS end-to-end smoke
	cd server && go test ./... && go vet ./...
	node client/dist/index.js --test

# Asset names must match releaseAssetName() in server/upgrade.go —
# `memstated upgrade` downloads them by exact name.
release:  ## Build static memstated for linux/amd64, darwin/arm64, windows/amd64 under dist/
	rm -rf $(DIST)
	mkdir -p $(DIST)
	cd server && CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ../$(DIST)/memstated-linux-amd64 .
	cd server && CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o ../$(DIST)/memstated-darwin-arm64 .
	cd server && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ../$(DIST)/memstated-windows-amd64.exe .
	@echo
	@echo "Release binaries for v$(VERSION) in $(DIST)/:"
	@ls -l $(DIST)

clean:  ## Remove build artifacts
	rm -f $(SERVER_BIN)
	rm -rf client/dist $(DIST)
