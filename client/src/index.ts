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
import * as net from "net";
import * as os from "os";
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

// Embedding options. The proxy owns the daemon it starts, so it is the
// place to decide these. A command-line flag beats the environment; an
// unset option is left to the daemon's own defaults.
interface EmbedOptions {
  model?: string;
  ollamaUrl?: string;
  timeout?: string;
}

const EMBED_FLAGS: Record<string, keyof EmbedOptions> = {
  "--embed-model": "model",
  "--ollama-url": "ollamaUrl",
  "--embed-timeout": "timeout",
};

export function parseEmbedOptions(argv: string[], env: NodeJS.ProcessEnv): EmbedOptions {
  const out: EmbedOptions = {};
  if (env.MEMSTATE_EMBED_MODEL) out.model = env.MEMSTATE_EMBED_MODEL;
  if (env.MEMSTATE_OLLAMA_URL) out.ollamaUrl = env.MEMSTATE_OLLAMA_URL;
  if (env.MEMSTATE_EMBED_TIMEOUT) out.timeout = env.MEMSTATE_EMBED_TIMEOUT;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const eq = arg.indexOf("=");
    const name = eq === -1 ? arg : arg.slice(0, eq);
    const key = EMBED_FLAGS[name];
    if (!key) continue;
    const value = eq === -1 ? argv[++i] : arg.slice(eq + 1);
    if (!value) throw new Error(`${name} needs a value`);
    out[key] = value;
  }
  return out;
}

// embedDaemonArgs renders the options as memstated flags for spawn.
export function embedDaemonArgs(opts: EmbedOptions): string[] {
  const args: string[] = [];
  if (opts.model) args.push("--embed-model", opts.model);
  if (opts.ollamaUrl) args.push("--ollama-url", opts.ollamaUrl);
  if (opts.timeout) args.push("--embed-timeout", opts.timeout);
  return args;
}

const EMBED_OPTS = parseEmbedOptions(process.argv.slice(2), process.env);
const TEST_MODE = process.argv.includes("--test");
const READY_BANNER = "MEMSTATE_READY addr=";

let daemonAddr = ""; // resolved after ensureDaemon()
let baseURL = "";
let managedChild: ChildProcess | null = null;

// deriveProjectId computes the session's default project_id from the git
// repo name (or the working directory's basename outside a repo), slugged
// to lowercase snake_case. MCP clients spawn this proxy in the project
// directory, so this pins one stable id per repo and stops callers from
// inventing near-duplicate ids. Outside a repository the id is a guess,
// which checkDefaultWrite gates; inRepo records which case this is.
function deriveProjectId(): { id: string; inRepo: boolean } {
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
  const inRepo = base !== "";
  if (!base) base = path.basename(process.cwd());
  return { id: slugName(base), inRepo };
}

// slugName is the shared id rule: lowercase, runs of other characters
// become "_", edge underscores trimmed. The Python skill and the Go daemon
// apply the same rule.
function slugName(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  return slug || "default";
}

const { id: DEFAULT_PROJECT, inRepo: CWD_IN_REPO } = deriveProjectId();
// The home directory names the user, not a project: no write lands in its
// project, and its name is never accepted as a pin or an explicit write
// target (see checkWriteTarget). Reads stay open so old data can migrate.
const CWD_IS_HOME = path.resolve(process.cwd()) === path.resolve(os.homedir());
const HOME_SLUG = slugName(path.basename(os.homedir()));

// USER_PROJECT is the daemon's one reserved project for facts about the
// user and the host. The daemon rejects writes there outside a short
// allowlist of keypath shapes. slugName never yields a leading "_", so no
// repo can collide with it.
const USER_PROJECT = "_user";

// HOST_SLUG names this machine under host.<slug> in USER_PROJECT: the first
// hostname label, slugged.
const HOST_SLUG = slugName(os.hostname().split(".")[0]);

const SCOPE_PROP = {
  type: "string",
  enum: ["project", "user"],
  default: "project",
  description:
    '"project" (default) = the cwd project\'s memories. "user" = the reserved ' +
    "user scope: facts about the user or this machine that hold in every " +
    "repo (preferences, profile, host env, host tools). Never both scope " +
    '"user" and project_id.',
};

type ToolArgs = Record<string, unknown>;

// ---------- session project ----------
//
// The cwd project is the default and the strong prior. On the first prompt
// the recall hook shows the model the cwd project and the other projects the
// prompt matches; when the prompt is clearly about another subject, the model
// pins this session to that project once with project_name. The pin lives in
// this process only. Caution rules keep misspelled or invented names out of
// the store: an existing id pins freely, a new id needs new_project=true and
// is refused when it looks like an existing one.

const PROJECT_ID_RE = /^[a-z0-9]+(_[a-z0-9]+)*$/;

const PROJECT_NAME_PROP = {
  type: "string",
  description:
    "Pin this session to a project other than the cwd project, when the " +
    "prompt is clearly about another subject. One pin per session: later " +
    "calls without project_id use it. Prefer an id that " +
    "memstate_get(list_projects=true) lists. A new id also needs " +
    "new_project=true and must not resemble an existing id. Not with " +
    'project_id or scope="user".',
};

const NEW_PROJECT_PROP = {
  type: "boolean",
  default: false,
  description:
    "Allow this call to create a project that does not exist yet, or revive " +
    "a soft-deleted one: with project_name, or on a write with project_id " +
    "or with the cwd project outside a git repository. Refused when the id " +
    "looks like a misspelling of an existing project, and an error where " +
    "nothing can be created.",
};

let sessionProject = "";

// normalizeId folds the spellings that produce near-duplicate projects:
// underscores, digits and a trailing dev/test/tmp/old/new.
function normalizeId(id: string): string {
  return id
    .replace(/_/g, "")
    .replace(/[0-9]/g, "")
    .replace(/(dev|test|tests|tmp|old|new)$/, "");
}

function levenshtein(a: string, b: string): number {
  const prev = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    let last = i;
    for (let j = 1; j <= b.length; j++) {
      const cur = Math.min(
        prev[j] + 1,
        last + 1,
        prev[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1)
      );
      prev[j - 1] = last;
      last = cur;
    }
    prev[b.length] = last;
  }
  return prev[b.length];
}

// nearDuplicates lists existing ids that name looks like: equal after
// normalization, one contains the other (both at least four characters), or
// a small edit distance.
function nearDuplicates(name: string, ids: string[]): string[] {
  const n = normalizeId(name);
  return ids.filter((id) => {
    if (id === name) return false;
    const m = normalizeId(id);
    if (n === m) return true;
    if (n.length >= 4 && m.length >= 4 && (n.includes(m) || m.includes(n))) return true;
    const limit = Math.min(n.length, m.length) < 6 ? 1 : 2;
    return levenshtein(n, m) <= limit;
  });
}

// Reserved ids start with "_". The daemon lists _user among the projects,
// but for the model it is scope="user", never a project id.
function isReservedId(id: string): boolean {
  return id.startsWith("_");
}

async function listProjectIds(): Promise<string[]> {
  const out = (await getJSON("/projects")) as { projects?: { id: string }[] };
  return (out.projects ?? []).map((p) => p.id).filter((id) => !isReservedId(id));
}

// Errors shared by the pin and the write gate.
function reservedIdError(id: string): Error {
  return new Error(
    `project ids that start with "_" are reserved ("${id}"); use scope="user" for the user scope`
  );
}
function homeNameError(id: string): Error {
  return new Error(
    `"${id}" is the name of your home directory, which names the user, not a project; ` +
      'pin another project with project_name, or use scope="user" for facts about this machine'
  );
}

// ---------- pin file for the recall hook ----------
//
// The UserPromptSubmit hook (memstated recall) runs outside this process
// and derives the project from the cwd. A pinned session publishes its pin
// as one file per proxy process, <db dir>/recall/pins/<pid>, holding
// "cwd\nproject\n". The hook follows a file whose PID is alive and prunes
// the rest; this process removes its own file on exit.
function memstateDir(): string {
  const db = process.env.MEMSTATE_DB;
  if (db) {
    const expanded = db.startsWith("~/") ? path.join(os.homedir(), db.slice(2)) : db;
    return path.dirname(path.resolve(expanded));
  }
  return path.join(os.homedir(), ".memstate");
}
const PIN_FILE = path.join(memstateDir(), "recall", "pins", String(process.pid));
let pinFileWritten = false;

function removePinFile(): void {
  try {
    fs.rmSync(PIN_FILE, { force: true });
  } catch {
    /* best effort */
  }
}

function writePinFile(project: string): void {
  try {
    fs.mkdirSync(path.dirname(PIN_FILE), { recursive: true });
    const tmp = `${PIN_FILE}.tmp`;
    fs.writeFileSync(tmp, `${process.cwd()}\n${project}\n`);
    fs.renameSync(tmp, PIN_FILE);
    if (!pinFileWritten) {
      pinFileWritten = true;
      process.on("exit", removePinFile);
    }
  } catch (err) {
    process.stderr.write(
      `memstate: could not publish the session pin for the recall hook: ${String(err)}\n`
    );
  }
}

// pinSession applies project_name: reserved and home names are refused,
// then format, one pin per session, existing id or a deliberate new one
// that resembles nothing.
async function pinSession(a: ToolArgs): Promise<void> {
  const name = a.project_name;
  if (a.project_id || a.scope === "user") {
    throw new Error('project_name cannot be combined with project_id or scope="user"');
  }
  if (typeof name !== "string") {
    throw new Error("project_name must be a string");
  }
  if (isReservedId(name)) throw reservedIdError(name);
  if (!PROJECT_ID_RE.test(name)) {
    throw new Error(
      "project_name must be lowercase snake_case: words of a-z and 0-9 joined " +
        'by single underscores, for example "billing_api"'
    );
  }
  if (name === HOME_SLUG) throw homeNameError(name);
  if (sessionProject && name !== sessionProject) {
    throw new Error(
      `this session is pinned to "${sessionProject}"; pass project_id to reach another project`
    );
  }
  if (name === sessionProject) return;
  const ids = await listProjectIds();
  if (ids.includes(name)) {
    knownProjects.add(name);
    sessionProject = name;
    writePinFile(name);
    return;
  }
  const near = nearDuplicates(name, ids);
  if (near.length > 0) {
    throw new Error(
      `"${name}" looks like existing project ${near.map((id) => `"${id}"`).join(", ")}; ` +
        `use project_name="${near[0]}", or choose a clearly different name`
    );
  }
  if (a.new_project !== true) {
    throw new Error(
      `no project "${name}". Pass an id from memstate_get(list_projects=true), ` +
        "or new_project=true to start a new project"
    );
  }
  sessionProject = name;
  writePinFile(name);
}

// knownProjects caches positive /projects answers: a project seen there
// needs no further check in this process. memstate_delete_project removes
// its id, so a revive needs new_project=true like any other creation.
const knownProjects = new Set<string>();

// checkWriteTarget enforces the one creation rule for writes: a write never
// creates a project unless it targets the git repository this session runs
// in, or the call carries new_project=true and the id resembles no existing
// project. The home directory names the user, so its project is refused
// outright, by cwd and by name. A deliberate near-duplicate needs the
// memstate CLI, which is a human's tool. Refusals create nothing; a
// successful create is not cached, so the next write asks /projects again
// and finds the project.
async function checkWriteTarget(id: string, a: ToolArgs, explicit: boolean): Promise<void> {
  if (!explicit) {
    if (CWD_IN_REPO) return;
    if (CWD_IS_HOME) {
      throw new Error(
        "the working directory is your home directory, which has no default " +
          "project for writes. Pin a project with project_name (an id from " +
          "memstate_get(list_projects=true), or a new id with new_project=true), " +
          'or use scope="user" for facts about this machine'
      );
    }
  } else if (id === HOME_SLUG) {
    throw homeNameError(id);
  }
  if (knownProjects.has(id)) return;
  const ids = await listProjectIds();
  if (ids.includes(id)) {
    knownProjects.add(id);
    return;
  }
  const where = explicit
    ? `project "${id}" does not exist`
    : `the working directory is not a git repository and its project "${id}" does not exist`;
  const near = nearDuplicates(id, ids);
  if (near.length > 0) {
    const fix = explicit
      ? `use project_id="${near[0]}"`
      : `pin the session with project_name="${near[0]}"`;
    throw new Error(
      `${where} but looks like existing project ${near.map((n) => `"${n}"`).join(", ")}; ${fix}. ` +
        `To create "${id}" as a separate project, use the memstate CLI`
    );
  }
  if (a.new_project !== true) {
    const other = explicit
      ? "Pass an id from memstate_get(list_projects=true)"
      : "Pass project_name=<an id from memstate_get(list_projects=true)> to use another project";
    throw new Error(
      `${where}. ${other}, new_project=true to create "${id}" (this also revives a ` +
        `soft-deleted project), or scope="user" for facts about this machine`
    );
  }
}

// checkNewProjectFlag rejects new_project where nothing can be created, so
// an agent that passes it by habit is corrected instead of ignored. With
// project_name the flag belongs to the pin and is judged there.
function checkNewProjectFlag(a: ToolArgs, write: boolean): void {
  if (a.new_project !== true || a.project_name !== undefined) return;
  if (!write) {
    throw new Error(
      "new_project has no effect on this call: only a write, or a pin with " +
        "project_name, creates a project"
    );
  }
  if (a.scope === "user") {
    throw new Error('new_project has no effect with scope="user": the user scope always exists');
  }
  if (!a.project_id && !sessionProject && CWD_IN_REPO) {
    throw new Error(
      `new_project has no effect here: the project of the git repository you are in ` +
        `("${DEFAULT_PROJECT}") is created without it`
    );
  }
}

// resolveProject picks the project id for a call: the reserved user
// project for scope "user", else the explicit id, else the session pin,
// else the cwd project. A write to an explicit id or to the cwd project
// passes checkWriteTarget first; a pinned project was checked at pin time.
async function resolveProject(a: ToolArgs, write = false): Promise<string> {
  if (a.project_name !== undefined) {
    await pinSession(a);
  }
  checkNewProjectFlag(a, write);
  if (a.scope === "user") {
    if (a.project_id) {
      throw new Error('pass scope="user" or project_id, not both');
    }
    return USER_PROJECT;
  }
  if (a.scope !== undefined && a.scope !== "project") {
    throw new Error(`unknown scope ${JSON.stringify(a.scope)}; use "project" or "user"`);
  }
  if (a.project_id) {
    const id = String(a.project_id);
    if (isReservedId(id)) throw reservedIdError(id);
    if (write) await checkWriteTarget(id, a, true);
    return id;
  }
  if (sessionProject) return sessionProject;
  if (write) await checkWriteTarget(DEFAULT_PROJECT, a, false);
  return DEFAULT_PROJECT;
}

// stripProxyFields removes the fields only the proxy understands before a
// body reaches the daemon, which rejects unknown fields.
function stripProxyFields(a: ToolArgs): ToolArgs {
  const { scope: _scope, project_name: _name, new_project: _new, ...rest } = a;
  return rest;
}

// withSessionProject tells the model which project a pinned session uses.
function withSessionProject(result: unknown): unknown {
  if (sessionProject && result && typeof result === "object" && !Array.isArray(result)) {
    return { ...(result as Record<string, unknown>), session_project: sessionProject };
  }
  return result;
}

type TreeNode = { name: string; children?: TreeNode[]; has_value?: boolean };

// pruneOtherHosts keeps only this machine's subtree under `host`.
function pruneOtherHosts(domains: TreeNode[]): TreeNode[] {
  return domains.map((d) =>
    d.name === "host"
      ? { ...d, children: (d.children ?? []).filter((c) => c.name === HOST_SLUG) }
      : d
  );
}

function countValues(nodes: TreeNode[]): number {
  let n = 0;
  for (const node of nodes) {
    if (node.has_value) n++;
    n += countValues(node.children ?? []);
  }
  return n;
}

// isOtherHost reports whether a user-scope keypath describes another
// machine.
function isOtherHost(keypath: string): boolean {
  const seg = keypath.split(".");
  return seg[0] === "host" && seg.length >= 2 && seg[1] !== HOST_SLUG;
}

// ---------- daemon lifecycle ----------

function resolveDaemonBin(): string {
  if (process.env.MEMSTATE_BIN && fs.existsSync(process.env.MEMSTATE_BIN)) {
    return process.env.MEMSTATE_BIN;
  }
  const exe = process.platform === "win32" ? ".exe" : "";
  const sibling = path.resolve(__dirname, "..", "..", "server", "memstated" + exe);
  if (fs.existsSync(sibling)) return sibling;
  return "memstated"; // fall through to PATH
}

type HealthProbe = "ours" | "alien" | "empty";

// daemonEmbedModel is the embed model the last successful /health probe
// reported. Empty when the daemon runs without embeddings.
let daemonEmbedModel = "";

// PROBE_TIMEOUT_MS is how long a /health probe waits. In attach mode the
// daemon can be on another machine: over WireGuard at a 280 ms round trip, the
// first /health on a new connection took 680 ms. A closed local port fails at
// once, so the long wait costs nothing when no daemon listens.
const PROBE_TIMEOUT_MS = 3000;

// PROBE_TRIES is how many /health probes attach mode makes before it decides
// that no daemon listens. A network that drops packets can make one probe
// time out. A closed port fails at once, so the extra probes cost nothing
// when no daemon listens.
const PROBE_TRIES = 3;

// isRemoteAddr reports whether addr has an IP address that no interface of
// this machine has. A daemon cannot listen on such an address. The function
// does not resolve a host name: the daemon does that when it starts.
function isRemoteAddr(addr: string): boolean {
  let host: string;
  try {
    host = new URL(`http://${addr}`).hostname.replace(/^\[(.*)\]$/, "$1");
  } catch {
    return false;
  }
  if (net.isIP(host) === 0) return false;
  if (host.startsWith("127.") || host === "0.0.0.0" || host === "::" || host === "::1") {
    return false;
  }
  return !Object.values(os.networkInterfaces()).some((list) =>
    list?.some((i) => i.address === host)
  );
}

async function probeHealth(addr: string): Promise<HealthProbe> {
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), PROBE_TIMEOUT_MS);
    const res = await fetch(`http://${addr}/health`, { signal: controller.signal });
    clearTimeout(timer);
    if (!res.ok) return "alien";
    try {
      const json = (await res.json()) as { service?: string; embed_model?: string };
      if (json.service !== "memstate") return "alien";
      daemonEmbedModel = json.embed_model ?? "";
      return "ours";
    } catch {
      return "alien";
    }
  } catch {
    return "empty";
  }
}

// openDaemonLog opens the daemon log next to the DB, so a daemon on a test DB
// does not write to the log of the user's daemon.
function openDaemonLog(): { logFD: number | null; logPath: string } {
  const logDir = memstateDir();
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
  let probe = await probeHealth(addr);
  for (let i = 1; i < PROBE_TRIES && probe === "empty"; i++) {
    probe = await probeHealth(addr);
  }
  if (probe === "alien") {
    throw new Error(
      `MEMSTATE_ADDR=${addr} is occupied by a non-memstate process; refusing to start.`
    );
  }
  if (probe === "empty" && isRemoteAddr(addr)) {
    throw new Error(
      `no memstate daemon answered at http://${addr}/health after ${PROBE_TRIES} ` +
        `probes. ${addr} is not an address of this machine, so the proxy does ` +
        `not start a daemon there. Start memstated on that machine, or check ` +
        `the network path to it.`
    );
  }
  if (probe === "empty") {
    await spawnDetached(addr);
  } else {
    // A daemon we just spawned inherited our env; warn only when attaching
    // to one we didn't start, since its MEMSTATE_DB and embed model were
    // decided earlier.
    if (process.env.MEMSTATE_DB) {
      process.stderr.write(
        `memstate: warning — MEMSTATE_DB is ignored when attaching to an ` +
          `already-running daemon at ${addr}.\n`
      );
    }
    const wantModel = EMBED_OPTS.model;
    if (wantModel && daemonEmbedModel && wantModel !== daemonEmbedModel) {
      process.stderr.write(
        `memstate: warning — embed model ${wantModel} is ignored; ` +
          `the daemon at ${addr} embeds with ${daemonEmbedModel}. ` +
          `Restart it with --embed-model ${wantModel} to switch.\n`
      );
    }
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

  const child = spawn(bin, ["--addr", addr, ...embedDaemonArgs(EMBED_OPTS)], {
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

  const child = spawn(bin, ["--owner-pid", String(process.pid), ...embedDaemonArgs(EMBED_OPTS)], {
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
      "old version is preserved and named as `superseded` with a 40-word " +
      "preview (memstate_history has it in full). Do not " +
      "create a new keypath for a new value of the same fact. For " +
      "multi-fact markdown summaries use memstate_remember instead.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT to use the cwd project (this session\'s default). " +
            "Only pass an id that memstate_get(list_projects=true) " +
            "lists — never invent a variant.",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
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
    handler: async (a) =>
      postJSON("/memories/store", {
        project_id: await resolveProject(a, true),
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
            "OMIT to use the cwd project (this session\'s default). " +
            "Only pass an id that memstate_get(list_projects=true) " +
            "lists — never invent a variant.",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
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
    handler: async (a) =>
      postJSON("/memories/remember", {
        ...stripProxyFields(a),
        project_id: await resolveProject(a, true),
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
            "OMIT to use the cwd project (this session's default)",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
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
        const out = (await getJSON("/projects")) as { projects?: { id: string }[] };
        return { ...out, projects: (out.projects ?? []).filter((p) => !isReservedId(p.id)) };
      }
      const pid = await resolveProject(a);
      if (a.keypath) {
        return postJSON("/keypaths", {
          project_id: pid,
          keypath: a.keypath,
          recursive: a.recursive ?? true,
          include_content: a.include_content ?? true,
        });
      }
      const tree = (await getJSON(
        `/tree?project_id=${encodeURIComponent(pid)}`
      )) as Record<string, unknown>;
      if (pid === USER_PROJECT) {
        return tree;
      }
      // The user scope rides along with every project tree, pruned to this
      // machine, so the agent sees env facts without knowing to ask. It is
      // a bonus: a soft-deleted _user project (409) must not hide the
      // project tree.
      let user: { domains?: TreeNode[] } = {};
      try {
        user = (await getJSON(
          `/tree?project_id=${encodeURIComponent(USER_PROJECT)}`
        )) as { domains?: TreeNode[] };
      } catch {
        /* user scope unavailable */
      }
      const domains = pruneOtherHosts(user.domains ?? []);
      return {
        ...tree,
        user: { host: HOST_SLUG, domains, total_memories: countValues(domains) },
      };
    },
  },
  {
    name: "memstate_search",
    description:
      "Find current memories when you don't know the exact keypath. Only " +
      "the latest version of each keypath is searched; deleted keypaths " +
      "and deleted projects never match. Searches this repo's project by " +
      "default; pass all_projects=true to search the whole store. Each hit " +
      "carries `preview` (its first 40 words), not the content: pick the " +
      "keypaths that matter and read them with memstate_get(keypath).",
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
            "OMIT to use the cwd project (this session's default)",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        all_projects: {
          type: "boolean",
          default: false,
          description: "search every project in the store instead of just this repo's",
        },
        limit: { type: "integer", default: 10 },
        mode: {
          type: "string",
          enum: ["hybrid", "fts", "semantic"],
          default: "hybrid",
          description:
            "\"hybrid\" (default) merges a literal-word match (any word " +
            "may hit) with a match by MEANING and ranks by both; when the " +
            "embedder is unavailable it returns the literal matches alone " +
            "and sets `degraded` to the reason. \"fts\" requires EVERY " +
            "word to match. \"semantic\" matches by meaning only and " +
            "fails when Ollama is down.",
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
            "semantic and hybrid modes: similarity floor 0..1 (default " +
            "0.5). Raise to tighten, lower to widen.",
        },
      },
      required: ["query"],
    },
    handler: async (a) => {
      const { all_projects, ...body } = stripProxyFields(a);
      if (!all_projects) {
        body.project_id = await resolveProject(a);
      }
      const out = (await postJSON("/memories/search", body)) as {
        results?: { keypath: string }[];
        total_found?: number;
      };
      if (a.scope === "user" && Array.isArray(out.results)) {
        // Facts about another machine are noise here.
        out.results = out.results.filter((h) => !isOtherHost(h.keypath));
        out.total_found = out.results.length;
      }
      return out;
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
            "OMIT to use the cwd project (this session's default)",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        keypath: { type: "string", description: "required unless memory_id is given" },
        memory_id: {
          type: "integer",
          description:
            "integer `id` from any prior response — alternative to keypath",
        },
      },
    },
    handler: async (a) => {
      const body = stripProxyFields(a);
      if (body.keypath) {
        body.project_id = await resolveProject(a);
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
            "OMIT to use the cwd project (this session's default)",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        keypath: { type: "string", description: "exact keypath, or subtree root when recursive" },
        recursive: {
          type: "boolean",
          default: false,
          description: "also delete every keypath under this prefix",
        },
      },
      required: ["keypath"],
    },
    handler: async (a) =>
      postJSON("/memories/delete", {
        ...stripProxyFields(a),
        project_id: await resolveProject(a),
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
    handler: (a) => {
      knownProjects.delete(String(a.project_id));
      return postJSON("/projects/delete", a);
    },
  },
];

// DEFAULT_ORIGIN tells the model where its default project id came from
// and, outside a repository, that writes to it are gated.
const DEFAULT_ORIGIN = CWD_IN_REPO
  ? "derived from the git repository name"
  : CWD_IS_HOME
    ? "derived from the home directory, which has no default project for writes; see Session project"
    : "derived from the directory name, which is not a git repository; see Session project";

const INSTRUCTIONS = `memstate — persistent memory across sessions, scoped per project.

When to use:
- Task start: memstate_get(project_id=...) to load prior context.
- Task end: memstate_remember to save decisions, progress, and key facts.
- Mid-task: memstate_search when you suspect prior context exists but don't
  know the keypath; memstate_set for single-fact updates (config, status).

Never save a denied prompt, for any reason. A denied prompt is a tool call
that the user or a permission check denied. Do not save its tool name, its
command, its arguments, or the fact of the denial. Do not save it under any
keypath or category, in a task summary, or as a warning for a later session.

Writes are versioned: writing an existing keypath supersedes the old value.
The response names the new and the prior version (keypath, version, and a
40-word preview of the prior content). It never echoes the content you
sent; memstate_history returns prior versions in full. Deletes keep history.

Conventions — follow these EXACTLY; every deviation fragments the store:
- project_id: OMIT it. The cwd project is "${DEFAULT_PROJECT}"
  (${DEFAULT_ORIGIN}); it is used whenever project_id
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
  deliberately want sections nested under a prefix.

User scope — facts that are not about this project:
- Pass scope="user" (never a project_id) to reach the reserved user scope.
  memstate_get() with no arguments already returns it under "user", pruned
  to this machine, so read it there first.
- A fact belongs in the user scope only when ALL three hold: (1) it stays
  true if this repo is deleted, it is not about any code; (2) it is true
  in every repo, for this user or for this machine; (3) it describes the
  user or the host, not work. Never a decision, todo, task summary, note,
  or gotcha about code — those stay in the project.
- The daemon enforces a keypath allowlist there and rejects everything
  else with 400. Allowed shapes:
    preferences.<topic>                 stated global working preferences
    profile.<topic>                     who the user is; never credentials
    host.${HOST_SLUG}.env.<topic>       OS, shell, paths, runtimes, ports
    host.${HOST_SLUG}.tools.<topic>     how an installed tool is configured
  This machine's host slug is "${HOST_SLUG}". Host facts need an explicit
  keypath; heading extraction fits only "## Preferences" and "## Profile".
- Never store secrets, tokens, or credentials in any scope. The denied-prompt
  rule applies to the user scope too.

Session project — when the prompt is not about this directory:
- The cwd project is the default and almost always right. On the first
  prompt the recall hook shows a <memstate-scope> block: the cwd project and
  the other projects the prompt matches. Judge from it. When the user
  clearly works on another subject (for example "set up my nginx config"
  from the home directory), pin this session with project_name on your
  first memstate call. Later calls without project_id use that project.
  Results then carry session_project.
- Prefer an id that memstate_get(list_projects=true) lists. A new id needs
  new_project=true as well, and is refused when it looks like an existing
  id ("regress_tests" vs "regress_test", "my_app_dev" vs "my_app"). Never
  invent a variant of an existing name.
- One pin per session; a different project_name later is an error. Pass
  project_id to reach another project for one call.
- One rule for creating projects: a write (memstate_set, memstate_remember)
  never creates a project unless it targets the git repository this
  session runs in, or the call carries new_project=true. This holds for
  the cwd project outside a git repository, for an explicit project_id,
  and for a soft-deleted project (new_project=true revives it). A name
  that resembles an existing project is always refused: use the existing
  project. Reads are never gated, and a refusal creates nothing.
- The home directory has no default project for writes at all, and its
  name is never a project: pin another project with project_name, or use
  scope="user" for facts about this machine.
- Ids that start with "_" are reserved. The user scope is scope="user",
  never a project_id or project_name; list_projects does not show it.
- new_project=true is an error where nothing can be created: on a read
  without project_name, with scope="user", or for the git repository's
  own project. Pass it only on the call that creates or revives a project.`;

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
      const result = withSessionProject(await tool.handler(request.params.arguments ?? {}));
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
