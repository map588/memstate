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
import { spawn, execSync, ChildProcess } from "child_process";
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

// deriveProjectId computes the session's default project_id from the git
// repo name (or the working directory's basename outside a repo), slugged
// to lowercase snake_case. MCP clients spawn this proxy in the project
// directory, so this pins one stable id per repo and stops callers from
// inventing near-duplicate ids.
function deriveProjectId(): string {
  let base = "";
  try {
    const top = execSync("git rev-parse --show-toplevel", {
      stdio: ["ignore", "pipe", "ignore"],
    })
      .toString()
      .trim();
    if (top) base = path.basename(top);
  } catch {
    /* not a git repo */
  }
  if (!base) base = path.basename(process.cwd());
  const slug = base
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  return slug || "default";
}

const DEFAULT_PROJECT = deriveProjectId();

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
// READY banner appears (yielding the parsed addr). Rejects on exit or 5s
// timeout. Child mode only — the proxy holds the stderr pipe open for the
// daemon's whole life, so the daemon can always write to it.
function awaitBanner(
  child: ChildProcess,
  logFD: number | null,
  logPath: string
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

// spawnDetached starts a daemon that must outlive this proxy, so its stderr
// goes straight to the log file — NEVER a pipe. A pipe would break once we
// exit, and Go raises SIGPIPE on EPIPE writes to fd 2, i.e. the daemon would
// be killed by its own next log line. The addr is known up front, so
// readiness is a /health poll instead of the banner (which still lands in
// the log for humans).
async function spawnDetached(addr: string): Promise<void> {
  const bin = resolveDaemonBin();
  const { logFD, logPath } = openDaemonLog();

  const child = spawn(bin, ["--addr", addr], {
    detached: true,
    stdio: ["ignore", logFD ?? "ignore", logFD ?? "ignore"],
  });
  if (!child.pid) {
    throw new Error(`memstated: spawn failed (bin=${bin})`);
  }
  let exitCode: number | null | undefined;
  child.once("exit", (code) => {
    exitCode = code;
  });
  child.unref();

  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    if (exitCode !== undefined) {
      // Exit 0 with --addr means the daemon found /health already = us; a
      // racing double-start. Exit 2 means the port holds an alien process.
      if (exitCode === 0) return;
      if (exitCode === 2) {
        throw new Error(
          `port ${addr} is occupied by a non-memstate process; refusing to start.`
        );
      }
      throw new Error(
        `memstated exited before ready (code=${exitCode}). See ${logPath}.`
      );
    }
    if ((await probeHealth(addr)) === "ours") return;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(
    `memstated did not become healthy at ${addr} within 5s. See ${logPath}.`
  );
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
      "Save ONE short fact at ONE keypath (e.g. `config.port` = `8080`). " +
      "To update a fact, write the SAME keypath with the new value — the " +
      "old version is preserved and returned as `superseded`. Do not " +
      "create a new keypath for a new value of the same fact. For " +
      "multi-fact markdown summaries use memstate_remember instead.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT to use this session's default (derived from the repo " +
            "name). Only pass an id that memstate_get(list_projects=true) " +
            "lists — never invent a variant.",
        },
        keypath: {
          type: "string",
          description:
            "dot-joined lowercase snake_case segments, shape " +
            "<area>.<topic>[.<detail>], e.g. \"config.port\" or " +
            "\"decisions.auth_provider\". Dates as YYYY_MM_DD. No kebab-case, " +
            "camelCase, or spaces.",
        },
        value: {
          type: "string",
          description: "the fact itself, plain text — short and self-contained",
        },
        source: {
          type: "string",
          description:
            "provenance of the fact, e.g. \"claude-code session 2026_07_04\" " +
            "or \"user decision\" — shown in history",
        },
        category: {
          type: "string",
          description:
            "kind of memory, ONE lowercase word from: decision, config, " +
            "status, note, gotcha, reference, learning. Filterable in " +
            "memstate_search.",
        },
        topics: {
          type: "array",
          items: { type: "string" },
          description:
            "subject tags, lowercase snake_case, e.g. [\"auth\", " +
            "\"embeddings\"]. Search matches ANY listed topic.",
        },
      },
      required: ["keypath", "value"],
    },
    handler: (a) =>
      postJSON("/memories/store", {
        project_id: a.project_id || DEFAULT_PROJECT,
        keypath: a.keypath,
        content: a.value,
        source: a.source,
        category: a.category,
        topics: a.topics,
      }),
  },
  {
    name: "memstate_remember",
    description:
      "Save a markdown summary at end-of-task (decisions, progress, key " +
      "facts). Two modes: pass `keypath` to store the whole content as ONE " +
      "memory there; omit `keypath` to split the markdown by `##` headings " +
      "into one memory per section. When splitting, each heading becomes a " +
      "top-level snake_case keypath (`## Auth` → keypath `auth`; `###` " +
      "headings nest as a further dot segment; prose before the first " +
      "heading lands at `preamble`). Heading names TODOs, Decisions, Open " +
      "Questions, Files, Notes, Gotchas map to the canonical segments " +
      "todo, decisions, questions, files, notes, gotchas.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT to use this session's default (derived from the repo " +
            "name). Only pass an id that memstate_get(list_projects=true) " +
            "lists — never invent a variant.",
        },
        keypath: {
          type: "string",
          description:
            "store ALL content as one memory at this exact keypath " +
            "(lowercase snake_case segments, dates YYYY_MM_DD, e.g. " +
            "\"task.summary.2026_07_04\"). Omit to split by ## headings " +
            "instead.",
        },
        content: { type: "string", description: "markdown (or plain text)" },
        source: {
          type: "string",
          description:
            "provenance, e.g. \"claude-code session 2026_07_04\" — shown in history",
        },
        category: {
          type: "string",
          description:
            "kind of memory applied to EVERY section written by this call, " +
            "ONE lowercase word from: decision, config, status, note, " +
            "gotcha, reference, learning",
        },
        topics: {
          type: "array",
          items: { type: "string" },
          description:
            "subject tags applied to EVERY section written by this call — " +
            "lowercase snake_case",
        },
        root: {
          type: "string",
          description:
            "heading-split mode only: optional prefix for extracted " +
            "keypaths, e.g. \"notes\" stores `## Auth` at `notes.auth`. " +
            "Default is none — sections are stored at the top level.",
        },
      },
      required: ["content"],
    },
    handler: (a) =>
      postJSON("/memories/remember", {
        ...a,
        project_id: a.project_id || DEFAULT_PROJECT,
      }),
  },
  {
    name: "memstate_get",
    description:
      "Read memories. No arguments → this repo's keypath tree (NAMES " +
      "ONLY, no content); pass `keypath` → the memories at that keypath " +
      "and below, with content; pass `list_projects: true` → all project " +
      "ids in the store. Call at task start to load prior context.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT to use this session's default (derived from the repo name)",
        },
        keypath: {
          type: "string",
          description:
            "subtree to read, e.g. \"decisions\" or \"task.summary\". " +
            "Omit to get the tree of keypath names only — you must pass a " +
            "keypath to read actual content.",
        },
        list_projects: {
          type: "boolean",
          default: false,
          description: "list every project id in the store instead of reading memories",
        },
        recursive: { type: "boolean", default: true },
        include_content: { type: "boolean", default: true },
      },
    },
    handler: async (a) => {
      if (a.list_projects) {
        return getJSON("/projects");
      }
      const pid = String(a.project_id || DEFAULT_PROJECT);
      if (a.keypath) {
        return postJSON("/keypaths", {
          project_id: pid,
          keypath: a.keypath,
          recursive: a.recursive ?? true,
          include_content: a.include_content ?? true,
        });
      }
      return getJSON(`/tree?project_id=${encodeURIComponent(pid)}`);
    },
  },
  {
    name: "memstate_search",
    description:
      "Find current memories when you don't know the exact keypath. Only " +
      "the latest version of each keypath is searched; deleted keypaths " +
      "and deleted projects never match. Searches this repo's project by " +
      "default; pass all_projects=true to search the whole store.",
    inputSchema: {
      type: "object",
      properties: {
        query: {
          type: "string",
          description:
            "plain words — no quoting or boolean operators needed; " +
            "punctuation is handled",
        },
        project_id: {
          type: "string",
          description:
            "OMIT to use this session's default (derived from the repo name)",
        },
        all_projects: {
          type: "boolean",
          default: false,
          description: "search every project in the store instead of just this repo's",
        },
        limit: { type: "integer", default: 20 },
        mode: {
          type: "string",
          enum: ["fts", "semantic"],
          default: "fts",
          description:
            "\"fts\" matches the literal words (stemmed) in content and " +
            "keypath. \"semantic\" matches by MEANING of the content — use " +
            "it when the stored wording is probably different from yours; " +
            "needs Ollama running on the server.",
        },
        category: {
          type: "string",
          description:
            "only memories stored with exactly this category (lowercase " +
            "word, e.g. \"decision\")",
        },
        topics: {
          type: "array",
          items: { type: "string" },
          description: "only memories tagged with AT LEAST ONE of these topics",
        },
        keypath_prefix: {
          type: "string",
          description:
            "only memories at this keypath or below (dot-boundary), e.g. " +
            "\"branches.feature_foo_bar\" to search one branch's state, or " +
            "\"decisions\" to search only decisions",
        },
        threshold: {
          type: "number",
          description:
            "semantic mode only: similarity floor 0..1 (default 0.5). " +
            "Raise to tighten, lower to widen.",
        },
      },
      required: ["query"],
    },
    handler: (a) => {
      const { all_projects, ...body } = a;
      if (!all_projects && !body.project_id) {
        body.project_id = DEFAULT_PROJECT;
      }
      return postJSON("/memories/search", body);
    },
  },
  {
    name: "memstate_history",
    description:
      "Every stored version of ONE keypath, newest first, including " +
      "tombstones. Use to see what a fact was before it changed. Identify " +
      "the keypath either by `keypath` (project_id defaults to this " +
      "repo's), or by the integer `id` of any memory in the chain (from a " +
      "previous response).",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT to use this session's default (derived from the repo name)",
        },
        keypath: { type: "string", description: "required unless memory_id is given" },
        memory_id: {
          type: "integer",
          description:
            "integer `id` from any prior response — alternative to keypath",
        },
      },
    },
    handler: (a) => {
      const body = { ...a };
      if (body.keypath && !body.project_id) {
        body.project_id = DEFAULT_PROJECT;
      }
      return postJSON("/memories/history", body);
    },
  },
  {
    name: "memstate_delete",
    description:
      "Tombstone a keypath so it stops appearing in reads and search. " +
      "With recursive=true, also tombstones every keypath below it (e.g. " +
      "a whole branches.<slug> subtree after a merge). Not destructive: " +
      "all prior versions remain readable via memstate_history, and " +
      "writing the keypath again resurrects it.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT to use this session's default (derived from the repo name)",
        },
        keypath: { type: "string", description: "exact keypath, or subtree root when recursive" },
        recursive: {
          type: "boolean",
          default: false,
          description: "also delete every keypath under this prefix",
        },
      },
      required: ["keypath"],
    },
    handler: (a) =>
      postJSON("/memories/delete", {
        ...a,
        project_id: a.project_id || DEFAULT_PROJECT,
      }),
  },
  {
    name: "memstate_delete_project",
    description:
      "Soft-delete an entire project: reads and searches stop returning " +
      "it. Any later write to the same project_id revives it with all " +
      "memories intact. Nothing is destroyed.",
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

Writes are versioned: writing an existing keypath supersedes the old value
and returns it to you, so you see what changed. Deletes keep history.

Conventions — follow these EXACTLY; every deviation fragments the store:
- project_id: OMIT it. This session's default is "${DEFAULT_PROJECT}"
  (derived from the repo/directory name) and is used whenever project_id
  is absent. Only pass project_id to reach a DIFFERENT project, and then
  only an id that memstate_get(list_projects=true) actually lists — NEVER
  invent a variant: "my-app", "myapp", and "my_app_dev" each create a
  separate, disconnected project.
- keypath segments: lowercase snake_case only ([a-z0-9_]), joined by dots.
  Dates are YYYY_MM_DD inside a segment: "task.summary.2026_07_03" — never
  "2026-07-03" (kebab) and never camelCase or spaces anywhere.
- keypath shape: <area>.<topic> or <area>.<topic>.<detail>. Prefer these
  area segments: decisions, todo, notes, gotchas, questions, files, config,
  arch, task.summary.<date>.
- One keypath = one fact. To update a fact, write the SAME keypath with the
  new value; versioning preserves the old one. Do not create a sibling
  keypath for a new value of the same fact.
- Git branches: keypaths describe the MAIN/default branch unless said
  otherwise. Facts that are only true on an unmerged branch go under
  branches.<branch_slug>.<area>... with the branch name slugged to
  snake_case ("feature/foo-bar" → branches.feature_foo_bar.todo). When the
  branch merges, write the durable outcomes to normal top-level keypaths
  and memstate_delete the branches.<branch_slug> subtree (recursive=true);
  if it is abandoned, just delete the subtree. Branch-independent
  knowledge (decisions taken, gotchas, architecture) always goes at the
  top level, never under branches. Scope a search to one branch with
  memstate_search's keypath_prefix="branches.<branch_slug>".
- Heading extraction (memstate_remember without keypath) writes each
  "## Section" at the top level — "## Auth" lands at keypath "auth",
  exactly like an explicit write. Pass root="notes" (etc.) only when you
  deliberately want sections nested under a prefix.`;

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
