# memstate

A memory server for AI agents. Stores facts, notes, and decisions
in a versioned, hierarchical SQLite database that your agent reads
before tasks and writes to after them.

Speaks MCP to Claude Code, Cursor, or any MCP-capable client. The
backing store is a single SQLite file on your machine. No API keys.
No hosted service. No daemon to babysit — it starts when your agent
does and stops when your agent does.

## Install

Requires Go 1.26+ and Node 18+. Semantic search additionally wants a
local [Ollama](https://ollama.com) with an embedding model pulled:

```bash
ollama pull nomic-embed-text
```

If Ollama isn't running, memstate still works — writes and FTS search
are unaffected; semantic search returns 503 until the embedder is up.

```bash
git clone git@github.com:map588/memstate.git
cd memstate
make install
```

That puts two things on your PATH:

- `memstated` — the Go daemon, installed to `$(go env GOPATH)/bin` (or `$GOBIN` if set)
- `memstate-mcp` — the MCP stdio proxy, `npm link`'d from `client/`

`make uninstall` reverses both. `make build` compiles in-place without
touching PATH. `make test` runs Go tests + the end-to-end smoke.

### Claude Code skill + hook (optional)

If you use Claude Code, `make install-skill` also installs the bundled
skill under `~/.claude/skills/memstate/` and adds a UserPromptSubmit
hook that nudges toward `memstate_remember` after ≥3 file edits since
your last persist. `make uninstall-skill` removes both. Idempotent —
safe to re-run; existing memstate entries in `settings.json` are
replaced, not duplicated.

## Wire it into your agent

**Claude Code** (one command):

```bash
claude mcp add --scope user -- memstate memstate-mcp
```

**Anything else that reads an MCP JSON config** (Cursor, Windsurf,
Claude Desktop, …):

```json
{
  "mcpServers": {
    "memstate": {
      "command": "memstate-mcp"
    }
  }
}
```

Restart the agent. Done. The first tool call launches the storage
daemon on a random loopback port; the daemon exits when the agent does.

### Without a global install

If you'd rather not touch PATH, skip `make install` and use `make
build`, then point the MCP config at the built script directly:

```json
{
  "mcpServers": {
    "memstate": {
      "command": "node",
      "args": ["/abs/path/to/memstate-mcp/client/dist/index.js"]
    }
  }
}
```

### Verify

```bash
node client/dist/index.js --test
```

Expected: proxy spawns a daemon, prints its address plus the seven tool
names, exits cleanly.

## The seven tools

All scoped by `project_id` (snake_case, usually the repo name). Keypaths
are dot-notation.

| Tool | Purpose |
|---|---|
| `memstate_set` | Write a short value at a keypath (`config.port = "8080"`). |
| `memstate_remember` | Write a markdown summary. Explicit keypath writes there; omit the keypath and each `## heading` becomes its own versioned memory nested by `###` depth. |
| `memstate_get` | Read a keypath, browse a subtree, or return the whole project tree. |
| `memstate_search` | Search current memories. `mode="fts"` (default) uses SQLite FTS5; `mode="semantic"` embeds the query via Ollama and cosine-ranks against stored keypath vectors. |
| `memstate_history` | Every version of a keypath, newest first — including tombstones. |
| `memstate_delete` | Tombstone a keypath. History is preserved. |
| `memstate_delete_project` | Soft-delete a project. |

A useful agent loop:

- **At task start** → `memstate_get(project_id=<project>)` to load the tree; `memstate_search(query=..., mode="semantic")` when the exact keypath isn't known.
- **At task end** → `memstate_remember(project_id=<project>, content="## Summary\n...\n## Decisions\n...")` and let the server extract.

`node client/dist/index.js init` writes rule files for several agents
(`CLAUDE.md`, `AGENTS.md`, `.cursor/rules/`, etc.) that encode this loop.

### `memstate_remember` — write shape

Returns `{ method, items: [{keypath, action, stored, superseded?}] }`
for both explicit-keypath and heading-extract modes.

- `method` is `"explicit"` or `"headings"`.
- `action` is `"created"`, `"superseded"` (a prior version at that keypath existed), or `"unchanged"` (identical content to current live version — no new row written).
- When extracting, each `##` becomes a keypath prefixed with `<project_id>.` by default. Pass `root: ""` to disable the prefix, or `root: "<anything>"` to override.
- Common section names collapse to canonical slugs: `## TODOs → todo`, `## Open Questions → questions`, `## Files to touch → files`, etc.
- Prose before the first `##` is captured under `<root>.preamble`.

### `memstate_search` — semantic mode

Under `mode="semantic"` the daemon embeds the query via Ollama, cosine-
ranks against stored **keypath** embeddings (one row per unique
`(project, keypath, model)` — keypaths are stable across versions), and
filters by `threshold` (default 0.5). Tune via the request field or
`MEMSTATE_SEMANTIC_THRESHOLD`. Each result pairs the keypath with the
current non-tombstoned memory at that keypath and the similarity score.

## How storage works

Data is a versioned keypath tree, per project:

```
project_id = "my_app"
├── auth.provider        v1: "JWT"              v2: "SuperTokens"   v3: tombstone
├── db.engine            v1: "Postgres 16"
└── task.summary.2026-04-21  v1: "## Refactor auth middleware …"
```

Each write appends a new version. If a prior version existed, the
response includes it as `superseded` so the agent sees the conflict
rather than silently overwriting. `memstate_history` walks the full
chain. `memstate_delete` appends a tombstone row: the data is still
there in history, but no longer surfaces in reads or search.

Two search paths: SQLite FTS5 (fast, lexical, works offline) and
semantic keypath search via Ollama embeddings. Embeddings are fire-and-
forget on write — the HTTP write returns immediately and a goroutine
embeds the new keypath in the background. If Ollama is unreachable the
daemon logs once per hour and moves on; FTS search is unaffected.

## Where your data lives

| Thing | Path |
|---|---|
| SQLite DB | `~/.memstate/memstate.db` (override with `MEMSTATE_DB`; `~/` is expanded) |
| Daemon log | `~/.memstate/memstated.log` |
| Ollama URL | `http://127.0.0.1:11434` (override `MEMSTATE_OLLAMA_URL`) |
| Embed model | `nomic-embed-text` (override `MEMSTATE_EMBED_MODEL`) |
| Semantic threshold | `0.5` (override `MEMSTATE_SEMANTIC_THRESHOLD` or per-request) |
| Network egress | The daemon binds `127.0.0.1` only. Ollama calls, when enabled, go to the configured Ollama URL — typically also loopback. |

For a per-project DB, put it in the MCP config's `env:` block:

```json
{
  "memstate": {
    "command": "memstate-mcp",
    "env": { "MEMSTATE_DB": "/abs/path/to/my_project.db" }
  }
}
```

## Sharing one daemon across agents (optional)

The default is one daemon per agent session: simple, clean, nothing to
garbage-collect. If you instead want one long-lived daemon that several
MCP clients and CLI scripts share, set `MEMSTATE_ADDR` and the proxy
will **lazy-spawn** a detached daemon on first use:

```bash
export MEMSTATE_ADDR=127.0.0.1:8765
export MEMSTATE_IDLE_TIMEOUT=30m    # optional: daemon self-exits after 30m idle
```

Any MCP proxy or CLI script with those vars set will attach to a running
daemon on `8765`, or spawn one detached if nobody's there. The daemon
outlives the proxy; with `MEMSTATE_IDLE_TIMEOUT` it also cleans up after
itself when nothing's been using it.

Start / stop / inspect manually:

```bash
memstated --addr 127.0.0.1:8765 --idle-timeout 30m   # foreground
memstated stop   --addr 127.0.0.1:8765               # POST /admin/shutdown
memstated status --addr 127.0.0.1:8765               # GET /health
```

Concurrent writers to the same DB file are fine — SQLite WAL serializes
them.

## Python CLI (for skills, hooks, scripts)

`client/skill/scripts/` has a Python CLI for each tool, using the same
child-vs-attach model as the MCP proxy. Each invocation with no
`MEMSTATE_ADDR` set spawns its own short-lived daemon and kills it on
exit.

```bash
# Explicit keypath
python3 client/skill/scripts/memstate_remember.py \
  --project my_app \
  --keypath task.summary.2026-04-21 \
  --content "## Auth migration done"

# Or omit --keypath to extract one memory per heading
python3 client/skill/scripts/memstate_remember.py \
  --project my_app \
  --content "$(cat summary.md)"

# Semantic search
python3 client/skill/scripts/memstate_search.py \
  --project my_app --mode semantic --query "how do users log in"
```

See `client/skill/SKILL.md` for the skill-style usage contract.

## How the pieces fit

```
Claude Code ──stdio──> client/dist/index.js ──HTTP loopback──> server/memstated ──> SQLite
               (MCP)        (thin TS proxy)                       (Go daemon)
```

The TypeScript proxy exists only to speak MCP — every tool call becomes
one HTTP POST. All logic (keypath versioning, FTS, conflict detection,
tombstones) lives in the Go daemon.

Lifetime:

- Daemon listens on `127.0.0.1:0` by default (OS-picked port) and
  prints `MEMSTATE_READY addr=127.0.0.1:<port>` on stderr.
- Proxy reads the banner, passes its own PID via `--owner-pid`.
- On clean exit the proxy SIGTERMs the daemon. On SIGKILL, the daemon's
  `kill(owner_pid, 0)` poll notices within ~2 s and exits on its own.

This is the child mode. The `--addr` flag (above) is the only other
mode.

## Not done yet

- LLM fallback for keypath extraction when headings are absent (currently such content lands under `<root>.preamble`).
- Time-travel reads (`at_revision` is accepted but ignored).
- Category / topic facets (same).
- `reindex-embeddings` subcommand for when `MEMSTATE_EMBED_MODEL` changes — existing embeddings get silently ignored on search until their keypaths are written again.

## License

MIT. Originally derived from
[memstate-ai/memstate-mcp](https://github.com/memstate-ai/memstate-mcp);
the storage engine, lifecycle model, and wire shape are all new.
