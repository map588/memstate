# memstate

A memory server for AI agents. It stores facts, notes, and decisions
in a versioned, hierarchical SQLite database. Your agent reads the
database before a task and writes to it after the task.

The server speaks MCP to Claude Code, Cursor, or any MCP-capable
client. The backing store is one SQLite file on your machine. No API
keys. No hosted service. No daemon to manage. The first agent starts
the daemon, and all later agents on the same machine use it.

## Install

### Install script

You need Node 18 or later. The script checks for it before it downloads
anything.

macOS or Linux (amd64 or arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/map588/memstate/main/install.sh | bash
```

Windows 10 1803 or later on amd64, or Windows 11 on arm64 (it runs the
amd64 build), in PowerShell:

```powershell
irm https://raw.githubusercontent.com/map588/memstate/main/install.ps1 | iex
```

The script does these steps:

1. It downloads the newest [release](https://github.com/map588/memstate/releases):
   the `memstated` daemon for your platform and `memstate-mcp.tar.gz`,
   which holds the MCP proxy and its Node packages.
2. It installs both. Then it starts the proxy once with a scratch
   database, to make sure that the proxy finds the daemon.
3. In a terminal, it runs `memstate-mcp setup`. Setup finds Claude Code,
   Claude Desktop, Cursor and Windsurf, asks for the embedding model, and
   asks before it changes a config.
4. It asks whether to install the Claude Code skill and hooks (see
   below).

Where the files go:

| | macOS, Linux | Windows |
|---|---|---|
| Programs | `~/.local/share/memstate/` (`$XDG_DATA_HOME/memstate` when set) | `%LOCALAPPDATA%\Programs\memstate\` |
| Commands | links in `~/.local/bin`: `memstated`, `memstate`, `memstate-mcp` | the `server\` directory of the programs, added to your user PATH |

The proxy finds the daemon next to it, in `server/`, so the MCP config
does not depend on PATH. Your memories stay in `~/.memstate/`.

Set these environment variables to change what the script does:

- `MEMSTATE_VERSION=v0.8.0` installs that release, not the newest one.
- `MEMSTATE_INSTALL_DIR` changes the directory for the links (macOS, Linux).
- `MEMSTATE_SETUP=0` or `1` skips or runs `memstate-mcp setup` with no prompt.
- `MEMSTATE_INSTALL_SKILL=0` or `1` skips or installs the skill with no prompt.

For example:

```bash
curl -fsSL https://raw.githubusercontent.com/map588/memstate/main/install.sh | MEMSTATE_INSTALL_SKILL=1 bash
```

```powershell
$env:MEMSTATE_INSTALL_SKILL = '1'; irm https://raw.githubusercontent.com/map588/memstate/main/install.ps1 | iex
```

To update, run the script again. It replaces the daemon and the proxy
together. `memstated upgrade` replaces only the daemon.

To uninstall on macOS or Linux:

```bash
python3 ~/.local/share/memstate/configure-claude-hook.py uninstall   # removes the hooks from settings.json
rm -rf ~/.local/share/memstate ~/.claude/skills/memstate ~/.claude/skills/memstate-precompact ~/.claude/hooks/memstate-*.sh
rm -f ~/.local/bin/memstated ~/.local/bin/memstate ~/.local/bin/memstate-mcp
claude mcp remove memstate --scope user
```

To uninstall on Windows:

```powershell
python "$env:LOCALAPPDATA\Programs\memstate\configure-claude-hook.py" uninstall
Remove-Item -Recurse -Force "$env:LOCALAPPDATA\Programs\memstate", "$HOME\.claude\skills\memstate", "$HOME\.claude\skills\memstate-precompact", "$HOME\.claude\hooks\memstate-*.sh"
claude mcp remove memstate --scope user
```

Then remove `%LOCALAPPDATA%\Programs\memstate\server` from your user PATH.
Remove the `memstate` entry from the config of each other MCP client.

### Build from source

You need Go 1.27+ and Node 18+.

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
test, and the MCP regression suite (`client/test/regression.mjs`). The
regression suite calls each tool through the real proxy and daemon
against a temporary database.

On Windows the same `make` targets run from cmd.exe or PowerShell with
GNU make (for example `choco install make`), Go, Node and Python on PATH.
You do not need bash or coreutils. The build produces `memstated.exe`,
and `make install` copies it to GOBIN as `memstated.exe` and
`memstate.exe` instead of a symlink. Git Bash and WSL work too. The hook
scripts that `make install-skill` copies are bash scripts and need Git
Bash to run.

### Embeddings (optional)

Semantic search needs a local [Ollama](https://ollama.com) with an
embedding model pulled:

```bash
ollama pull nomic-embed-text
```

Any Ollama embedding model works. Select one with `MEMSTATE_EMBED_MODEL`
or `memstated --embed-model NAME`. For example:

```bash
ollama pull qwen3-embedding:4b
memstated --addr 127.0.0.1:8765 --embed-model qwen3-embedding:4b
```

If Ollama does not run, memstate still works. Writes and FTS search
are not affected. The default hybrid search returns FTS hits alone and
sets `degraded` in the response. Explicit semantic search returns 503
until the embedder is available.

A server with an OpenAI-compatible embeddings API also works, for
example the llama.cpp server, LM Studio, or vLLM. Set
`MEMSTATE_EMBEDDING_URL` (or `--embedding-url`) to its base URL, which
ends in `/v1`. The daemon then sends `POST {url}/embeddings` with
`{"model", "input"}` instead of Ollama's `/api/embeddings`. For
example, with the llama.cpp server and nomic-embed-text:

```bash
llama-server -m nomic-embed-text-v1.5.Q8_0.gguf --embeddings --pooling mean --alias nomic-embed-text --port 8081
memstated --addr 127.0.0.1:8765 --embedding-url http://127.0.0.1:8081/v1
```

`MEMSTATE_OLLAMA_URL` and `--ollama-url` are the old names. They still
work, print a notice, and will be removed.

When the server rejects a long memory ("context length" from Ollama,
"too large to process" from llama.cpp), the daemon halves the text and
tries again.

The daemon limits the embedding calls that it sends to the server at the
same time: two for content embeds and the backfill, four for search
queries. Many agents cannot overload one local model server, and a burst
of writes does not make searches wait.

### Claude Code skill and hook (optional)

If you use Claude Code, the install script asks whether to install the
bundled skills. From a clone, `make install-skill` installs them. The
memstate skill goes to `~/.claude/skills/memstate/`, the pre-compact
skill to `~/.claude/skills/memstate-precompact/`, and two
UserPromptSubmit hooks are added:

- `memstate-persist-reminder.sh` points you to `memstate_remember`
  after three or more file edits since your last persist.
- `memstate-recall.sh` runs `memstated recall`. It searches the
  current repository's memories with the prompt text (hybrid mode)
  and injects up to three hits the model has not seen in this
  session. It finds the shared daemon through `MEMSTATE_ADDR` or
  `~/.memstate/daemon.addr` (see "One daemon for all agents"). A
  private daemon (`MEMSTATE_CHILD=1`) is not visible to the hook, and
  the hook then prints nothing. When the daemon runs a different
  version or build than the hook, the hook prints a notice one time
  and asks you to restart the daemon. Set `MEMSTATE_NO_RECALL=1` to
  turn the hook off.

The skill scripts need Python 3. The hooks are bash scripts. On Windows,
Claude Code runs hooks with Git Bash, so the install script adds the
hooks only when it finds Git Bash. The persist reminder also needs `jq`
and does nothing without it.

Before a `/compact`, run `/memstate-precompact`. The agent writes the
session state (task summary, decisions, gotchas, todo, open questions)
to memstate and prints the `/compact` line to paste. That line tells
the summary what to keep and tells the next context window to read
those keypaths first.

`make uninstall-skill` removes both skills and both hooks. The install
is idempotent and safe to run again. It replaces existing memstate
entries in `settings.json` and does not duplicate them.

## Connect memstate to your agent

The install script runs `memstate-mcp setup`, which writes the config
for you (with `node` and the full path of the proxy). To do it by hand:

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

Restart the agent. The first proxy starts the shared storage daemon on
`127.0.0.1:8765`. Later proxies attach to it. The daemon continues
to run after the agent exits (see "One daemon for all agents").

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
memstate-mcp --test
```

From a clone without `make install`, run `node client/dist/index.js --test`.

Expected result: the proxy attaches to the shared daemon or starts one.
It prints the daemon address, the daemon mode (`shared`, `attach` or
`child`), and the seven tool names. Then it exits.

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
| `memstate_search` | Search current memories. `mode="hybrid"` (default) fuses an any-word FTS5 match with the semantic ranking. `mode="fts"` uses SQLite FTS5 and requires every word. `mode="semantic"` embeds the query with Ollama and ranks by cosine similarity against the embedding of each keypath's current content. All modes accept `category` and `topics` filters. |
| `memstate_history` | Return every version of a keypath, newest first, with tombstones. |
| `memstate_delete` | Add a tombstone to a keypath. History is kept. |
| `memstate_delete_project` | Soft-delete a project. |

A useful agent loop:

- At task start, call `memstate_get()` to load the tree. The proxy derives the project id from the repository name. The response also carries the user scope under `user`. Call `memstate_search(query=...)` when you do not know the exact keypath.
- At task end, call `memstate_remember(content="## Summary\n...\n## Decisions\n...")` and let the server extract the sections.
- Facts about the user or this machine, not about the code, go to `scope="user"`: `preferences.*`, `profile.*`, `host.<host_slug>.env.*`, `host.<host_slug>.tools.*`. The daemon rejects any other keypath there, so decisions and task summaries cannot leak into a shared store.
- When the prompt is clearly about another subject than the directory (for example an nginx config asked from the home directory), pin the session once with `project_name`. Prefer an existing id. A new id also needs `new_project=true`. The proxy refuses a new id that looks like an existing one. On the first prompt, the recall hook prints a `<memstate-scope>` block. It shows the cwd project, whether it exists, and the other projects that the prompt matches. The recall hook follows a session pin through a per-process pin file.
- A write creates a project only when it targets the git repository you are in, or when it carries `new_project=true`. This rule applies to the cwd project outside a repository, to an explicit `project_id`, and to a soft-deleted project. The proxy always refuses a name that resembles an existing project, and the name of your home directory. From the home directory, the proxy refuses all writes to the default project. Pin a project or use the user scope. Ids that start with `_` are reserved for the user scope. The Python scripts apply the same rule with `--new-project`.
- Never save a denied prompt. A denied prompt is a tool call that the user or a permission check denied.

`node client/dist/index.js init` writes rule files for several agents
(`CLAUDE.md`, `AGENTS.md`, `.cursor/rules/`, and more). These files
encode this loop.

### `memstate_remember`: write shape

The tool returns `{ method, items: [{keypath, action, stored, superseded?}] }`
for both the explicit-keypath mode and the heading-extract mode. `stored`
and `superseded` name the versions and carry no content. `superseded` has
a 40-word `preview`. Search hits carry a `preview` too, and the agent reads
the keypaths it wants with `memstate_get`.

- `method` is `"explicit"` or `"headings"`.
- `action` is `"created"`, `"superseded"`, or `"unchanged"`. `"superseded"` means a prior version existed at that keypath. `"unchanged"` means the content is identical to the current version, and no new row is written.
- In extract mode, each `##` heading becomes a top-level keypath (`## Auth` → `auth`). This is the same tree that explicit writes use. Pass `root: "<prefix>"` to nest all sections under a prefix.
- Common section names collapse to canonical slugs: `## TODOs` → `todo`, `## Open Questions` → `questions`, `## Files to touch` → `files`, and more.
- The server stores prose before the first `##` under `preamble`, or under `<root>.preamble` when a root is given.

### `memstate_search`: hybrid mode

`mode="hybrid"` is the default. The daemon runs two searches. The first
is an FTS5 match where any query word may hit, ranked by bm25. The
second is the semantic search described below. Reciprocal rank fusion
(k=60) merges the two lists.
A keypath found by both searches outranks one found by only one. Each
result carries `score` (the fused score) and `sources` (`fts`,
`semantic`, or both). When the embedder is not configured or the query
embedding fails, the response holds the FTS hits alone and `degraded`
names the reason. Hybrid search never fails because Ollama is down.

### `memstate_search`: semantic mode

In `mode="semantic"`, the daemon embeds the query with the embedding
server (Ollama by default). It
ranks results by cosine similarity against the embedding of the
**current content** at each keypath. One embedding row exists per
unique `(project, keypath, model)`, and the daemon recomputes it when
the content changes. The daemon drops results below `threshold`
(default 0.5). Set the threshold in the request or with
`MEMSTATE_SEMANTIC_THRESHOLD`. Each result pairs the keypath with the
current non-tombstoned memory and the similarity score. With
nomic-embed models, the daemon adds the `search_query:` and
`search_document:` task prefixes automatically. With Qwen3-Embedding
models, the daemon adds the retrieval instruction line to the query and
sends documents as raw text. Other models get raw text on both sides.

### Embedding models

The daemon keys vectors by model name, so a model switch does not
destroy the old set. The daemon reports its model in `/health` as `embed_model`, and
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
also shows how many keypaths exceed the embed cap. The daemon embeds
only the head of such a keypath. For the configured model, it shows a
histogram of pairwise cosine scores, the nearest-neighbor percentiles,
and the share of pairs that pass the current threshold. Use the last
two to set `MEMSTATE_SEMANTIC_THRESHOLD` for a new model. Raise it
until few pairs pass but most nearest neighbors still do. `--watch` redraws every two
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
└── task.summary.2026_04_21  v1: "## Refactor auth middleware …"
```

Each write appends a new version. If a prior version existed, the
response names it as `superseded` with a 40-word preview. The agent sees the conflict, and
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

A soft-deleted project blocks reads. A write to the project revives
it. Through MCP, that write also needs `new_project=true`. A deleted keypath loses its embedding row and no longer appears in
search, but its full version history stays readable.

## Where your data lives

| Thing | Path |
|---|---|
| SQLite DB | `~/.memstate/memstate.db` (override with `MEMSTATE_DB`, and `~/` is expanded) |
| Daemon log | `memstated.log` next to the DB (default `~/.memstate/memstated.log`) |
| Shared daemon address | `~/.memstate/daemon.addr`, next to the DB. A `--addr` daemon writes it at startup and removes it at shutdown. |
| Embedding URL | `http://127.0.0.1:11434` (override with `MEMSTATE_EMBEDDING_URL` or `--embedding-url`). A URL that ends in `/v1` selects an OpenAI-compatible API. |
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
`--embed-model NAME` to skip the prompt. `--embedding-url` and
`--embed-timeout` work the same way.

## One daemon for all agents

The default is one shared daemon per database. The first MCP proxy
starts it detached on `127.0.0.1:8765`, and the daemon writes its
address to `~/.memstate/daemon.addr`. Every later proxy, the recall
hook, the `memstate` CLI and the Python scripts find it through that
file. The daemon outlives the clients. With `MEMSTATE_IDLE_TIMEOUT`, it
exits when nothing has used it for the timeout period:

```bash
export MEMSTATE_IDLE_TIMEOUT=30m    # optional
```

Two variables change the default:

- `MEMSTATE_ADDR=HOST:PORT` names the daemon to use. The proxy attaches
  to it, or starts one there when nothing answers. Use it for a daemon
  on another port or another machine.
- `MEMSTATE_CHILD=1` gives the proxy a private daemon on a random port
  that lives and dies with it. The test suite uses this. A custom
  `MEMSTATE_DB` with no daemon of its own gets a private daemon too.

If port `8765` is held by another program, the proxy warns and starts a
private daemon.

Start, stop, or inspect the daemon manually:

```bash
memstated --addr 127.0.0.1:8765 --idle-timeout 30m   # foreground
memstated stop   --addr 127.0.0.1:8765               # POST /admin/shutdown
memstated status --addr 127.0.0.1:8765               # GET /health
```

Concurrent writers to the same database file are safe. The daemon uses
SQLite in WAL mode with a small connection pool. Each write takes the
write lock when its transaction starts, so other writers wait in a
queue and do not fail. Reads run on the other connections during a
write.

## Python CLI (for skills, hooks, scripts)

`client/skill/scripts/` contains a Python CLI for each tool. The CLI
finds a daemon the same way as the MCP proxy: `MEMSTATE_ADDR` first,
then `~/.memstate/daemon.addr`. The scripts never start a shared
daemon. When no shared daemon answers, or `MEMSTATE_CHILD=1` is set,
each invocation starts its own short-lived daemon and stops it on exit.

```bash
# Explicit keypath
python3 client/skill/scripts/memstate_remember.py \
  --project my_app \
  --keypath task.summary.2026_04_21 \
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

## Browse and edit from the shell (`memstate`)

`make install` links `memstate` to the daemon binary. It reads the SQLite
file directly and sends writes through the shared daemon, so versioning,
embeddings and the user-scope rules apply exactly as they do for an agent.
The project defaults to the repository you are in.

```bash
memstate tree                          # keypath tree for this repo, plus your user scope
memstate get decisions                 # content under one keypath
memstate get todo --raw | less         # content only
memstate history config.port           # every version, newest first
memstate search "why sqlite" --limit 5 # hybrid search via the daemon
memstate set config.port 8080 --category config
memstate edit notes.setup              # $EDITOR on the current content
memstate rm branches.old --recursive   # asks y/N; --yes to skip
memstate tree --user                   # preferences, profile, this host's env and tools
memstate tree --json | jq .user        # raw shapes for scripts
memstate projects                      # every live project
```

`--project ID` reaches another project, `--user` the reserved user scope,
`--all` (search) the whole store. Flags may follow positionals. Without a
shared daemon, reads still work and `search` falls back to FTS. `set`,
`edit` and `rm` need the daemon and tell you how to start one.

## How the pieces fit

```
Claude Code ──stdio──> client/dist/index.js ──HTTP loopback──> server/memstated ──> SQLite
               (MCP)        (thin TS proxy)                       (Go daemon)
```

The TypeScript proxy exists only to speak MCP. Each tool call becomes
one HTTP POST. All logic (keypath versioning, FTS, conflict detection,
tombstones, embeddings) lives in the Go daemon.

The proxy selects one of three modes at startup:

- **Shared** (the default): the proxy reads `daemon.addr` next to the DB and attaches when `/health` answers there. For the default DB, it then tries `127.0.0.1:8765`. When nothing answers, it starts a detached daemon there.
- **Attach** (`MEMSTATE_ADDR` set): the proxy attaches to that address, or starts a detached daemon there. It stops with an error when a program that is not memstate holds the port.
- **Child** (`MEMSTATE_CHILD=1`, a custom `MEMSTATE_DB` with no daemon, or a foreign program on port 8765): the proxy starts a private daemon on `127.0.0.1:0` (an OS-picked port) and passes its own PID with `--owner-pid`. The daemon prints `MEMSTATE_READY addr=127.0.0.1:<port>` on stderr, and the proxy reads it.

In child mode, the proxy sends SIGTERM to the daemon on a clean exit.
After a SIGKILL, the daemon's `kill(owner_pid, 0)` poll notices within
about 2 seconds, and the daemon exits on its own.

## Not done yet

- LLM fallback for keypath extraction when headings are absent. Such content now lands under `<root>.preamble`.
- Time-travel reads. The server now rejects an `at_revision` request field instead of ignoring it.

## License

MIT. The project derives from
[memstate-ai/memstate-mcp](https://github.com/memstate-ai/memstate-mcp).
The storage engine, the lifecycle model, and the wire shape are all
new.
