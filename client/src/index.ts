#!/usr/bin/env node
/**
 * @memstate/local-mcp
 *
 * MCP (stdio) front-end for a LOCAL memstate daemon.
 *
 * Two modes:
 *  - "child" (default): spawn memstated as a non-detached child, read the
 *    address it prints on stderr, send SIGTERM when we exit. The child
 *    also watches our PID and shuts itself down if we vanish without
 *    clean signalling (e.g. SIGKILL).
 *  - "attach" (MEMSTATE_ADDR set): talk to a daemon someone else started.
 *    We never spawn in this mode; if the addr is down or hostile, we throw.
 *
 * Environment:
 *   MEMSTATE_ADDR    — attach to this host:port instead of spawning a child
 *   MEMSTATE_BIN     — path to memstated (default: sibling build / PATH)
 *   MEMSTATE_LOCAL_URL — full base URL override
 */
import { spawn, ChildProcess } from "child_process";
import * as fs from "fs";
import * as path from "path";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

// eslint-disable-next-line @typescript-eslint/no-require-imports
const { version: VERSION } = require("../package.json") as { version: string };

const ATTACH_ADDR = process.env.MEMSTATE_ADDR ?? "";
const TEST_MODE = process.argv.includes("--test");
const READY_BANNER = "MEMSTATE_READY addr=";

let daemonAddr = ""; // resolved after ensureDaemon()
let baseURL = "";
let managedChild: ChildProcess | null = null;

// ---------- daemon lifecycle ----------

function resolveDaemonBin(): string {
  if (process.env.MEMSTATE_BIN && fs.existsSync(process.env.MEMSTATE_BIN)) {
    return process.env.MEMSTATE_BIN;
  }
  const sibling = path.resolve(__dirname, "..", "..", "server", "memstated");
  if (fs.existsSync(sibling)) return sibling;
  return "memstated"; // fall through to PATH
}

/**
 * Probe an existing daemon. Returns true iff /health reports service=memstate.
 * Non-memstate 200s, timeouts, or connection refusals all return false.
 */
async function probeOurs(addr: string): Promise<boolean> {
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 500);
    const res = await fetch(`http://${addr}/health`, { signal: controller.signal });
    clearTimeout(timer);
    if (!res.ok) return false;
    const json = (await res.json()) as { service?: string };
    return json.service === "memstate";
  } catch {
    return false;
  }
}

/**
 * Attach mode: we did not spawn anything. If the addr is live and ours, good.
 * Anything else is a user error we surface, not something we try to recover.
 */
async function attach(addr: string): Promise<void> {
  if (!(await probeOurs(addr))) {
    throw new Error(
      `MEMSTATE_ADDR=${addr} is not reachable or not a memstate daemon. ` +
        `Start one with \`memstated --addr ${addr}\` or unset MEMSTATE_ADDR to spawn a child.`
    );
  }
  // A running daemon already decided which file backs it; MEMSTATE_DB set by
  // our caller is silently ignored in attach mode, which is a common foot-gun.
  if (process.env.MEMSTATE_DB) {
    process.stderr.write(
      `memstate-local: warning — MEMSTATE_DB is ignored in attach mode ` +
        `(the daemon at ${addr} picked its DB when it started).\n`
    );
  }
  daemonAddr = addr;
  baseURL = process.env.MEMSTATE_LOCAL_URL ?? `http://${addr}/api/v1`;
}

/**
 * Child mode: spawn memstated, read its READY banner from stderr, and wire
 * up cleanup so the child dies with us.
 */
async function spawnChild(): Promise<void> {
  const bin = resolveDaemonBin();
  const logDir = path.join(process.env.HOME ?? "/tmp", ".memstate");
  try {
    fs.mkdirSync(logDir, { recursive: true });
  } catch {
    /* best effort */
  }
  const logPath = path.join(logDir, "memstated.log");
  let logFD: number | null = null;
  try {
    logFD = fs.openSync(logPath, "a");
  } catch {
    logFD = null;
  }

  const child = spawn(
    bin,
    ["--owner-pid", String(process.pid)],
    {
      // NOT detached — we want the child in our process group so a terminal
      // SIGINT reaches it, and so .kill() is authoritative.
      detached: false,
      // stdin ignored, stdout to log (daemon normally silent there), stderr
      // to a pipe so we can read the banner + tee the rest into the log.
      stdio: ["ignore", logFD ?? "ignore", "pipe"],
    }
  );

  if (!child.pid) {
    throw new Error(`memstated: spawn failed (bin=${bin})`);
  }
  managedChild = child;

  // Read stderr line-by-line, copying to the log file as we go. Resolve as
  // soon as we see the READY banner. Reject if the child exits before then.
  const addr = await new Promise<string>((resolve, reject) => {
    let buf = "";
    let resolved = false;
    const onData = (chunk: Buffer) => {
      buf += chunk.toString("utf-8");
      let newline = buf.indexOf("\n");
      while (newline !== -1) {
        const line = buf.slice(0, newline);
        buf = buf.slice(newline + 1);
        if (logFD !== null) {
          try {
            fs.writeSync(logFD, line + "\n");
          } catch {
            /* ignore */
          }
        }
        if (!resolved) {
          const idx = line.indexOf(READY_BANNER);
          if (idx !== -1) {
            const rest = line.slice(idx + READY_BANNER.length).trim();
            const addrToken = rest.split(/\s+/)[0] ?? "";
            if (addrToken) {
              resolved = true;
              resolve(addrToken);
              return;
            }
          }
        }
        newline = buf.indexOf("\n");
      }
    };
    child.stderr?.on("data", onData);
    child.once("exit", (code, signal) => {
      if (!resolved) {
        reject(
          new Error(
            `memstated child exited before becoming ready ` +
              `(code=${code}, signal=${signal}). See ${logPath}.`
          )
        );
      }
    });
    // Reasonable upper bound — Go process startup + SQLite init on any
    // realistic machine completes well under this.
    setTimeout(() => {
      if (!resolved) {
        reject(new Error(`memstated child did not print ready banner within 5s`));
      }
    }, 5000);
  });

  daemonAddr = addr;
  baseURL = process.env.MEMSTATE_LOCAL_URL ?? `http://${addr}/api/v1`;

  // Cleanup wiring. SIGTERM is polite and fast; if the parent is SIGKILLed
  // we rely on the child's --owner-pid watchdog as the safety net.
  const killChild = () => {
    if (managedChild && managedChild.exitCode === null) {
      try {
        managedChild.kill("SIGTERM");
      } catch {
        /* already gone */
      }
    }
  };
  process.once("exit", killChild);
  process.once("SIGINT", () => {
    killChild();
    process.exit(130);
  });
  process.once("SIGTERM", () => {
    killChild();
    process.exit(143);
  });

  // If the child dies on its own (crash), take the proxy down with it so
  // the MCP session reports the failure rather than silently timing out.
  child.once("exit", (code, signal) => {
    if (managedChild === child) {
      process.stderr.write(
        `memstate-local: memstated child exited (code=${code}, signal=${signal})\n`
      );
      process.exit(1);
    }
  });
}

async function ensureDaemon(): Promise<void> {
  if (ATTACH_ADDR) {
    await attach(ATTACH_ADDR);
    return;
  }
  await spawnChild();
}

// ---------- MCP tool surface ----------

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
  handler: (args: Record<string, unknown>) => Promise<unknown>;
}

async function postJSON(route: string, body: unknown): Promise<unknown> {
  const res = await fetch(`${baseURL}${route}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  let parsed: unknown = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    /* keep string */
  }
  if (!res.ok) {
    const msg =
      typeof parsed === "object" && parsed && "error" in parsed
        ? (parsed as { error: string }).error
        : text;
    throw new Error(`HTTP ${res.status}: ${msg}`);
  }
  return parsed;
}

async function getJSON(route: string): Promise<unknown> {
  const res = await fetch(`${baseURL}${route}`);
  const text = await res.text();
  let parsed: unknown = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    /* keep */
  }
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${text}`);
  return parsed;
}

const TOOLS: ToolDef[] = [
  {
    name: "memstate_set",
    description:
      "Store a short value at an explicit keypath. Creates a new version; prior value is returned as 'superseded' so conflicts are visible.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: { type: "string" },
        keypath: { type: "string", description: "dot notation, e.g. config.port" },
        value: { type: "string" },
        source: { type: "string" },
      },
      required: ["project_id", "keypath", "value"],
    },
    handler: (a) =>
      postJSON("/memories/store", {
        project_id: a.project_id,
        keypath: a.keypath,
        content: a.value,
        source: a.source,
      }),
  },
  {
    name: "memstate_remember",
    description:
      "Store a markdown summary. If keypath is provided, writes to it directly. " +
      "If keypath is omitted, the server extracts one keypath per ## heading " +
      "(deeper headings nest as dot segments) and writes each section as its own " +
      "versioned memory. Returns { method, items: [...] }.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: { type: "string" },
        keypath: {
          type: "string",
          description: "optional — omit to extract keypaths from markdown headings",
        },
        content: { type: "string" },
        source: { type: "string" },
        root: {
          type: "string",
          description: "optional prefix applied to every extracted keypath",
        },
      },
      required: ["project_id", "content"],
    },
    handler: (a) => postJSON("/memories/remember", a),
  },
  {
    name: "memstate_get",
    description:
      "Fetch a keypath or browse a subtree. Omit keypath to return the whole project tree.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: { type: "string" },
        keypath: { type: "string" },
        recursive: { type: "boolean" },
        include_content: { type: "boolean", default: true },
      },
      required: ["project_id"],
    },
    handler: async (a) => {
      if (a.keypath) {
        return postJSON("/keypaths", {
          project_id: a.project_id,
          keypath: a.keypath,
          recursive: a.recursive ?? true,
          include_content: a.include_content ?? true,
        });
      }
      return getJSON(`/tree?project_id=${encodeURIComponent(String(a.project_id))}`);
    },
  },
  {
    name: "memstate_search",
    description: "Full-text search (SQLite FTS5) across current, non-tombstoned memories.",
    inputSchema: {
      type: "object",
      properties: {
        query: { type: "string" },
        project_id: { type: "string" },
        limit: { type: "integer", default: 20 },
      },
      required: ["query"],
    },
    handler: (a) => postJSON("/memories/search", a),
  },
  {
    name: "memstate_history",
    description: "Full version chain for a keypath (newest first), including tombstones.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: { type: "string" },
        keypath: { type: "string" },
        memory_id: { type: "integer" },
      },
    },
    handler: (a) => postJSON("/memories/history", a),
  },
  {
    name: "memstate_delete",
    description: "Tombstone a keypath. Set recursive=true to tombstone the whole subtree.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: { type: "string" },
        keypath: { type: "string" },
        recursive: { type: "boolean" },
      },
      required: ["project_id", "keypath"],
    },
    handler: (a) => postJSON("/memories/delete", a),
  },
  {
    name: "memstate_delete_project",
    description:
      "Soft-delete a project. Reads and writes are rejected until a new write revives it.",
    inputSchema: {
      type: "object",
      properties: { project_id: { type: "string" } },
      required: ["project_id"],
    },
    handler: (a) => postJSON("/projects/delete", a),
  },
];

const INSTRUCTIONS = `Local memstate memory store (SQLite-backed).

Tool summary:
- memstate_get: browse before starting a task
- memstate_set: store a short fact at a keypath
- memstate_remember: store a markdown summary at a keypath
- memstate_search: full-text find when keypath is unknown
- memstate_history: see how a keypath changed over time
- memstate_delete / memstate_delete_project: tombstone a keypath or whole project

Keypaths are dot-separated. memstate_remember accepts an explicit keypath OR
extracts one keypath per ## heading from markdown; either way returns
{ method, items: [{keypath, action, stored, superseded?}] }.`;

// ---------- main ----------

async function main(): Promise<void> {
  try {
    await ensureDaemon();
  } catch (err) {
    process.stderr.write(
      `memstate-local: ${err instanceof Error ? err.message : String(err)}\n`
    );
    process.exit(1);
  }

  if (TEST_MODE) {
    const res = await fetch(`http://${daemonAddr}/health`);
    const body = await res.json();
    const mode = ATTACH_ADDR ? "attach" : "child";
    process.stdout.write(
      `✓ daemon reachable at http://${daemonAddr} (${JSON.stringify(body)}) mode=${mode}\n`
    );
    process.stdout.write(`✓ ${TOOLS.length} tools:\n`);
    for (const t of TOOLS) process.stdout.write(`    ${t.name}\n`);
    // In child mode, the --test exit will trigger our cleanup handler and
    // SIGTERM the daemon. In attach mode, we leave it running.
    process.exit(0);
  }

  const server = new Server(
    { name: "memstate-local", version: VERSION },
    { capabilities: { tools: {} }, instructions: INSTRUCTIONS }
  );

  server.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: TOOLS.map((t) => ({
      name: t.name,
      description: t.description,
      inputSchema: t.inputSchema,
    })),
  }));

  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const tool = TOOLS.find((t) => t.name === request.params.name);
    if (!tool) {
      return {
        isError: true,
        content: [{ type: "text", text: `unknown tool: ${request.params.name}` }],
      };
    }
    try {
      const result = await tool.handler(request.params.arguments ?? {});
      return {
        content: [{ type: "text", text: JSON.stringify(result, null, 2) }],
      };
    } catch (err) {
      return {
        isError: true,
        content: [
          { type: "text", text: err instanceof Error ? err.message : String(err) },
        ],
      };
    }
  });

  const stdio = new StdioServerTransport();
  await server.connect(stdio);
  const mode = ATTACH_ADDR ? "attach" : "child";
  process.stderr.write(
    `memstate-local MCP ready (mode=${mode}, daemon @ http://${daemonAddr})\n`
  );
}

async function run(): Promise<void> {
  const command = process.argv[2];
  if (command === "setup") {
    const { main: setupMain } = await import("./setup.js");
    await setupMain();
    return;
  }
  if (command === "init") {
    const { main: initMain } = await import("./init.js");
    await initMain();
    return;
  }
  await main();
}

run().catch((err) => {
  process.stderr.write(
    `Fatal: ${err instanceof Error ? err.message : String(err)}\n`
  );
  process.exit(1);
});
