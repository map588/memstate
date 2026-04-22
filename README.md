# memstate-local

A local memory server for AI agents. Stores facts, notes, and decisions
in a versioned, hierarchical SQLite database that your agent reads
before tasks and writes to after them — and that nothing else on the
internet ever sees.

Speaks MCP to Claude Code, Cursor, or any MCP-capable client. The
backing store is a single SQLite file on your machine. No API keys.
No hosted service. No daemon to babysit — it starts when your agent
does and stops when your agent does.

## Install

```bash
git clone <this repo>
cd memstate-local

cd server && go build -o memstated .
cd ../client && npm install && npm run build
```

You need Go 1.22+ and Node 18+. That's the whole toolchain.

## Wire it into your agent

**Claude Code** (one command):

```bash
claude mcp add --scope user -- memstate-local \
  node /abs/path/to/memstate-local/client/dist/index.js
```

**Anything else that reads an MCP JSON config** (Cursor, Windsurf,
Claude Desktop, …):

```json
{
  "mcpServers": {
    "memstate-local": {
      "command": "node",
      "args": ["/abs/path/to/memstate-local/client/dist/index.js"]
    }
  }
}
```

Restart the agent. Done. The first tool call launches the storage
daemon on a random loopback port; the daemon exits when the agent does.

### Verify

```bash
node client/dist/index.js --test
```

Expected: proxy spawns a daemon, prints its address plus the seven tool
names, exits cleanly.

## The seven tools

All scoped by `project_id`. Keypaths are explicit dot-notation
(`auth.provider`, `task.summary.2026-04-21`). No LLM-based extraction.

| Tool | Purpose |
|---|---|
| `memstate_set` | Write a short value at a keypath (`config.port = "8080"`). |
| `memstate_remember` | Write a markdown summary at a keypath. |
| `memstate_get` | Read a keypath, browse a subtree, or return the whole project tree. |
| `memstate_search` | Full-text (SQLite FTS5) search across current memories. |
| `memstate_history` | Every version of a keypath, newest first — including tombstones. |
| `memstate_delete` | Tombstone a keypath. History is preserved. |
| `memstate_delete_project` | Soft-delete a project. |

A useful agent loop:

- **At task start** → `memstate_get(project_id=<project>)` to load context
- **At task end** → `memstate_remember(project_id=<project>, keypath="task.summary.<date>", content="## Summary ...")`

`node client/dist/index.js init` writes rule files for several agents
(`CLAUDE.md`, `AGENTS.md`, `.cursor/rules/`, etc.) that encode this loop.

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

Search is SQLite FTS5 — fast, lexical, works offline. Semantic search
(via Ollama) is on the roadmap and has an empty embeddings table
waiting for it.

## Where your data lives

| Thing | Path |
|---|---|
| SQLite DB | `~/.memstate/memstate.db` (override with `MEMSTATE_DB`; `~/` is expanded) |
| Daemon log | `~/.memstate/memstated.log` |
| Network egress | None. The daemon binds `127.0.0.1` only. |

For a per-project DB, put it in the MCP config's `env:` block:

```json
{
  "memstate-local": {
    "command": "node",
    "args": ["/abs/path/to/client/dist/index.js"],
    "env": { "MEMSTATE_DB": "/abs/path/to/my_project.db" }
  }
}
```

## Sharing one daemon across agents (optional)

The default is one daemon per agent session: simple, clean, nothing to
garbage-collect. If you instead want one long-lived daemon that several
MCP clients and CLI scripts share, run it yourself and point callers at
it:

```bash
./server/memstated --addr 127.0.0.1:8765    # foreground; use nohup or a service manager if you want it detached
export MEMSTATE_ADDR=127.0.0.1:8765         # any MCP proxy / CLI seeing this will attach, not spawn
```

Stop / inspect:

```bash
./server/memstated stop   --addr 127.0.0.1:8765    # POST /admin/shutdown
./server/memstated status --addr 127.0.0.1:8765    # GET /health
```

Concurrent writers to the same DB file are fine — SQLite WAL serializes
them.

## Python CLI (for skills, hooks, scripts)

`client/skill/scripts/` has a Python CLI for each tool, using the same
child-vs-attach model as the MCP proxy. Each invocation with no
`MEMSTATE_ADDR` set spawns its own short-lived daemon and kills it on
exit.

```bash
python3 client/skill/scripts/memstate_remember.py \
  --project my_app \
  --keypath task.summary.2026-04-21 \
  --content "## Auth migration done"
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

- Semantic search via Ollama. Empty `embeddings` table in the schema.
- Time-travel reads (`at_revision` is accepted but ignored).
- Category / topic facets (same).

## License

MIT. Originally derived from
[memstate-ai/memstate-mcp](https://github.com/memstate-ai/memstate-mcp);
the storage engine, lifecycle model, and wire shape are all new.
