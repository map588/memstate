# @memstate/mcp

MCP stdio front-end for the memstate daemon. Data stays on your
machine in one SQLite file. No hosted service. No API key.

This package is one half of a two-process architecture:

```
Claude Code ──stdio──> @memstate/mcp ──HTTP loopback──> memstated (Go) ──> ~/.memstate/memstate.db
```

The Go daemon source and binary live under `../server/` in this
repository.

## How spawning works

**Default: one child daemon per proxy.**
The proxy spawns `memstated` as a non-detached child on a random port.
It reads the chosen address from the daemon's stderr banner
(`MEMSTATE_READY addr=127.0.0.1:<port>`). When the proxy exits, it
sends SIGTERM to the child. The daemon also polls the proxy's PID and
stops itself when orphaned. Two Claude Code sessions each get an
independent daemon on a different port.

**Attach mode: shared daemon.**
If `MEMSTATE_ADDR` is set, the proxy attaches to a daemon that already
listens at that address and does not spawn one. Use this when Python
CLI scripts and the MCP tools must share one long-lived daemon:

```bash
# terminal 1: start a shared daemon
../server/memstated --addr 127.0.0.1:8765

# Claude Code and Python scripts both use it
export MEMSTATE_ADDR=127.0.0.1:8765
```

To stop a shared daemon, run `../server/memstated stop`.

## Environment variables

| Variable | Meaning |
|---|---|
| `MEMSTATE_ADDR` | Host:port to attach to. When unset, the proxy spawns a child. |
| `MEMSTATE_BIN` | Path to the `memstated` binary. Default: `../server/memstated`, then PATH lookup. |
| `MEMSTATE_DB` | SQLite file path. Default `~/.memstate/memstate.db`. The daemon reads it, not this proxy. |

## Tool surface

Seven tools match the original memstate shape: `memstate_set`,
`memstate_remember`, `memstate_get`, `memstate_search`,
`memstate_history`, `memstate_delete`, `memstate_delete_project`. See
the daemon's REST routes for exact semantics. `memstate_remember`
without an explicit `keypath` extracts one keypath per markdown `##`
heading. See the SKILL doc for the full contract.

## Build

```bash
npm install
npm run build
node dist/index.js --test   # spawns a daemon, lists the tools
```

## License

MIT.
