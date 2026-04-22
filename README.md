# memstate-local

Local-only fork of [memstate-mcp](https://github.com/memstate-ai/memstate-mcp).
Everything runs on your machine — no hosted service, no API key.

```
Claude Code ──stdio──> client/   (TS MCP proxy) ──HTTP loopback──> server/   (Go daemon) ──> SQLite
                                                                        ▲
Python CLI scripts ──── same HTTP ─────────────────────────────────────┘
```

## Layout

| Dir | What it is |
|---|---|
| `server/` | Go daemon. Owns the SQLite file, exposes a REST API, no MCP knowledge. |
| `client/` | TypeScript MCP stdio proxy + Python CLI scripts (for the `memstate-local` skill). Both translate to HTTP calls on the daemon. |

## Build

```bash
# Go daemon
cd server && go build -o memstated .

# MCP proxy
cd ../client && npm install && npm run build
```

Verify:
```bash
node client/dist/index.js --test
```

Expected output (with no daemon running): proxy spawns a child on a random
port, lists the 7 tools, then exits — taking the child with it.

## Process model

The proxy and the daemon have two lifetimes depending on how they're started.

| Mode | How | Daemon lifetime | Use when |
|---|---|---|---|
| **Child** (default) | MCP proxy or Python script spawns `memstated` as a non-detached child with `--owner-pid=$$` | Dies with the spawner (via SIGTERM on clean exit, owner-pid watchdog on SIGKILL) | Single Claude Code session, or one-off Python calls. Nothing leftover. |
| **Attach** | `MEMSTATE_ADDR=host:port` set; the daemon was started separately (`memstated --addr 127.0.0.1:8765`) | Survives proxy exits; stopped manually | Sharing one daemon across multiple Claude Code sessions and/or Python scripts. |

Multiple Claude Code sessions with no `MEMSTATE_ADDR` each get their own
daemon on their own random port. They all read and write the same SQLite
file — SQLite WAL mode handles concurrency.

### Stopping a shared daemon

```bash
server/memstated stop --addr 127.0.0.1:8765     # POST /admin/shutdown
server/memstated status --addr 127.0.0.1:8765   # GET /health
```

### Guards against runaway spawning

- Go daemon on `--addr` EADDRINUSE: probes `/health`. If the occupant is one
  of ours, it exits 0. If it's alien, it exits 2 loudly — the proxy does not
  try to respawn.
- Go daemon polls `--owner-pid` via `kill(pid, 0)` every 2s; on ESRCH it
  shuts down. Orphans can't linger.
- Proxy has no respawn logic at all — if the child crashes, the proxy dies
  with it and the MCP session reports the failure.

## Env vars

| Var | Effect |
|---|---|
| `MEMSTATE_ADDR` | `host:port` to attach to instead of spawning. Disables child mode. |
| `MEMSTATE_BIN` | Path to the `memstated` binary. Defaults to `server/memstated` (sibling), then PATH. |
| `MEMSTATE_DB` | SQLite file. Default `~/.memstate/memstate.db`. `~/` is expanded. **Ignored in attach mode** — the already-running daemon picked its DB when it started; the proxy warns if you set both. |
| `MEMSTATE_LOCAL_URL` | Full base URL override, e.g. `http://127.0.0.1:9000/api/v1`. Takes precedence over `MEMSTATE_ADDR`. |

Claude Code MCP configs pass env explicitly. If you want a per-project DB,
put it in the `env:` block when registering the server:

```json
{
  "mcpServers": {
    "memstate-local": {
      "command": "node",
      "args": ["/abs/path/to/client/dist/index.js"],
      "env": {
        "MEMSTATE_DB": "/abs/path/to/project.db"
      }
    }
  }
}
```

## Test suite

```bash
cd server && go test ./...           # 22 tests: store, HTTP, lifecycle
cd ../client && npx tsc              # TS build
```

## Not implemented (yet)

- Semantic search via Ollama (`embeddings` table exists, logic doesn't).
- `--category`, `--topics`, and `--at-revision` are accepted by the REST
  layer for compat with the hosted-API Python scripts, but are ignored.

## License

MIT, inherited from upstream.
