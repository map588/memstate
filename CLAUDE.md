# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & test

Two-process project: Go daemon under `server/`, TypeScript MCP proxy under `client/`. Requires Go 1.26+ and Node 18+.

The Makefile at the repo root is the canonical entrypoint:

```bash
make build            # compile both in-place (server/memstated, client/dist/)
make install          # put `memstated` on GOBIN and `memstate-mcp` on PATH via npm link
make uninstall        # reverse of install
make install-skill    # copy skill to ~/.claude/skills/memstate + add UserPromptSubmit hook
make uninstall-skill  # reverse of install-skill
make test             # go test + go vet + TS end-to-end smoke
make clean            # remove build artifacts
```

Raw commands for partial work:

```bash
cd server && go build -o memstated .          # Go daemon only
cd client && npm install && npm run build     # TS proxy only
cd server && go test -run TestStoreRoundTrip  # single Go test
node client/dist/index.js --test              # end-to-end: spawn daemon, hit /health, list tools
```

Running the daemon directly:

```bash
./server/memstated                                       # child mode, random port, banner on stderr
./server/memstated --addr 127.0.0.1:8765                 # shared-daemon mode
./server/memstated --addr 127.0.0.1:8765 --idle-timeout 30m   # long-lived, self-exits when idle
./server/memstated status --addr 127.0.0.1:8765
./server/memstated stop   --addr 127.0.0.1:8765
./server/memstated projects                              # list live projects with memory counts
./server/memstated export --all                          # full-history JSON to ~/.memstate/exports/
./server/memstated import backup.json                    # timestamp-merge into the local DB
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
- **Detached MEMSTATE_DB warning:** the proxy only warns that `MEMSTATE_DB` is ignored when it *attached* to an already-running daemon. When it lazy-spawns, the child inherits env, so the warning would be wrong — tracked via the `alreadyRunning` flag in `attach()`.

A manually-started `--addr` daemon that finds the port busy probes `/health` itself: if occupant is ours → exit 0 quietly; otherwise exit 2 loudly. The proxy translates exit 2 from a lazy-spawn into a readable "port occupied by non-memstate" error.

### Idle-exit (`server/main.go`)

`--idle-timeout` (env `MEMSTATE_IDLE_TIMEOUT`) wraps the router in `activityMiddleware` that stamps `lastActivity` on every request; a `watchIdle` goroutine polls and triggers `shutdownFn` once no request has arrived for the timeout. Disabled when `--owner-pid` is set (parent already owns our lifetime). Poll interval is `timeout/4` clamped to `[5s, 60s]`.

### Versioned keypath store (`server/store.go`)

- Data model: per-project dot-notation keypath tree. Each write appends a new row to `memories` (never updates). Prior version is returned as `superseded` so the caller sees the conflict.
- Identical content AND metadata (category/topics) to current version is a no-op → `action: "unchanged"`, no new row. Same content with different metadata DOES version.
- `Delete` appends a tombstone row; history is preserved. `ProjectDeleted` gates reads and deletes only — **any write revives a soft-deleted project** (`ensureProject`'s `ON CONFLICT ... SET deleted_at = NULL`).
- FTS5 virtual table `memories_fts` is the default search; only the current version is indexed (the superseded version's FTS row is deleted on write). Free-text queries are token-quoted (`ftsQuote`) so punctuation can't hit FTS5 operator syntax.
- Semantic search uses `keypath_embeddings` (one row per `(project, keypath, model)`), where the vector is computed from the **current content** at that keypath — recomputed on content change, deleted on tombstone, healed on an unchanged write if the row is missing (e.g. Ollama was down). The `meta` table's `embed_source` row wipes all vectors on startup when the embedding scheme changes.
- `category` (string) and `topics` (JSON array in TEXT) are per-version columns; `/memories/search` filters on them (topics = match-any, via `json_each`).
- Storage runs on a single pooled connection (SQLite WAL + tuned pragmas); `Write` wraps read-latest + insert in a transaction so concurrent same-keypath writers can't collide on the version unique index.

### Heading extraction (`server/extract.go`)

`memstate_remember` without an explicit keypath runs `ExtractHeadings`, which maps each `##`+ heading to a top-level keypath segment (deeper headings nest via dots; optional `root` request field nests everything under a prefix) and collapses common section names (`## TODOs` → `todo`, `## Files to touch` → `files`, etc.) via `reservedAliases`. Fenced code blocks are ignored when scanning for headings. Pre-heading prose lands under `preamble` (or `<root>.preamble`). The TS proxy derives a default `project_id` from the git repo name (cwd basename outside a repo), slugged snake_case, applied whenever a tool call omits project_id; the Python skill scripts share the same rule via `_client.default_project()`.

### Embeddings (`server/embed.go`)

Ollama-backed content embeddings are fire-and-forget from the write path via `maybeEmbedContent` → `embedder.inFlight`. Writes succeed even if Ollama is down; errors are throttled to one log per model per hour. Semantic search returns 503 when the embedder is disabled. nomic-embed models get `search_document:` / `search_query:` task prefixes (`EmbedDocument` / `EmbedQuery`); other models get raw text. On startup `BackfillEmbeddings` eagerly embeds every current keypath missing a vector for the configured model (sequential, aborts on first error — the next startup or per-write heal retries). Tests use `Embedder.WaitForPending()` for determinism — production never waits.

## Conventions (non-obvious)

- **No co-authoring on commits.** Do not add `Co-Authored-By:` trailers.
- New tool → edit three places in lockstep: `server/http.go` (route + handler), `client/src/index.ts` (`TOOLS` entry), `client/skill/scripts/` (Python CLI). The skill scripts are a supported interface, not a legacy artifact.
- `SKILL.md` in `client/skill/` now describes THIS daemon accurately (integer IDs, synchronous writes, headings-only extraction, category/topics filterable) and is the canonical statement of naming conventions (snake_case ids/segments, YYYY_MM_DD dates, `branches.<slug>.*` for branch-scoped state). Keep it, the MCP `INSTRUCTIONS`/tool descriptions in `client/src/index.ts`, and the Python `--help` strings in agreement when conventions change.
- `.claude/hooks/memstate-persist-reminder.sh` runs on `UserPromptSubmit` and nudges toward `memstate_remember` after ≥3 file edits since the last persist. If you want to silence it for a session, touch a trivial `memstate_set` or `memstate_remember` call.
- Formerly accepted-but-ignored fields: `category`/`topics` are now stored and filterable; `at_revision` and `context` were dropped and are rejected by `DisallowUnknownFields`. Do not add silently-ignored request fields — accept a field only when it does something.
- **Export/import is deliberately CLI-only** (`memstated export|import`, direct SQLite access in `server/export.go` + `server/main.go`). It's a human cross-machine workflow; do NOT add an MCP tool or HTTP route for it. Import merges by timestamp: keypaths absent locally get their full version history copied; existing keypaths take the file's latest value only when it is strictly newer (replayed through the normal write path, so dedupe/supersede/tombstone semantics apply and re-imports are no-ops).

## Where data lives

| Thing | Path |
|---|---|
| SQLite DB | `~/.memstate/memstate.db` (env `MEMSTATE_DB`; `~/` is expanded) |
| Daemon log | `~/.memstate/memstated.log` |
| Ollama URL | `http://127.0.0.1:11434` (env `MEMSTATE_OLLAMA_URL`) |
| Embed model | `nomic-embed-text` (env `MEMSTATE_EMBED_MODEL`) |
| Semantic threshold | `0.5` (env `MEMSTATE_SEMANTIC_THRESHOLD` or per-request) |

Daemon binds loopback only. In attach mode, `MEMSTATE_DB` on the proxy is silently ignored (the running daemon picked its DB at startup) — the proxy emits a warning.
