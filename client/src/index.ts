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
 * Environment (every name may also live in ~/.memstate/config.env, see
 * config.ts; the environment wins over the file):
 *   MEMSTATE_ADDR        attach to this host:port; spawn detached if empty
 *   MEMSTATE_BIN         path to memstated (default: sibling build / PATH)
 *   MEMSTATE_LOCAL_URL   full base URL override
 */
import { spawn, execSync, ChildProcess } from "child_process";
import * as fs from "fs";
import * as net from "net";
import * as os from "os";
import * as path from "path";
import { fileURLToPath } from "url";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import { loadConfigFile } from "./config.js";

// eslint-disable-next-line @typescript-eslint/no-require-imports
const { version: VERSION } = require("../package.json") as { version: string };

// The settings file goes into process.env before anything below reads it,
// and the daemon this proxy spawns inherits the result.
const CONFIG_FILE = loadConfigFile();

const ATTACH_ADDR = process.env.MEMSTATE_ADDR ?? "";
// MEMSTATE_CHILD=1 asks for a private daemon that lives and dies with this
// proxy: the mode that was the default before one shared daemon per
// database became the default. Tests use it to stay hermetic.
const CHILD_MODE = process.env.MEMSTATE_CHILD === "1";
// DEFAULT_ADDR is where the proxy starts the shared daemon of the default
// database when none is published in daemon.addr. It matches defaultAddr
// in server/main.go, so `memstated status|stop|upgrade` find it without
// flags.
const DEFAULT_ADDR = "127.0.0.1:8765";
// daemonMode names how this proxy reached its daemon: "attach" through
// MEMSTATE_ADDR, "shared" through daemon.addr or DEFAULT_ADDR, "child" for
// a private daemon.
type DaemonMode = "attach" | "shared" | "child";
let daemonMode: DaemonMode = "child";

// Embedding options. The proxy owns the daemon it starts, so it is the
// place to decide these. A command-line flag beats the environment; an
// unset option is left to the daemon's own defaults.
interface EmbedOptions {
  model?: string;
  embeddingUrl?: string;
  timeout?: string;
  // legacy names the old option that supplied embeddingUrl, for one
  // deprecation line at startup.
  legacy?: string;
}

const EMBED_FLAGS: Record<string, keyof EmbedOptions> = {
  "--embed-model": "model",
  "--embedding-url": "embeddingUrl",
  "--ollama-url": "embeddingUrl", // deprecated alias
  "--embed-timeout": "timeout",
};

export function parseEmbedOptions(argv: string[], env: NodeJS.ProcessEnv): EmbedOptions {
  const out: EmbedOptions = {};
  if (env.MEMSTATE_EMBED_MODEL) out.model = env.MEMSTATE_EMBED_MODEL;
  if (env.MEMSTATE_EMBEDDING_URL) out.embeddingUrl = env.MEMSTATE_EMBEDDING_URL;
  else if (env.MEMSTATE_OLLAMA_URL) {
    out.embeddingUrl = env.MEMSTATE_OLLAMA_URL;
    out.legacy = "MEMSTATE_OLLAMA_URL";
  }
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
    if (name === "--ollama-url") out.legacy = "--ollama-url";
    else if (key === "embeddingUrl") delete out.legacy;
  }
  return out;
}

// embedDaemonArgs renders the options as memstated flags for spawn.
export function embedDaemonArgs(opts: EmbedOptions): string[] {
  const args: string[] = [];
  if (opts.model) args.push("--embed-model", opts.model);
  if (opts.embeddingUrl) args.push("--embedding-url", opts.embeddingUrl);
  if (opts.timeout) args.push("--embed-timeout", opts.timeout);
  return args;
}

const EMBED_OPTS = parseEmbedOptions(process.argv.slice(2), process.env);
// Only flags given to the proxy itself become daemon flags. Values from
// the environment (config.env included) are NOT turned into flags: the
// daemon inherits the environment and reads config.env on its own, and a
// flag would pin the value so that `memstated restart`, which replays
// flags, could never pick up an edited setting.
const DAEMON_EMBED_ARGS = embedDaemonArgs(parseEmbedOptions(process.argv.slice(2), {}));
if (EMBED_OPTS.legacy) {
  process.stderr.write(
    `memstate: ${EMBED_OPTS.legacy} is deprecated, use ` +
      `${EMBED_OPTS.legacy.startsWith("--") ? "--embedding-url" : "MEMSTATE_EMBEDDING_URL"}\n`
  );
}
const TEST_MODE = process.argv.includes("--test");
const READY_BANNER = "MEMSTATE_READY addr=";

let daemonAddr = ""; // resolved after ensureDaemon()
let baseURL = "";
let managedChild: ChildProcess | null = null;

// deriveProjectId computes the default project_id of a directory: the git
// repo name when dir is inside a repository, else the directory's basename,
// slugged to lowercase snake_case. One stable id per repo stops callers
// from inventing near-duplicate ids. Outside a repository the id is a
// guess, which checkWriteTarget gates; inRepo records which case this is.
function deriveProjectId(dir: string): { id: string; inRepo: boolean } {
  let base = "";
  try {
    const top = execSync("git rev-parse --show-toplevel", {
      cwd: dir,
      stdio: ["ignore", "pipe", "ignore"],
    })
      .toString()
      .trim();
    if (top) base = path.basename(top);
  } catch {
    /* not a git repo */
  }
  const inRepo = base !== "";
  if (!base) base = path.basename(dir);
  return { id: slugName(base), inRepo };
}

// systemDirReason says why dir can name no project: a filesystem root, the
// Windows directory (a desktop app that is not started from a project runs
// its MCP servers from C:\WINDOWS\system32), or a Unix system tree. "" for
// an ordinary directory.
function systemDirReason(dir: string): string {
  const r = path.resolve(dir);
  if (r === path.parse(r).root) return "a filesystem root";
  const win = process.env.SystemRoot || process.env.windir;
  if (win && r.toLowerCase().startsWith(path.resolve(win).toLowerCase())) {
    return "the Windows system directory";
  }
  // Not /tmp or /var: scratch and data trees hold real work (and CI runs
  // these tests from /tmp).
  if (/^\/(usr|bin|sbin|etc|lib|lib64|opt|proc|sys|dev|boot)(\/|$)/.test(r)) {
    return "a system directory";
  }
  return "";
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

// The workspace is the directory this session works in: the process cwd,
// replaced by the MCP client's first workspace root when the client offers
// roots (applyWorkspace runs once more after initialize). Everything that
// scopes a call reads these, and only applyWorkspace assigns them.
let WORKSPACE_DIR = process.cwd();
let DEFAULT_PROJECT = "";
let CWD_IN_REPO = false;
// The home directory names the user, not a project, and a system directory
// names nothing: no write lands in their project, and their names are never
// accepted as a pin or an explicit write target (see checkWriteTarget).
// Reads stay open so old data can migrate.
let CWD_IS_HOME = false;
let CWD_SYSTEM_REASON = "";
const HOME_SLUG = slugName(path.basename(os.homedir()));

function applyWorkspace(dir: string): void {
  WORKSPACE_DIR = dir;
  ({ id: DEFAULT_PROJECT, inRepo: CWD_IN_REPO } = deriveProjectId(dir));
  CWD_IS_HOME = path.resolve(dir) === path.resolve(os.homedir());
  CWD_SYSTEM_REASON = CWD_IN_REPO ? "" : systemDirReason(dir);
}
applyWorkspace(process.cwd());

// noDefaultReason says, for error text, why the workspace has no default
// project for writes; "" when it has one.
function noDefaultReason(): string {
  if (CWD_IS_HOME) return "your home directory";
  if (CWD_SYSTEM_REASON) {
    return `${CWD_SYSTEM_REASON} (${WORKSPACE_DIR}) that the MCP client started this proxy in`;
  }
  return "";
}

function systemNameError(name: string): Error {
  return new Error(
    `"${name}" is the name of ${CWD_SYSTEM_REASON} this proxy was started in, not a ` +
      "project; pass the project you work in as project_id, or pin it with project_name"
  );
}

// SHARED_PROJECT is the daemon's one reserved project for facts that hold
// in every repository: the user's preferences and profile, each host's env
// and tools, and the practices and lessons the agent carries between
// repositories. The daemon rejects writes there outside a short allowlist
// of keypath shapes. slugName never yields a leading "_", so no repo can
// collide with it.
const SHARED_PROJECT = "_shared";

// HOST_SLUG names this machine under host.<slug> in SHARED_PROJECT: the first
// hostname label, slugged.
const HOST_SLUG = slugName(os.hostname().split(".")[0]);

const SCOPE_PROP = {
  type: "string",
  enum: ["project", "shared"],
  default: "project",
  description:
    '"shared" = facts that hold in every repository. Not with project_id.',
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
    "Pin this session to another project (one pin). A new id also needs new_project=true.",
};

const NEW_PROJECT_PROP = {
  type: "boolean",
  default: false,
  description:
    "Allow this call to create a project or revive a deleted one.",
};

let sessionProject = "";

// STRICT_SCOPE_PROP is the one session-level switch on the creation gate.
// Loose mode lifts exactly one rule: a write or a pin to a project that
// does not exist needs no new_project=true. Every other refusal stays. The
// state lives in this process; there is no setting for it, because a
// sticky loose mode is what produced the scratch projects the gate exists
// for.
const STRICT_SCOPE_PROP = {
  type: "boolean",
  default: true,
  description:
    "false: a missing project needs no new_project=true for the rest of this session.",
};

let strictScope = true;

// HOST_TOPIC tags a project memory that holds only on this machine (a build
// path, a toolchain, a port). It is a plain topic, so memstate_search
// filters on it with topics=[HOST_TOPIC], and the daemon knows nothing of
// it. host=true on a write appends it; hostTopics is the one place that does.
const HOST_TOPIC = `host_${HOST_SLUG}`;

const HOST_PROP = {
  type: "boolean",
  default: false,
  description:
    // No host name here: the size of the schemas must not follow it.
    // INSTRUCTIONS names the topic.
    "true: the fact holds only on this machine (a build path, a port); adds its host topic.",
};

// hostTopics returns the topics a write sends: the caller's, plus
// HOST_TOPIC once when host=true. Anything but a boolean is an error, so a
// host name passed by mistake is never dropped in silence.
function hostTopics(a: ToolArgs): string[] | undefined {
  if (a.host !== undefined && typeof a.host !== "boolean") {
    throw new Error(`host must be true or false; the host tag is always "${HOST_TOPIC}"`);
  }
  const topics = Array.isArray(a.topics) ? (a.topics as string[]) : undefined;
  if (a.host !== true) return topics;
  return topics?.includes(HOST_TOPIC) ? topics : [...(topics ?? []), HOST_TOPIC];
}

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

// Reserved ids start with "_". The daemon lists _shared among the projects,
// but for the model it is scope="shared", never a project id.
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
    `project ids that start with "_" are reserved ("${id}"); use scope="shared" for the shared scope`
  );
}
function homeNameError(id: string): Error {
  return new Error(
    `"${id}" is the name of your home directory, which names the user, not a project; ` +
      'pin another project with project_name, or use scope="shared" for facts that hold in every repository'
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
    fs.writeFileSync(tmp, `${WORKSPACE_DIR}\n${project}\n`);
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
  if (a.project_id || a.scope === "shared") {
    throw new Error('project_name cannot be combined with project_id or scope="shared"');
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
  if (CWD_SYSTEM_REASON && name === DEFAULT_PROJECT) throw systemNameError(name);
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
  if (a.new_project !== true && strictScope) {
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
    const why = noDefaultReason();
    if (why) {
      throw new Error(
        `the working directory is ${why}, which has no default ` +
          "project for writes. Pin a project with project_name (an id from " +
          "memstate_get(list_projects=true), or a new id with new_project=true), " +
          'or use scope="shared" for facts that hold in every repository' +
          (CWD_SYSTEM_REASON
            ? ". The recall hook's <memstate-scope cwd_project> names the project of the directory you work in"
            : "")
      );
    }
  } else if (id === HOME_SLUG) {
    throw homeNameError(id);
  } else if (CWD_SYSTEM_REASON && id === DEFAULT_PROJECT) {
    throw systemNameError(id);
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
  if (a.new_project !== true && strictScope) {
    const other = explicit
      ? "Pass an id from memstate_get(list_projects=true)"
      : "Pass project_name=<an id from memstate_get(list_projects=true)> to use another project";
    throw new Error(
      `${where}. ${other}, new_project=true to create "${id}" (this also revives a ` +
        `soft-deleted project), or scope="shared" for facts that hold in every repository`
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
  if (a.scope === "shared") {
    throw new Error('new_project has no effect with scope="shared": the shared scope always exists');
  }
  if (!a.project_id && !sessionProject && CWD_IN_REPO) {
    throw new Error(
      `new_project has no effect here: the project of the git repository you are in ` +
        `("${DEFAULT_PROJECT}") is created without it`
    );
  }
}

// resolveProject picks the project id for a call: the reserved shared
// project for scope "shared", else the explicit id, else the session pin,
// else the cwd project. A write to an explicit id or to the cwd project
// passes checkWriteTarget first; a pinned project was checked at pin time.
// strict_scope is applied before anything else, so the call that sets it
// already runs under it.
async function resolveProject(a: ToolArgs, write = false): Promise<string> {
  if (typeof a.strict_scope === "boolean") {
    strictScope = a.strict_scope;
  } else if (a.strict_scope !== undefined) {
    throw new Error("strict_scope must be true or false");
  }
  if (a.project_name !== undefined) {
    await pinSession(a);
  }
  checkNewProjectFlag(a, write);
  if (a.scope === "shared") {
    if (a.project_id) {
      throw new Error('pass scope="shared" or project_id, not both');
    }
    return SHARED_PROJECT;
  }
  if (a.scope === "user") {
    throw new Error('scope "user" was renamed: use scope="shared"');
  }
  if (a.scope !== undefined && a.scope !== "project") {
    throw new Error(`unknown scope ${JSON.stringify(a.scope)}; use "project" or "shared"`);
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
  const {
    scope: _scope,
    project_name: _name,
    new_project: _new,
    strict_scope: _strict,
    host: _host,
    ...rest
  } = a;
  return rest;
}

// withSessionProject tells the model which project a pinned session uses
// and that loose mode is on.
function withSessionProject(result: unknown): unknown {
  if (!result || typeof result !== "object" || Array.isArray(result)) return result;
  const out = { ...(result as Record<string, unknown>) };
  if (sessionProject) out.session_project = sessionProject;
  if (!strictScope) out.strict_scope = false;
  return out;
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

// ---------- result rendering ----------
//
// A tool result goes into the context of a model, and each later turn reads
// it again. The daemon answers with the rows as it stores them. The functions
// below keep what the model can use and drop what the model sent (source,
// topics) or cannot use (row ids of reads, epoch times). The REST shapes of
// the daemon do not change: the CLI, the scripts and the hooks read them.
// Measured 2026-11-17: the tree of one project was 24 KB as indented JSON and
// 3 KB as an outline.

type Row = Record<string, unknown>;

function omit(row: Row, keys: string[]): Row {
  const out: Row = {};
  for (const [k, v] of Object.entries(row)) {
    if (!keys.includes(k)) out[k] = v;
  }
  return out;
}

// treeOutline renders a keypath tree as text. A line "a, b" lists leaves. A
// line "name: a, b" is a branch with its leaves, and "name*:" is a branch
// that also holds a value. Two spaces of indent are one level. A name holds
// no space, comma, colon or star (the daemon's name rule), so the outline
// reads back to the same keypaths.
function treeOutline(nodes: TreeNode[], depth = 0): string {
  const pad = "  ".repeat(depth);
  const isBranch = (n: TreeNode) => (n.children ?? []).length > 0;
  const lines: string[] = [];
  const leaves = nodes.filter((n) => !isBranch(n)).map((n) => n.name);
  if (leaves.length) lines.push(pad + leaves.join(", "));
  for (const n of nodes.filter(isBranch)) {
    const children = n.children ?? [];
    const names = children.filter((c) => !isBranch(c)).map((c) => c.name);
    lines.push(
      pad + n.name + (n.has_value ? "*" : "") + ":" + (names.length ? " " + names.join(", ") : "")
    );
    const below = treeOutline(children.filter(isBranch), depth + 1);
    if (below) lines.push(below);
  }
  return lines.join("\n");
}

// slimTree replaces the node list of a /tree answer with its outline.
function slimTree(tree: Row): Row {
  return { ...omit(tree, ["domains"]), tree: treeOutline((tree.domains as TreeNode[]) ?? []) };
}

type WriteRef = {
  id?: number;
  project_id?: string;
  keypath?: string;
  version?: number;
  preview?: string;
};
type WriteResult = { keypath?: string; action?: string; stored?: WriteRef; superseded?: WriteRef };

// slimWrite names one write: what happened, where, and the 40-word preview
// of the version that the write replaced. project_id stays, because it tells
// the model which project the write went to.
function slimWrite(out: WriteResult): Row {
  const row: Row = {
    action: out.action,
    project_id: out.stored?.project_id,
    keypath: out.stored?.keypath ?? out.keypath,
    version: out.stored?.version,
    id: out.stored?.id,
  };
  if (out.action === "superseded" && out.superseded?.preview) {
    row.prior = out.superseded.preview;
  }
  return row;
}

// A write result carries a hint when the memory will be hard to find. A
// search finds a short memory with one fact. In a field test of 2026-11-19
// a list of 13 facts at "gotchas" hid each of its facts, and that short
// parent memory came before its own child. The hint costs no space in the
// tool schemas, which are at their size budget.
const HINT_LIST_ITEMS = 5;
const HINT_BYTES = 1500;
const AREAS = ["decisions", "todo", "notes", "gotchas", "questions", "files", "config", "arch"];
// A memory of at most HINT_INDEX_BYTES that names HINT_INDEX_REFS keypaths
// is a list of other memories. A longer memory has text of its own.
const HINT_INDEX_REFS = 3;
const HINT_INDEX_BYTES = 400;
const KEYPATH_REF = new RegExp(`\\b(?:${AREAS.join("|")})\\.[a-z0-9_]+`, "g");

// areaHint is the hint for a memory at the name of an area.
function areaHint(keypaths: string[]): string | undefined {
  const areas = keypaths.filter((k) => AREAS.includes(k));
  if (areas.length === 0) return undefined;
  const names = areas.map((k) => `"${k}"`).join(", ");
  return (
    `${names} is an area, not a fact. Give each fact its keypath: <area>.<topic>, ` +
    'or a "### <topic>" heading below the area heading.'
  );
}

// factHint is the hint for one memory whose text the proxy knows. A task
// summary is long by design and gets no hint.
function factHint(keypath: string, content: string): string | undefined {
  if (keypath.startsWith("task.")) return undefined;
  const area = areaHint([keypath]);
  if (area) return area;
  const items = content.split("\n").filter((line) => /^\s*(?:[-*]|\d+\.)\s+/.test(line)).length;
  if (items >= HINT_LIST_ITEMS) {
    return `${items} list items in one memory. If a search can ask for one item alone, write one memory for each item.`;
  }
  const bytes = Buffer.byteLength(content, "utf8");
  const refs = new Set(content.match(KEYPATH_REF) ?? []).size;
  if (refs >= HINT_INDEX_REFS && bytes <= HINT_INDEX_BYTES) {
    return `This memory names ${refs} other keypaths and holds little else. The keypath tree is the index: a memory that lists other memories comes before them in a search.`;
  }
  if (bytes > HINT_BYTES) {
    return `${bytes} bytes in one memory. A search finds a short memory with one fact: split this one if it holds more than one fact.`;
  }
  return undefined;
}

// withHint adds the hint to a result when there is one.
function withHint(row: Row, hint: string | undefined): Row {
  return hint ? { ...row, hint } : row;
}

// slimRemember is slimWrite for each section. All sections go to one
// project, so the project is named once.
function slimRemember(out: { method?: string; items?: WriteResult[] }): Row {
  const items = out.items ?? [];
  return {
    method: out.method,
    project_id: items[0]?.stored?.project_id,
    items: items.map((item) => omit(slimWrite(item), ["project_id"])),
  };
}

// slimMemories drops the row fields of a keypath read and gives the write
// time as a date, which tells the model how old a fact is.
function slimMemories(out: Row): Row {
  const memories = ((out.memories as Row[]) ?? []).map((m) => {
    const row = omit(m, ["id", "project_id", "parent_id", "created_at"]);
    if (typeof m.created_at === "number") {
      row.updated = new Date(m.created_at * 1000).toISOString().slice(0, 10);
    }
    return row;
  });
  return { ...out, memories };
}

// READ_BUDGET is the content, in bytes, that one subtree read returns in
// full. Above it the daemon answers with a preview per memory, and the model
// reads the keypaths that it needs. One keypath always reads in full. A read
// of all "decisions" of one project returned 29 KB before this (2026-11).
// The skill script memstate_get.py holds the same number.
const READ_BUDGET = 8000;

// daemonTakesReadBudget goes false when the daemon refuses the field: a
// daemon from before the read guard refuses each field it does not know, and
// each keypath read would fail. The proxy then reads without the field until
// it stops; the read is complete, as it was before the guard.
let daemonTakesReadBudget = true;

async function readKeypaths(body: Row): Promise<Row> {
  if (daemonTakesReadBudget) {
    try {
      return (await postJSON("/keypaths", { ...body, max_content_bytes: READ_BUDGET })) as Row;
    } catch (err) {
      if (!String(err).includes('unknown field "max_content_bytes"')) throw err;
      daemonTakesReadBudget = false;
    }
  }
  return (await postJSON("/keypaths", body)) as Row;
}

// slimSearch drops the row fields of each hit. A search of one project
// names the project once; a search of all projects names it on each hit.
function slimSearch(out: Row, project: string): Row {
  const drop = ["id", "source", "version", "created_at"];
  if (project) drop.push("project_id");
  const results = ((out.results as Row[]) ?? []).map((hit) => omit(hit, drop));
  return { ...(project ? { project_id: project } : {}), ...omit(out, ["query"]), results };
}

// isOtherHost reports whether a shared-scope keypath describes another
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

// readAddrFile returns the address a shared daemon published next to the
// database (server/main.go addrFilePath), or "" when there is none. The
// caller probes it: a daemon that died without cleanup leaves a stale file.
function readAddrFile(): string {
  try {
    return fs.readFileSync(path.join(memstateDir(), "daemon.addr"), "utf-8").trim();
  } catch {
    return "";
  }
}

// attach reaches the daemon at addr, or starts one there. sameDB says the
// caller knows the daemon runs on this proxy's database (it came from the
// daemon.addr next to it), so the MEMSTATE_DB warning does not apply.
async function attach(addr: string, sameDB = false): Promise<void> {
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
    if (process.env.MEMSTATE_DB && !sameDB) {
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
          `\`memstated config\` shows both; \`memstated restart\` applies the config.\n`
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

  const child = spawn(bin, ["--addr", addr, ...DAEMON_EMBED_ARGS], {
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

  const child = spawn(bin, ["--owner-pid", String(process.pid), ...DAEMON_EMBED_ARGS], {
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

// ensureDaemon picks the daemon for this proxy. The default is one shared
// daemon per database: a running daemon publishes its address in
// daemon.addr next to the DB, and the proxy attaches to it; with no such
// daemon, the proxy starts one detached on DEFAULT_ADDR. A custom
// MEMSTATE_DB with no daemon of its own gets a private daemon, because the
// default port belongs to the default database. MEMSTATE_ADDR and
// MEMSTATE_CHILD override the default.
async function ensureDaemon(): Promise<void> {
  if (CHILD_MODE) {
    daemonMode = "child";
    await spawnChild();
    return;
  }
  if (ATTACH_ADDR) {
    daemonMode = "attach";
    await attach(ATTACH_ADDR);
    return;
  }
  const published = readAddrFile();
  if (published && (await probeHealth(published)) === "ours") {
    daemonMode = "shared";
    await attach(published, true);
    return;
  }
  if (process.env.MEMSTATE_DB) {
    daemonMode = "child";
    await spawnChild();
    return;
  }
  if ((await probeHealth(DEFAULT_ADDR)) === "alien") {
    process.stderr.write(
      `memstate: warning — ${DEFAULT_ADDR} is held by a non-memstate process; ` +
        `starting a private daemon instead. Set MEMSTATE_ADDR to share one.\n`
    );
    daemonMode = "child";
    await spawnChild();
    return;
  }
  daemonMode = "shared";
  await attach(DEFAULT_ADDR);
}

// ---------- MCP tool surface ----------

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
  handler: (args: Record<string, unknown>) => Promise<unknown>;
}

// reconnecting is the one ensureDaemon call that runs after a request could
// not connect. Requests that fail together wait for the same call, so they
// start one daemon.
let reconnecting: Promise<void> | null = null;

// daemonFetch sends one request to the daemon. A shared daemon can stop
// while this proxy runs: its idle timeout ends it during a long turn, or
// someone restarts it. Before 2026-11-15 the proxy looked for its daemon one
// time, at startup, and each later tool call failed with "fetch failed"
// until a person started the daemon again. Now a request that cannot
// connect makes the proxy choose its daemon again with the startup rule
// (ensureDaemon: attach, or start one where the rule permits it) and send
// the request one more time. A write that the daemon did store before the
// connection broke is not stored twice: equal content is "unchanged".
// A child daemon is not started again here, because the proxy exits when
// its child exits.
async function daemonFetch(route: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetch(`${baseURL}${route}`, init);
  } catch (err) {
    if (daemonMode === "child") throw err;
    process.stderr.write(
      `memstate: no answer from the daemon at ${daemonAddr} (${String(err)}); connecting again\n`
    );
    reconnecting ??= ensureDaemon().finally(() => {
      reconnecting = null;
    });
    await reconnecting;
    process.stderr.write(
      `memstate: connected again (mode=${daemonMode}, daemon @ http://${daemonAddr})\n`
    );
    return fetch(`${baseURL}${route}`, init);
  }
}

async function postJSON(route: string, body: unknown): Promise<unknown> {
  const res = await daemonFetch(route, {
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
  const res = await daemonFetch(route);
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
      "Save ONE fact at ONE keypath. To update a fact, write the SAME keypath.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT for the cwd project. Else only an id that list_projects lists.",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        strict_scope: STRICT_SCOPE_PROP,
        host: HOST_PROP,
        keypath: {
          type: "string",
          description:
            "<area>.<topic>[.<detail>], e.g. \"decisions.auth_provider\". Name it " +
            "after the fact, not after where or when you found it.",
        },
        value: {
          type: "string",
          description:
            "Start with the question or symptom a searcher would type, then the fact, " +
            "then the reason. A gotcha: what went wrong first. Do not list other memories.",
        },
        source: {
          type: "string",
          description:
            "provenance, e.g. \"claude-code session 2026_07_04\" or \"user decision\"",
        },
        category: {
          type: "string",
          description:
            "ONE word from: decision, config, status, note, gotcha, reference, learning",
        },
        topics: {
          type: "array",
          items: { type: "string" },
          description:
            "subject tags, lowercase snake_case, e.g. [\"auth\"]",
        },
      },
      required: ["keypath", "value"],
    },
    handler: async (a) =>
      withHint(
        slimWrite(
          (await postJSON("/memories/store", {
            project_id: await resolveProject(a, true),
            keypath: a.keypath,
            content: a.value,
            source: a.source,
            category: a.category,
            topics: hostTopics(a),
          })) as WriteResult
        ),
        factHint(String(a.keypath ?? ""), String(a.value ?? ""))
      ),
  },
  {
    name: "memstate_remember",
    description:
      "Save a markdown summary at task end. With `keypath`: ALL content is one " +
      "memory there. Without: one memory per heading. A heading is ONE fact, not " +
      "a subject: `## Decisions`, then `### <topic>` per fact → decisions.<topic>.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT for the cwd project. Else only an id that list_projects lists.",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        strict_scope: STRICT_SCOPE_PROP,
        host: HOST_PROP,
        keypath: {
          type: "string",
          description:
            "store ALL content as one memory here, e.g. \"task.summary.2026_07_04\". " +
            "Omit to split by ## headings.",
        },
        content: { type: "string", description: "markdown (or plain text)" },
        source: {
          type: "string",
          description:
            "provenance, e.g. \"claude-code session 2026_07_04\"",
        },
        category: {
          type: "string",
          description:
            "ONE word for EVERY section: decision, config, status, note, gotcha, reference, learning",
        },
        topics: {
          type: "array",
          items: { type: "string" },
          description:
            "subject tags for EVERY section, lowercase snake_case",
        },
        root: {
          type: "string",
          description:
            "split mode only: prefix for the keypaths, e.g. \"notes\" stores `## Auth` at `notes.auth`",
        },
      },
      required: ["content"],
    },
    handler: async (a) => {
      const out = slimRemember(
        (await postJSON("/memories/remember", {
          ...stripProxyFields(a),
          topics: hostTopics(a),
          project_id: await resolveProject(a, true),
        })) as { method?: string; items?: WriteResult[] }
      );
      // With a keypath the proxy knows the text of the one memory. With
      // headings it knows only where each section went.
      const hint = a.keypath
        ? factHint(String(a.keypath), String(a.content ?? ""))
        : areaHint(((out.items as Row[]) ?? []).map((item) => String(item.keypath ?? "")));
      return withHint(out, hint);
    },
  },
  {
    name: "memstate_get",
    description:
      "Read memories. No arguments → the keypath names of this repo as an " +
      "outline in `tree`, no content (`a: b, c` = a.b and a.c; `a*:` = a also " +
      "holds a value; indent nests). `keypath` → the memories at and below it, " +
      "with content; a subtree above 8,000 bytes returns a `preview` per " +
      "memory instead: read the keypaths you need. `list_projects: true` → all " +
      "project ids. Call at task start.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT for the cwd project",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        strict_scope: STRICT_SCOPE_PROP,
        keypath: {
          type: "string",
          description:
            "subtree to read with content, e.g. \"decisions\"",
        },
        list_projects: {
          type: "boolean",
          default: false,
          description: "list every project id",
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
        return slimMemories(
          await readKeypaths({
            project_id: pid,
            keypath: a.keypath,
            recursive: a.recursive ?? true,
            include_content: a.include_content ?? true,
          })
        );
      }
      const tree = (await getJSON(
        `/tree?project_id=${encodeURIComponent(pid)}`
      )) as Record<string, unknown>;
      if (pid === SHARED_PROJECT) {
        return slimTree(tree);
      }
      // The shared scope rides along with every project tree, pruned to
      // this machine, so the agent sees env facts and lessons without
      // knowing to ask. It is a bonus: a soft-deleted _shared project (409)
      // must not hide the project tree.
      let shared: { domains?: TreeNode[] } = {};
      try {
        shared = (await getJSON(
          `/tree?project_id=${encodeURIComponent(SHARED_PROJECT)}`
        )) as { domains?: TreeNode[] };
      } catch {
        /* shared scope unavailable */
      }
      const domains = pruneOtherHosts(shared.domains ?? []);
      return {
        ...slimTree(tree),
        shared: { host: HOST_SLUG, tree: treeOutline(domains), total_memories: countValues(domains) },
      };
    },
  },
  {
    name: "memstate_search",
    description:
      "Find current memories when you do not know the keypath. A hit carries " +
      "`preview`: read the ones that matter with memstate_get(keypath).",
    inputSchema: {
      type: "object",
      properties: {
        query: {
          type: "string",
          description:
            "plain words; no operators needed",
        },
        project_id: {
          type: "string",
          description:
            "OMIT for the cwd project",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        strict_scope: STRICT_SCOPE_PROP,
        all_projects: {
          type: "boolean",
          default: false,
          description: "search every project",
        },
        limit: { type: "integer", default: 10 },
        mode: {
          type: "string",
          enum: ["hybrid", "fts", "semantic"],
          default: "hybrid",
          description:
            "hybrid: any word OR meaning, ranked by both; words only, with `degraded` " +
            "set, when the embedder is down. fts: EVERY word. semantic: meaning only.",
        },
        category: {
          type: "string",
          description:
            "only this category, e.g. \"decision\"",
        },
        topics: {
          type: "array",
          items: { type: "string" },
          description: "only memories with ANY of these topics",
        },
        keypath_prefix: {
          type: "string",
          description:
            "only memories at or below this keypath, e.g. \"decisions\"",
        },
        threshold: {
          type: "number",
          description:
            "similarity floor 0..1 for hybrid and semantic (daemon default)",
        },
        rerank: {
          type: "boolean",
          description: "false: skip the reranker (faster, fused order)",
        },
      },
      required: ["query"],
    },
    handler: async (a) => {
      const { all_projects, ...body } = stripProxyFields(a);
      if (!all_projects) {
        body.project_id = await resolveProject(a);
      }
      let reply: unknown;
      try {
        reply = await postJSON("/memories/search", body);
      } catch (err) {
        // A daemon from before the reranker refuses the field. That daemon
        // has no reranker, so rerank=false holds without the field.
        if (body.rerank !== false || !String(err).includes('unknown field "rerank"')) throw err;
        delete body.rerank;
        reply = await postJSON("/memories/search", body);
      }
      const out = reply as {
        results?: { keypath: string }[];
        total_found?: number;
      };
      if (a.scope === "shared" && Array.isArray(out.results)) {
        // Facts about another machine are noise here.
        out.results = out.results.filter((h) => !isOtherHost(h.keypath));
        out.total_found = out.results.length;
      }
      return slimSearch(out as Row, all_projects ? "" : String(body.project_id));
    },
  },
  {
    name: "memstate_history",
    description:
      "Every stored version of ONE keypath, newest first, with tombstones: " +
      "what a fact was before it changed. Name it by `keypath`, or by `memory_id`.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT for the cwd project",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        strict_scope: STRICT_SCOPE_PROP,
        keypath: { type: "string", description: "required unless memory_id is given" },
        memory_id: {
          type: "integer",
          description:
            "the `id` from a write result or a history row, in place of keypath",
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
      "Tombstone a keypath: reads and searches stop returning it. " +
      "recursive=true also takes every keypath below it. History stays " +
      "(memstate_history), and a new write brings the keypath back.",
    inputSchema: {
      type: "object",
      properties: {
        project_id: {
          type: "string",
          description:
            "OMIT for the cwd project",
        },
        scope: SCOPE_PROP,
        project_name: PROJECT_NAME_PROP,
        new_project: NEW_PROJECT_PROP,
        strict_scope: STRICT_SCOPE_PROP,
        keypath: { type: "string", description: "exact keypath, or subtree root when recursive" },
        recursive: {
          type: "boolean",
          default: false,
          description: "also delete every keypath below",
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
      "Soft-delete a project: reads and searches stop returning it. A later " +
      "write to the project_id revives it with all memories.",
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
  ? "the git repository name"
  : CWD_IS_HOME
    ? "the home directory, which has no default project for writes; see Session project"
    : CWD_SYSTEM_REASON
      ? `${CWD_SYSTEM_REASON} (${WORKSPACE_DIR}), which has no default project for writes: ` +
        "on your first call pass project_name=<the cwd_project of <memstate-scope>>, " +
        "or pass project_id on each call"
      : "the directory name; not a git repository, see Session project";

// INSTRUCTIONS must stay under 3,900 characters, with the most important
// rule first. Claude Code cuts the instructions of an MCP server at 4,096
// characters (older versions at 2,048). Until 2026-11-17 this text was 7,816
// characters, so the host tag and the session project rules never reached
// the model. The regression suite holds the limit. A rule belongs here once;
// a field description names what the field does in one line, and a refused
// call gives the reason.
const INSTRUCTIONS = `memstate: persistent memory across sessions, one store per project.

Use: memstate_get() at task start (keypath names; pass keypath for content). memstate_remember at task end (decisions, reasons, unfinished work, gotchas). memstate_search when you do not know the keypath. memstate_set for one fact.

Never save a denied prompt, for any reason. A denied prompt is a tool call that the user or a permission check denied. Do not save its tool name, command, arguments, or the fact of the denial, under any keypath, in a summary, or as a warning. Never store secrets, tokens or credentials.

Project: OMIT project_id. The cwd project is "${DEFAULT_PROJECT}" (${DEFAULT_ORIGIN}). Pass project_id only to reach another project that memstate_get(list_projects=true) lists. Never invent a variant: "my-app", "myapp" and "my_app_dev" are three projects.

Keypaths (follow exactly; a deviation fragments the store):
- Segments are lowercase snake_case [a-z0-9_], joined by dots. Dates are YYYY_MM_DD: task.summary.2026_07_03, never 2026-07-03.
- Shape <area>.<topic>[.<detail>]. Areas: decisions, todo, notes, gotchas, questions, files, config, arch, task.summary.<date>.
- One keypath = one fact. To update a fact, write the SAME keypath; memstate_history keeps the old versions, and deletes keep history. A write result carries "prior", a 40-word preview of the version it replaced.
- State that changes each session (a version, a count) goes to todo or task.summary, not into a decision or a gotcha.
- memstate_remember without keypath splits by "## " headings: "## Auth" lands at "auth". root="notes" nests the sections.
- A fact true only on an unmerged branch goes under branches.<branch_slug>.<area> ("feature/foo-bar" → branches.feature_foo_bar). After the merge, write the outcomes at the top level and delete the subtree (recursive=true). Decisions, gotchas and architecture always go at the top level.
- task.compact.<date> belongs to the compact hook, which also files sections under the areas above (topic "compact"). Read it; do not write it.

Shared scope (scope="shared", never a project_id): facts that hold in every repository and stay true if this repo is deleted: the user, this machine, how you work across repos. Never a decision, todo or note about one repo's code. memstate_get() already returns it under "shared". The daemon refuses each keypath there except:
  preferences.<topic>, profile.<topic> (never credentials),
  practices.<topic> (a way of working, with the case where it helped),
  lessons.<topic> (a trap anywhere: how to spot it, and the fix),
  host.${HOST_SLUG}.env.<topic>, host.${HOST_SLUG}.tools.<topic>.
This machine's host slug is "${HOST_SLUG}". Host facts need an explicit keypath; headings fit only Preferences, Profile, Practices, Lessons.
Host tag: host=true on a write marks a project fact true only on this machine (a build path, a port); it adds the topic "${HOST_TOPIC}".

Session project: the cwd project is almost always right. On the first prompt the recall hook shows <memstate-scope>: the cwd project and other projects that match the prompt. When the user clearly works on another subject, pass project_name on your first call to pin the session. One pin per session; project_id reaches another project for one call.
- A write never creates a project unless it targets the git repository of this session or carries new_project=true. That holds outside a repository, for an explicit project_id and for a deleted project. A name that resembles an existing project is always refused: use that project. new_project=true is an error where nothing can be created.
- The home directory has no default project for writes: pin a project or use scope="shared". Ids that start with "_" are reserved.
- strict_scope=false lifts the new_project rule for the rest of the session. Use it only when the user wants scratch projects.`;

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
    process.stdout.write(
      `✓ daemon reachable at http://${daemonAddr} (${JSON.stringify(body)}) mode=${daemonMode}\n`
    );
    process.stdout.write(`✓ config file: ${CONFIG_FILE ?? "(none)"}\n`);
    process.stdout.write(`✓ ${TOOLS.length} tools:\n`);
    for (const t of TOOLS) process.stdout.write(`    ${t.name}\n`);
    // In child mode, the --test exit will trigger our cleanup handler and
    // SIGTERM the daemon. A shared or attached daemon keeps running.
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

  // Workspace roots: a client that offers them names the directory the
  // session works in, which beats the process cwd (a desktop app may start
  // this proxy from a system directory). Asked once after initialize; the
  // first tool call waits for the answer, two seconds at most.
  let settleRoots: () => void = () => {};
  const rootsReady = new Promise<void>((resolve) => {
    settleRoots = resolve;
  });
  server.oninitialized = () => {
    void (async () => {
      try {
        if (server.getClientCapabilities()?.roots) {
          const { roots } = await server.listRoots();
          const first = roots.find((r) => r.uri.startsWith("file:"));
          if (first) applyWorkspace(fileURLToPath(first.uri));
        }
      } catch (err) {
        process.stderr.write(`memstate: roots/list failed, keeping the cwd: ${String(err)}\n`);
      } finally {
        process.stderr.write(
          `memstate: workspace ${WORKSPACE_DIR} -> project ${DEFAULT_PROJECT}` +
            (CWD_IN_REPO ? " (git repository)" : noDefaultReason() ? " (no default for writes)" : " (not a repository)") +
            "\n"
        );
        settleRoots();
      }
    })();
  };

  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    await Promise.race([rootsReady, new Promise((r) => setTimeout(r, 2000))]);
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
        content: [{ type: "text", text: JSON.stringify(result) }],
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
  process.stderr.write(
    `memstate MCP ready (mode=${daemonMode}, daemon @ http://${daemonAddr})\n`
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
