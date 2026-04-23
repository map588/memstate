#!/usr/bin/env node
/**
 * @memstate/mcp
 *
 * MCP (stdio) front-end for the memstate daemon.
 *
 * Two modes:
 *  - "child" (default): spawn memstated as a non-detached child, read the
 *    address it prints on stderr, send SIGTERM when we exit. The child
 *    also watches our PID and shuts itself down if we vanish without
 *    clean signalling (e.g. SIGKILL).
 *  - "attach" (MEMSTATE_ADDR set): talk to a daemon someone else started,
 *    or lazy-spawn a detached daemon on that addr if nothing is listening.
 *
 * Environment:
 *   MEMSTATE_ADDR        attach to this host:port; spawn detached if empty
 *   MEMSTATE_BIN         path to memstated (default: sibling build / PATH)
 *   MEMSTATE_LOCAL_URL   full base URL override
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

type HealthProbe = "ours" | "alien" | "empty";

async function probeHealth(addr: string): Promise<HealthProbe> {
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 500);
    const res = await fetch(`http://${addr}/health`, { signal: controller.signal });
    clearTimeout(timer);
    if (!res.ok) return "alien";
    try {
      const json = (await res.json()) as { service?: string };
      return json.service === "memstate" ? "ours" : "alien";
    } catch {
      return "alien";
    }
  } catch {
    return "empty";
  }
}

function openDaemonLog(): { logFD: number | null; logPath: string } {
  const logDir = path.join(process.env.HOME ?? "/tmp", ".memstate");
  try {
    fs.mkdirSync(logDir, { recursive: true });
  } catch {}
  const logPath = path.join(logDir, "memstated.log");
  let logFD: number | null = null;
  try {
    logFD = fs.openSync(logPath, "a");
  } catch {
    logFD = null;
  }
  return { logFD, logPath };
}

// awaitBanner tees child.stderr to logFD line-by-line and resolves when the
// READY banner appears (yielding the parsed addr) or via the optional onExit
// shortcut. Rejects on unhandled exit or 5s timeout.
type ExitOutcome = { addr: string } | Error | null;
function awaitBanner(
  child: ChildProcess,
  logFD: number | null,
  logPath: string,
  onExit: (code: number | null) => ExitOutcome = () => null
): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    let buf = "";
    let settled = false;
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        reject(new Error(`memstated did not print ready banner within 5s`));
      }
    }, 5000);
    const settle = (fn: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      fn();
    };
    child.stderr?.on("data", (chunk: Buffer) => {
      buf += chunk.toString("utf-8");
      let nl = buf.indexOf("\n");
      while (nl !== -1) {
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        if (logFD !== null) {
          try {
            fs.writeSync(logFD, line + "\n");
          } catch {}
        }
        const idx = line.indexOf(READY_BANNER);
        if (idx !== -1) {
          const token = line.slice(idx + READY_BANNER.length).trim().split(/\s+/)[0] ?? "";
          if (token) {
            settle(() => resolve(token));
            return;
          }
        }
        nl = buf.indexOf("\n");
      }
    });
    child.once("exit", (code) => {
      const outcome = onExit(code);
      if (outcome && "addr" in outcome) {
        settle(() => resolve(outcome.addr));
        return;
      }
      if (outcome instanceof Error) {
        settle(() => reject(outcome));
        return;
      }
      settle(() =>
        reject(new Error(`memstated exited before ready (code=${code}). See ${logPath}.`))
      );
    });
  });
}

async function attach(addr: string): Promise<void> {
  const probe = await probeHealth(addr);
  if (probe === "alien") {
    throw new Error(
      `MEMSTATE_ADDR=${addr} is occupied by a non-memstate process; refusing to start.`
    );
  }
  if (probe === "empty") {
    await spawnDetached(addr);
  } else if (process.env.MEMSTATE_DB) {
    // A daemon we just spawned inherited our env; warn only when attaching
    // to one we didn't start, since its MEMSTATE_DB was decided earlier.
    process.stderr.write(
      `memstate: warning — MEMSTATE_DB is ignored when attaching to an ` +
        `already-running daemon at ${addr}.\n`
    );
  }
  daemonAddr = addr;
  baseURL = process.env.MEMSTATE_LOCAL_URL ?? `http://${addr}/api/v1`;
}

async function spawnDetached(addr: string): Promise<void> {
  const bin = resolveDaemonBin();
  const { logFD, logPath } = openDaemonLog();

  const child = spawn(bin, ["--addr", addr], {
    detached: true,
    stdio: ["ignore", logFD ?? "ignore", "pipe"],
  });
  if (!child.pid) {
    throw new Error(`memstated: spawn failed (bin=${bin})`);
  }

  await awaitBanner(child, logFD, logPath, (code) => {
    // Exit 0 with --addr means the daemon found /health already = us; racing
    // double-start. Exit 2 means the port is occupied by an alien process.
    if (code === 0) return { addr };
    if (code === 2) {
      return new Error(
        `port ${addr} is occupied by a non-memstate process; refusing to start.`
      );
    }
    return null;
  });

  try {
    child.stderr?.removeAllListeners("data");
    child.stderr?.destroy();
    child.unref();
  } catch {}
}

async function spawnChild(): Promise<void> {
  const bin = resolveDaemonBin();
  const { logFD, logPath } = openDaemonLog();

  const child = spawn(bin, ["--owner-pid", String(process.pid)], {
    // Not detached: keeps the child in our process group so a terminal SIGINT
    // reaches it and .kill() is authoritative.
    detached: false,
    stdio: ["ignore", logFD ?? "ignore", "pipe"],
  });
  if (!child.pid) {
    throw new Error(`memstated: spawn failed (bin=${bin})`);
  }
  managedChild = child;

  const addr = await awaitBanner(child, logFD, logPath);
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
        `memstate: memstated child exited (code=${code}, signal=${signal})\n`
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
      "Save a single short fact at a keypath (e.g. `config.port` = `8080`). " +
      "If a prior value existed, it is returned so you can see what changed.",
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
      "Save a markdown summary. Use at end-of-task for decisions, progress " +
      "notes, and multi-fact summaries. Pass `keypath` to write one memory " +
      "there; omit it to auto-split the markdown by `##` headings into one " +
      "memory per section (deeper headings nest as dot segments).",
    inputSchema: {
      type: "object",
      properties: {
        project_id: { type: "string" },
        keypath: {
          type: "string",
          description: "optional — omit to split by ## headings",
        },
        content: { type: "string" },
        source: { type: "string" },
        root: {
          type: "string",
          description:
            "prefix for extracted keypaths (defaults to project_id; pass `\"\"` to disable)",
        },
      },
      required: ["project_id", "content"],
    },
    handler: (a) => postJSON("/memories/remember", a),
  },
  {
    name: "memstate_get",
    description:
      "Read memories for a project. Omit `keypath` to see the whole project " +
      "tree; pass one to drill into a subtree. Call at task start to load " +
      "prior context.",
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
    description:
      "Find memories when you don't know the exact keypath. " +
      "`mode=\"fts\"` (default) matches keywords; `mode=\"semantic\"` matches " +
      "by meaning and needs semantic search enabled on the server.",
    inputSchema: {
      type: "object",
      properties: {
        query: { type: "string" },
        project_id: { type: "string" },
        limit: { type: "integer", default: 20 },
        mode: {
          type: "string",
          enum: ["fts", "semantic"],
          default: "fts",
        },
        threshold: {
          type: "number",
          description:
            "Semantic mode only. Similarity floor (default 0.5) — raise to tighten, lower to widen.",
        },
      },
      required: ["query"],
    },
    handler: (a) => postJSON("/memories/search", a),
  },
  {
    name: "memstate_history",
    description:
      "All prior versions of a keypath, newest first. Use when you need to see what a fact was before a change.",
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
    description:
      "Remove a keypath (or the whole subtree with `recursive=true`). Prior " +
      "versions remain reachable via `memstate_history`.",
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
      "Remove an entire project. Reads/writes fail until you write again, which restores it.",
    inputSchema: {
      type: "object",
      properties: { project_id: { type: "string" } },
      required: ["project_id"],
    },
    handler: (a) => postJSON("/projects/delete", a),
  },
];

const INSTRUCTIONS = `memstate — persistent memory across sessions, scoped per project.

When to use:
- Task start: memstate_get(project_id=...) to load prior context.
- Task end: memstate_remember to save decisions, progress, and key facts.
- Mid-task: memstate_search when you suspect prior context exists but don't
  know the keypath; memstate_set for single-fact updates (config, status).

Keypaths are dot-separated (e.g. "auth.provider", "task.summary.2026-04-23").
Writes are versioned — a prior value at the same keypath is preserved and
returned to you, so you can see what changed. Deletes keep history.`;

// ---------- main ----------

async function main(): Promise<void> {
  try {
    await ensureDaemon();
  } catch (err) {
    process.stderr.write(
      `memstate: ${err instanceof Error ? err.message : String(err)}\n`
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
    { name: "memstate", version: VERSION },
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
    `memstate MCP ready (mode=${mode}, daemon @ http://${daemonAddr})\n`
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
