# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & test

Two-process project: Go daemon under `server/`, TypeScript MCP proxy under `client/`. Requires Go 1.27+ and Node 18+.

The Makefile at the repo root is the canonical entrypoint:

```bash
make build            # compile both in-place (server/memstated, client/dist/)
make install          # put `memstated` on GOBIN and `memstate-mcp` on PATH via npm link
make uninstall        # reverse of install
make install-skill    # copy skill to ~/.claude/skills/memstate + add UserPromptSubmit hook
make uninstall-skill  # reverse of install-skill
make test             # go test + go vet + TS smoke + MCP regression suite
make clean            # remove build artifacts
```

Raw commands for partial work:

```bash
cd server && go build -o memstated .          # Go daemon only
cd client && npm install && npm run build     # TS proxy only
cd server && go test -run TestStoreRoundTrip  # single Go test
node client/dist/index.js --test              # end-to-end: spawn daemon, hit /health, list tools
node client/test/regression.mjs               # full-stack regression: every MCP tool over stdio against a temp DB
```

Running the daemon directly:

```bash
./server/memstated                                       # child mode, random port, banner on stderr
./server/memstated --addr 127.0.0.1:8765                 # shared-daemon mode
./server/memstated --addr 127.0.0.1:8765 --idle-timeout 30m   # long-lived, self-exits when idle
./server/memstated status --addr 127.0.0.1:8765
./server/memstated stop   --addr 127.0.0.1:8765
./server/memstated projects                              # list live projects with memory counts
./server/memstated dump memstate_mcp                     # pretty-print a project's memories (ANSI markdown)
./server/memstated dump --keys memstate_mcp decisions    # keypath tree only, optionally scoped to a subtree
./server/memstated search --project memstate_mcp ollama  # FTS5 search from the CLI (omit --project for all)
./server/memstated export --all                          # full-history JSON to ~/.memstate/exports/
./server/memstated import backup.json                    # timestamp-merge into the local DB
./server/memstated upgrade                               # swap in the latest GitHub release binary; restarts a running shared daemon
./server/memstated --addr 127.0.0.1:8765 --embed-model qwen3-embedding:4b   # pick the Ollama embed model (env MEMSTATE_EMBED_MODEL)
./server/memstated embed status [--watch] [--probe]      # dashboard: coverage bar per model, sizes, cosine histogram, threshold fit
./server/memstated embed rebuild --model qwen3-embedding:4b   # drop + recompute one model's vectors (direct SQLite, prints progress)
./server/memstated embed prune --keep qwen3-embedding:4b      # delete vectors of every other model
echo '{"session_id":"s1","cwd":"'$PWD'","prompt":"how does the upgrade keep the embed model"}' | ./server/memstated recall   # what the UserPromptSubmit hook prints
```

## Architecture

```
MCP client ──stdio──> client/dist/index.js ──HTTP loopback──> server/memstated ──> SQLite
              (MCP)        (thin TS proxy)                     (Go daemon, REST)
```

**All storage logic (keypath versioning, FTS, conflict detection, tombstones, embeddings) lives in the Go daemon.** The TS proxy (`client/src/index.ts`) exists only to speak MCP — each tool call becomes one HTTP POST. Do not add business logic to the proxy.

Wire protocol between proxy and daemon is REST, not MCP. Daemon routes are declared in `server/http.go` (`newRouter`). MCP tool shape is declared in `client/src/index.ts` (`TOOLS`). Any new tool requires edits in both, plus the Python CLI mirror under `client/skill/scripts/`.

### Lifecycle model (non-obvious)

Two modes, selected at proxy startup:

- **Child mode (default):** proxy spawns `memstated` on `127.0.0.1:0` (OS-picked port), passes its own PID via `--owner-pid`, and reads the `MEMSTATE_READY addr=<addr>` banner from the daemon's stderr to learn the address. On proxy exit the daemon is SIGTERMed; if the proxy is SIGKILLed, the daemon's 2-second `kill(owner_pid, 0)` loop notices and self-exits.
- **Attach mode (`MEMSTATE_ADDR` set):** proxy probes `/health`. If a memstate daemon answers → attach. If nothing is listening → spawn a **detached** daemon on that addr (no `--owner-pid`, own session) and attach; the daemon outlives the proxy. If a non-memstate process is on the port → hard error. Set `MEMSTATE_IDLE_TIMEOUT` (e.g. `30m`) in the proxy's env to have the lazy-spawned daemon self-exit after idleness.
- **Embed options flow through the proxy.** `parseEmbedOptions` in `client/src/index.ts` reads `--embed-model` / `--ollama-url` / `--embed-timeout` from the proxy argv (flag beats env) and `embedDaemonArgs` renders them as daemon flags on every spawn, child or detached. `memstate-mcp setup [--embed-model NAME]` writes the model into the agent config `args` (or the `claude mcp add` command), listing the models Ollama serves when interactive. On attach the proxy compares its model with the daemon's `embed_model` from `/health` and warns on mismatch.
- **Detached MEMSTATE_DB warning:** the proxy only warns that `MEMSTATE_DB` is ignored when it *attached* to an already-running daemon. When it lazy-spawns, the child inherits env, so the warning would be wrong — tracked via the `alreadyRunning` flag in `attach()`.

A manually-started `--addr` daemon that finds the port busy probes `/health` itself: if occupant is ours → exit 0 quietly; otherwise exit 2 loudly. The proxy translates exit 2 from a lazy-spawn into a readable "port occupied by non-memstate" error.

### Idle-exit (`server/main.go`)

`--idle-timeout` (env `MEMSTATE_IDLE_TIMEOUT`) wraps the router in `activityMiddleware` that stamps `lastActivity` on every request; a `watchIdle` goroutine polls and triggers `shutdownFn` once no request has arrived for the timeout. Disabled when `--owner-pid` is set (parent already owns our lifetime). Poll interval is `timeout/4` clamped to `[5s, 60s]`.

### Versioned keypath store (`server/store.go`)

- Data model: per-project dot-notation keypath tree. Each write appends a new row to `memories` (never updates). Prior version is returned as `superseded` so the caller sees the conflict.
- Identical content AND metadata (category/topics) to current version is a no-op → `action: "unchanged"`, no new row. Same content with different metadata DOES version.
- `Delete` appends a tombstone row; history is preserved. `ProjectDeleted` gates reads and deletes only — **any write revives a soft-deleted project** (`ensureProject`'s `ON CONFLICT ... SET deleted_at = NULL`).
- FTS5 virtual table `memories_fts` backs the `fts` mode and the FTS side of `hybrid`; only the current version is indexed (the superseded version's FTS row is deleted on write). Free-text queries are token-quoted (`ftsQuote`, implicit AND; `ftsQuoteOr` for hybrid, any token) so punctuation can't hit FTS5 operator syntax.
- `hybrid` is the default search mode (`server/hybrid.go`): `SearchAny` (FTS, OR) and `SemanticSearch` (cosine ≥ threshold) each return a candidate pool of `max(3*limit, 30)`, `rrfFuse` merges them by reciprocal rank fusion (k=60) keyed on project+keypath, and each hit carries `score` + `sources`. When the embedder is nil or the query embed fails, the response is FTS-only with a `degraded` reason; it never returns 5xx for that. Explicit `semantic` mode still returns 503/502.
- Semantic search uses `keypath_embeddings` (one row per `(project, keypath, model)`), where the vector is computed from the **current content** at that keypath — recomputed on content change, deleted on tombstone, healed on an unchanged write if the row is missing (e.g. Ollama was down). The `meta` table's `embed_source` row wipes all vectors on startup when the embedding scheme changes.
- `category` (string) and `topics` (JSON array in TEXT) are per-version columns; `/memories/search` filters on them (topics = match-any, via `json_each`).
- Storage runs on a single pooled connection (SQLite WAL + tuned pragmas); `Write` wraps read-latest + insert in a transaction so concurrent same-keypath writers can't collide on the version unique index.

### Heading extraction (`server/extract.go`)

`memstate_remember` without an explicit keypath runs `ExtractHeadings`, which maps each `##`+ heading to a top-level keypath segment (deeper headings nest via dots; optional `root` request field nests everything under a prefix) and collapses common section names (`## TODOs` → `todo`, `## Files to touch` → `files`, etc.) via `reservedAliases`. Fenced code blocks are ignored when scanning for headings. Pre-heading prose lands under `preamble` (or `<root>.preamble`). The TS proxy derives a default `project_id` from the git repo name (cwd basename outside a repo), slugged snake_case, applied whenever a tool call omits project_id; the Python skill scripts share the same rule via `_client.default_project()`.

### Embeddings (`server/embed.go`)

Ollama-backed content embeddings are fire-and-forget from the write path via `maybeEmbedContent` → `embedder.inFlight`. A URL that ends in `/v1` selects an OpenAI-compatible server instead (llama.cpp server, LM Studio, vLLM): `Embed` posts `{"model","input"}` to `{url}/embeddings` rather than `{"model","prompt"}` to `/api/embeddings`, and `contextOverflow` treats llama.cpp's "too large to process" like Ollama's "context length", so `EmbedDocument` halves for both. Writes succeed even if Ollama is down; errors are throttled to one log per model per hour. Semantic search returns 503 when the embedder is disabled. Model-family prompt formats live in `queryText` / `documentText`: nomic-embed models get `search_document:` / `search_query:` prefixes, qwen3-embedding models get an `Instruct:`/`Query:` line on the query side only, other models get raw text. `NewEmbedder(url, model, timeout)` resolves explicit args, then `MEMSTATE_OLLAMA_URL` / `MEMSTATE_EMBED_MODEL` / `MEMSTATE_EMBED_TIMEOUT`, then defaults (the 60s timeout exists because a 4B model cold-loads in ~20s, which the old 10s bound cut off); the daemon exposes both as `--ollama-url` / `--embed-model` and reports the model in `/health` (`embed_model`), which the proxy checks against its own env on attach; `/health` also carries `semantic_threshold`, and `memstated embed status` reads both from the running daemon (`runningEmbedConfig`) unless overridden by flag or env. On startup `BackfillEmbeddings` eagerly embeds every current keypath missing a vector for the configured model (sequential, aborts on first transport error — the next startup or per-write heal retries). `Backfill` is the synchronous core; `Rebuild` = delete the model's rows + `Backfill`. Tests use `Embedder.WaitForPending()` for determinism — production never waits.

## Conventions (non-obvious)

- **No co-authoring on commits.** Do not add `Co-Authored-By:` trailers.
- New tool → edit three places in lockstep: `server/http.go` (route + handler), `client/src/index.ts` (`TOOLS` entry), `client/skill/scripts/` (Python CLI). The skill scripts are a supported interface, not a legacy artifact.
- `SKILL.md` in `client/skill/` now describes THIS daemon accurately (integer IDs, synchronous writes, headings-only extraction, category/topics filterable) and is the canonical statement of naming conventions (snake_case ids/segments, YYYY_MM_DD dates, `branches.<slug>.*` for branch-scoped state). Keep it, the MCP `INSTRUCTIONS`/tool descriptions in `client/src/index.ts`, and the Python `--help` strings in agreement when conventions change.
- `.claude/hooks/memstate-persist-reminder.sh` runs on `UserPromptSubmit` and nudges toward `memstate_remember` after ≥3 file edits since the last persist. If you want to silence it for a session, touch a trivial `memstate_set` or `memstate_remember` call.
- `.claude/hooks/memstate-recall.sh` runs on `UserPromptSubmit` and execs `memstated recall` (`server/recall.go`): hybrid-search the project derived from the hook's `cwd` (same slug rule as the proxy and the Python skill, pinned by `TestDeriveProject`) with the prompt text, print up to 3 unseen hits at 500 chars each inside `<memstate-recall>`, and record them in `~/.memstate/recall/<session_id>` so a keypath is injected once per session (files older than 7 days are pruned). It needs a **shared** daemon: `MEMSTATE_ADDR`, else `daemon.addr`; child-mode daemons are invisible to it. Prompts under 4 words are skipped, `MEMSTATE_NO_RECALL=1` disables it, `MEMSTATE_RECALL_DEBUG=1` explains a silent exit on stderr. It always exits 0. `make install-skill` installs both hook scripts; `scripts/configure-claude-hook.py` strips and re-adds every entry that names one of its `MARKERS`.
- Formerly accepted-but-ignored fields: `category`/`topics` are now stored and filterable; `at_revision` and `context` were dropped and are rejected by `DisallowUnknownFields`. Do not add silently-ignored request fields — accept a field only when it does something.
- **Dump/search subcommands are CLI-only** (`memstated dump|search`, `server/dump.go`): human DB-inspection over direct SQLite, with ANSI markdown highlighting (auto-disabled when stdout isn't a TTY or `NO_COLOR`/`--no-color` is set). The model already has `memstate_get`/`memstate_search`; do not add HTTP routes or MCP tools for these.
- **Releases & self-upgrade:** `healthVersion` in `server/main.go` is the single version source; CI (`.github/workflows/build.yml`) refuses a `v*` tag that doesn't match it, then publishes raw platform binaries (`memstated-<os>-<arch>[.exe]`, built by `make release`) as GitHub release assets. `memstated upgrade` (`server/upgrade.go`) downloads the matching asset over the current executable and stop-swap-restarts a shared daemon; before stopping it reads `/health` (`fetchHealth`) and `restartPlan` turns `embed_model`, `ollama_url`, `embed_timeout`, `idle_timeout` and `semantic_threshold` back into flags and env for the new daemon, so an upgrade never silently reverts a model or threshold (it did once, 2026-09-06). Asset names must stay in sync with `releaseAssetName()`. The daemon checks the releases API on startup + daily (`watchUpdates`) and nudges via a stderr line + `latest_available` in `/health`; `MEMSTATE_NO_UPDATE_CHECK` disables it (tests set it — hermetic daemons must not touch the network).
- **`memstated embed status|rebuild|prune` is CLI-only** (`cmdEmbed` in `server/main.go`, direct SQLite like dump/export; the status dashboard lives in `server/embedstatus.go`: `collectEmbedStatus` gathers, `renderEmbedStatus` prints, `similarityStats` samples up to 500 vectors for the pairwise cosine histogram and nearest-neighbour percentiles that guide `MEMSTATE_SEMANTIC_THRESHOLD`). Vectors are keyed by model name, so several sets coexist; `rebuild` rewrites one model's set from scratch, `prune` drops all others. Do not add an HTTP route or MCP tool for this.
- **Export/import is deliberately CLI-only** (`memstated export|import`, direct SQLite access in `server/export.go` + `server/main.go`). It's a human cross-machine workflow; do NOT add an MCP tool or HTTP route for it. Import merges by timestamp: keypaths absent locally get their full version history copied; existing keypaths take the file's latest value only when it is strictly newer (replayed through the normal write path, so dedupe/supersede/tombstone semantics apply and re-imports are no-ops). `import --force` skips the timestamp comparison so the file's latest always wins (identical state still dedupes).

## Where data lives

| Thing | Path |
|---|---|
| SQLite DB | `~/.memstate/memstate.db` (env `MEMSTATE_DB`; `~/` is expanded) |
| Daemon log | `~/.memstate/memstated.log` |
| Shared daemon address | `~/.memstate/daemon.addr` (next to the DB; written by a `--addr` daemon at startup, removed on shutdown; `discoverAddr()` reads `MEMSTATE_ADDR` first, then this file, and trusts the file only when `/health` answers) |
| Ollama URL | `http://127.0.0.1:11434` (env `MEMSTATE_OLLAMA_URL`; a URL ending in `/v1` is an OpenAI-compatible server) |
| Embed model | `nomic-embed-text` (env `MEMSTATE_EMBED_MODEL` or `--embed-model`) |
| Embed timeout | `60s` per Ollama call (env `MEMSTATE_EMBED_TIMEOUT` or `--embed-timeout`; must cover a cold load of a large model) |
| Semantic threshold | `0.5` (env `MEMSTATE_SEMANTIC_THRESHOLD` or per-request) |

Daemon binds loopback only. In attach mode, `MEMSTATE_DB` on the proxy is silently ignored (the running daemon picked its DB at startup) — the proxy emits a warning.
