# @memstate/local-mcp

MCP stdio front-end for the **local** memstate daemon. Data stays on your
machine in a single SQLite file. No hosted service, no API key.

This package is one half of a two-process architecture:

```
Claude Code ──stdio──> @memstate/local-mcp ──HTTP loopback──> memstated (Go) ──> ~/.memstate/memstate.db
```

The Go daemon source and binary live under `../server/` in this repo.

## How spawning works

**Default: 1:1 child-owned daemon.**
The MCP proxy spawns `memstated` as a non-detached child on a random port,
reads the chosen address from the daemon's stderr banner
(`MEMSTATE_READY addr=127.0.0.1:<port>`), and sends SIGTERM to the child
when the proxy itself exits. The daemon also polls the proxy's PID and
shuts itself down if orphaned. Two Claude Code sessions each get their own
independent daemon on different ports.

**Attach mode: shared daemon.**
If `MEMSTATE_ADDR` is set, the MCP proxy attaches to a daemon already
listening at that address instead of spawning one. Useful when you want
Python CLI scripts and the MCP tools to share a single long-lived daemon:

```bash
# terminal 1: start a shared daemon
../server/memstated --addr 127.0.0.1:8765

# Claude Code and Python scripts both pick it up
export MEMSTATE_ADDR=127.0.0.1:8765
```

To stop a shared daemon: `../server/memstated stop`.

## Environment variables

| Variable | Meaning |
|---|---|
| `MEMSTATE_ADDR` | Host:port to attach to. Unset = spawn a child. |
| `MEMSTATE_BIN` | Path to the `memstated` binary. Defaults to `../server/memstated` or PATH lookup. |
| `MEMSTATE_DB` | SQLite file path. Default `~/.memstate/memstate.db`. Read by the daemon, not by this proxy. |

## Tool surface

Seven tools matching the original memstate shape: `memstate_set`,
`memstate_remember`, `memstate_get`, `memstate_search`, `memstate_history`,
`memstate_delete`, `memstate_delete_project`. See the daemon's REST routes
for exact semantics. In local mode, `memstate_remember` requires an
explicit `keypath` — no LLM-based auto-extraction.

## Build

```bash
npm install
npm run build
node dist/index.js --test   # verifies daemon comes up, lists tools
```

## License

MIT.
