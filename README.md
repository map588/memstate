# memstate

A memory server for AI agents. It stores facts, notes, and decisions
in a versioned, hierarchical SQLite database. Your agent reads the
database before a task and writes to it after the task.

The server speaks MCP to Claude Code, Cursor, or any MCP-capable
client. The backing store is one SQLite file on your machine. No API
keys. No hosted service. No daemon to manage. The daemon starts when
your agent starts and stops when your agent stops.

## Install

You need Go 1.27+ and Node 18+. Semantic search also needs a local
[Ollama](https://ollama.com) with an embedding model pulled:

```bash
ollama pull nomic-embed-text
```

Any Ollama embedding model works. Select one with `MEMSTATE_EMBED_MODEL`
or `memstated --embed-model NAME`; for example:

```bash
ollama pull qwen3-embedding:4b
memstated --addr 127.0.0.1:8765 --embed-model qwen3-embedding:4b
```

If Ollama does not run, memstate still works. Writes and FTS search
are not affected. Semantic search returns 503 until the embedder is
available.

```bash
git clone git@github.com:map588/memstate.git
cd memstate
make install
```

This puts two programs on your PATH:

- `memstated`: the Go daemon. It installs to `$(go env GOPATH)/bin`, or to `$GOBIN` when set.
- `memstate-mcp`: the MCP stdio proxy, linked from `client/` with `npm link`.

`make uninstall` removes both. `make build` compiles in place and does
not touch PATH. `make test` runs the Go tests, the end-to-end smoke
test, and the MCP regression suite (`client/test/regression.mjs`), which
calls each tool through the real proxy and daemon against a temporary
database.

### Claude Code skill and hook (optional)

If you use Claude Code, `make install-skill` installs the bundled
skill under `~/.claude/skills/memstate/`. It also adds a
UserPromptSubmit hook. The hook points you to `memstate_remember`
after three or more file edits since your last persist.
`make uninstall-skill` removes both. The install is idempotent and
safe to run again. It replaces existing memstate entries in
`settings.json` and does not duplicate them.

## Connect memstate to your agent

**Claude Code** (one command):

```bash
claude mcp add --scope user -- memstate memstate-mcp
```

**Other clients that read an MCP JSON config** (Cursor, Windsurf,
Claude Desktop, and more):

```json
{
  "mcpServers": {
    "memstate": {
      "command": "memstate-mcp"
    }
  }
}
```

Restart the agent. The first tool call starts the storage daemon on a
random loopback port. The daemon exits when the agent exits.

### Without a global install

If you do not want to change PATH, skip `make install` and use
`make build`. Then point the MCP config at the built script:

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

Expected result: the proxy spawns a daemon, prints the daemon address
and the seven tool names, and exits cleanly.

## The seven tools

Every tool is scoped by `project_id`. The proxy derives the default id
from the git repository name (the directory basename outside a
repository), slugged to snake_case. Omit `project_id` in tool calls.
Pass it only to reach a different project. Keypaths use dot notation.

| Tool | Purpose |
|---|---|
| `memstate_set` | Write a short value at a keypath (`config.port = "8080"`). |
| `memstate_remember` | Write a markdown summary. An explicit keypath stores all content there. Without a keypath, each `## heading` becomes its own versioned memory, nested by `###` depth. |
| `memstate_get` | Read a keypath, browse a subtree, or return the full project tree. |
| `memstate_search` | Search current memories. `mode="fts"` (default) uses SQLite FTS5. `mode="semantic"` embeds the query with Ollama and ranks by cosine similarity against the embedding of each keypath's current content. Both modes accept `category` and `topics` filters. |
| `memstate_history` | Return every version of a keypath, newest first, with tombstones. |
| `memstate_delete` | Add a tombstone to a keypath. History is kept. |
| `memstate_delete_project` | Soft-delete a project. |

A useful agent loop:

- At task start, call `memstate_get()` to load the tree. The proxy derives the project id from the repository name. Call `memstate_search(query=..., mode="semantic")` when you do not know the exact keypath.
- At task end, call `memstate_remember(content="## Summary\n...\n## Decisions\n...")` and let the server extract the sections.

`node client/dist/index.js init` writes rule files for several agents
(`CLAUDE.md`, `AGENTS.md`, `.cursor/rules/`, and more). These files
encode this loop.

### `memstate_remember`: write shape

The tool returns `{ method, items: [{keypath, action, stored, superseded?}] }`
for both the explicit-keypath mode and the heading-extract mode.

- `method` is `"explicit"` or `"headings"`.
- `action` is `"created"`, `"superseded"`, or `"unchanged"`. `"superseded"` means a prior version existed at that keypath. `"unchanged"` means the content is identical to the current version, and no new row is written.
- In extract mode, each `##` heading becomes a top-level keypath (`## Auth` → `auth`). This is the same tree that explicit writes use. Pass `root: "<prefix>"` to nest all sections under a prefix.
- Common section names collapse to canonical slugs: `## TODOs` → `todo`, `## Open Questions` → `questions`, `## Files to touch` → `files`, and more.
- The server stores prose before the first `##` under `preamble`, or under `<root>.preamble` when a root is given.

### `memstate_search`: semantic mode

In `mode="semantic"`, the daemon embeds the query with Ollama. It
ranks results by cosine similarity against the embedding of the
**current content** at each keypath. One embedding row exists per
unique `(project, keypath, model)`, and the daemon recomputes it when
the content changes. Results below `threshold` (default 0.5) are
dropped. Set the threshold in the request or with
`MEMSTATE_SEMANTIC_THRESHOLD`. Each result pairs the keypath with the
current non-tombstoned memory and the similarity score. With
nomic-embed models, the daemon adds the `search_query:` and
`search_document:` task prefixes automatically. With Qwen3-Embedding
models, the daemon adds the retrieval instruction line to the query and
sends documents as raw text. Other models get raw text on both sides.

### Embedding models

Vectors are stored per model name, so a model switch does not destroy the
old set. The daemon reports its model in `/health` as `embed_model`, and
the proxy warns when its own `MEMSTATE_EMBED_MODEL` differs from that of
a daemon it attached to. After a switch, the daemon fills in the new
model's vectors on its next start. Use the `embed` subcommand to inspect
or rewrite the sets directly:

```bash
memstated embed status [--watch] [--probe]      # dashboard: coverage bars, sizes, threshold fit
memstated embed rebuild --model qwen3-embedding:4b   # drop and recompute one model's vectors
memstated embed prune --keep qwen3-embedding:4b      # delete every other model's vectors
```

`embed status` shows, per model, a coverage bar of current keypaths
with a vector, the vector dimension, storage size, and row count. It
also shows how many keypaths exceed the embed cap (only their head is
embedded), a histogram of pairwise cosine scores for the configured
model, the nearest-neighbour percentiles, and the share of pairs that
pass the current threshold. Use the last two to set
`MEMSTATE_SEMANTIC_THRESHOLD` for a new model: raise it until few pairs
pass but most nearest neighbours still do. `--watch` redraws every two
seconds while a backfill runs. `--probe` times one live Ollama call.

`rebuild` is the tool for a change in embedding structure under the same
model name: a new prompt format, a new Ollama build with different
output, or a corrupted set. It runs one Ollama call at a time and prints
progress per keypath.

## How storage works

Data is a versioned keypath tree, one tree per project:

```
project_id = "my_app"
├── auth.provider        v1: "JWT"              v2: "SuperTokens"   v3: tombstone
├── db.engine            v1: "Postgres 16"
└── task.summary.2026-04-21  v1: "## Refactor auth middleware …"
```

Each write appends a new version. If a prior version existed, the
response includes it as `superseded`. The agent sees the conflict, and
no data is overwritten silently. `memstate_history` returns the full
chain. `memstate_delete` appends a tombstone row. The data stays in
history but no longer appears in reads or search.

There are two search paths: SQLite FTS5 (fast, lexical, works offline)
and semantic search through Ollama embeddings. Embeddings are
asynchronous on write. The HTTP write returns immediately, and a
goroutine embeds the new content in the background. If Ollama is not
reachable, the daemon logs once per hour and continues. FTS search is
not affected. The daemon heals the missing vector the next time the
same content is written.

On each startup, the daemon backfills missing vectors in the
background, one Ollama call at a time. An embedding-model switch or a
period of Ollama downtime heals itself on the next start.

A soft-deleted project blocks reads. Any write to the project revives
it. A deleted keypath loses its embedding row and no longer appears in
search, but its full version history stays readable.

## Where your data lives

| Thing | Path |
|---|---|
| SQLite DB | `~/.memstate/memstate.db` (override with `MEMSTATE_DB`, and `~/` is expanded) |
| Daemon log | `~/.memstate/memstated.log` |
| Ollama URL | `http://127.0.0.1:11434` (override with `MEMSTATE_OLLAMA_URL` or `--ollama-url`) |
| Embed model | `nomic-embed-text` (override with `MEMSTATE_EMBED_MODEL` or `--embed-model`) |
| Embed timeout | `60s` per Ollama call (override with `MEMSTATE_EMBED_TIMEOUT` or `--embed-timeout`). Must cover a cold model load: a 4B model needs about 20s on first use. |
| Semantic threshold | `0.5` (override with `MEMSTATE_SEMANTIC_THRESHOLD` or per request) |
| Network egress | The daemon binds `127.0.0.1` only. Ollama calls, when enabled, go to the configured Ollama URL, which is usually also loopback. |

For a per-project database, set `MEMSTATE_DB` in the MCP config's
`env:` block:

```json
{
  "memstate": {
    "command": "memstate-mcp",
    "env": { "MEMSTATE_DB": "/abs/path/to/my_project.db" }
  }
}
```

The proxy starts the daemon, so it also decides the embedding model.
Pass it as a proxy argument, and the proxy hands it to every daemon it
spawns. A flag wins over the matching environment variable.

```json
{
  "memstate": {
    "command": "memstate-mcp",
    "args": ["--embed-model", "qwen3-embedding:4b"]
  }
}
```

`memstate-mcp setup` asks for the model, lists what your local Ollama
serves, and writes the choice into each agent config. Pass
`--embed-model NAME` to skip the prompt. `--ollama-url` and
`--embed-timeout` work the same way.

## Share one daemon across agents (optional)

The default is one daemon per agent session. This is simple, and there
is nothing to clean up. If you want one long-lived daemon that several
MCP clients and CLI scripts share, set `MEMSTATE_ADDR`. The proxy then
spawns a detached daemon on first use:

```bash
export MEMSTATE_ADDR=127.0.0.1:8765
export MEMSTATE_IDLE_TIMEOUT=30m    # optional: the daemon exits after 30 minutes idle
```

Any MCP proxy or CLI script with these variables set attaches to a
running daemon on port `8765`. If no daemon runs there, it spawns one
detached. The daemon outlives the proxy. With `MEMSTATE_IDLE_TIMEOUT`,
the daemon also exits when nothing has used it for the timeout period.

Start, stop, or inspect the daemon manually:

```bash
memstated --addr 127.0.0.1:8765 --idle-timeout 30m   # foreground
memstated stop   --addr 127.0.0.1:8765               # POST /admin/shutdown
memstated status --addr 127.0.0.1:8765               # GET /health
```

Concurrent writers to the same database file are safe. SQLite WAL
serializes them.

## Python CLI (for skills, hooks, scripts)

`client/skill/scripts/` contains a Python CLI for each tool. The CLI
uses the same child-or-attach model as the MCP proxy. Without
`MEMSTATE_ADDR` set, each invocation spawns its own short-lived daemon
and stops it on exit.

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

See `client/skill/SKILL.md` for the skill usage contract.

## How the pieces fit

```
Claude Code ──stdio──> client/dist/index.js ──HTTP loopback──> server/memstated ──> SQLite
               (MCP)        (thin TS proxy)                       (Go daemon)
```

The TypeScript proxy exists only to speak MCP. Each tool call becomes
one HTTP POST. All logic (keypath versioning, FTS, conflict detection,
tombstones) lives in the Go daemon.

Lifetime:

- The daemon listens on `127.0.0.1:0` by default (an OS-picked port) and prints `MEMSTATE_READY addr=127.0.0.1:<port>` on stderr.
- The proxy reads the banner and passes its own PID with `--owner-pid`.
- On a clean exit, the proxy sends SIGTERM to the daemon. After a SIGKILL, the daemon's `kill(owner_pid, 0)` poll notices within about 2 seconds, and the daemon exits on its own.

This is the child mode. The `--addr` flag (above) is the only other
mode.

## Not done yet

- LLM fallback for keypath extraction when headings are absent. Such content now lands under `<root>.preamble`.
- Time-travel reads. The server now rejects an `at_revision` request field instead of ignoring it.

## License

MIT. The project derives from
[memstate-ai/memstate-mcp](https://github.com/memstate-ai/memstate-mcp).
The storage engine, the lifecycle model, and the wire shape are all
new.
