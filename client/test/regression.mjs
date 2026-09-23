#!/usr/bin/env node
/**
 * Regression test for the full stack:
 *
 *   MCP client (this file) --stdio--> dist/index.js --HTTP--> memstated --> SQLite
 *
 * The test spawns the built proxy in child mode with a temporary database,
 * calls each MCP tool, and checks the response contracts: write actions
 * (created / superseded / unchanged), heading extraction, tree and keypath
 * reads, FTS search and filters, history with tombstones, recursive delete,
 * project soft-delete and revival, error paths, and the `memstated recall`
 * hook against a second daemon in shared mode (found through daemon.addr).
 *
 * The test is hermetic: MEMSTATE_OLLAMA_URL points at a closed port, so
 * embedding is unreachable. Writes must still succeed (fire-and-forget) and
 * semantic search must fail fast — both are asserted below.
 *
 * Run: node client/test/regression.mjs   (after `make build`)
 */
import { execFileSync, spawn } from "child_process";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import { fileURLToPath } from "url";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY = path.resolve(__dirname, "..", "dist", "index.js");
const PROJECT = "regress_test";
const DAEMON =
  process.env.MEMSTATE_BIN ||
  path.resolve(__dirname, "..", "..", "server", "memstated");

let failures = 0;
function check(name, cond, detail = "") {
  if (cond) {
    process.stdout.write(`  ok  ${name}\n`);
  } else {
    failures++;
    process.stdout.write(`FAIL  ${name}${detail ? ` — ${detail}` : ""}\n`);
  }
}

// call invokes one MCP tool and parses the JSON payload the proxy embeds in
// content[0].text. Errors come back as { isError, message }.
async function call(client, name, args) {
  const res = await client.callTool({ name, arguments: args });
  const text = res.content?.[0]?.text ?? "";
  if (res.isError) return { isError: true, message: text };
  return { isError: false, data: JSON.parse(text) };
}

async function main() {
  if (!fs.existsSync(PROXY)) {
    throw new Error(`${PROXY} not found — run \`make build\` first`);
  }
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "memstate-regress-"));
  const watchdog = setTimeout(() => {
    process.stderr.write("regression test timed out after 120s\n");
    process.exit(1);
  }, 120_000);

  const env = { ...process.env };
  delete env.MEMSTATE_ADDR; // force child mode
  env.MEMSTATE_DB = path.join(tmp, "regress.db");
  env.MEMSTATE_NO_UPDATE_CHECK = "1";
  env.MEMSTATE_OLLAMA_URL = "http://127.0.0.1:9"; // closed port: no network

  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [PROXY],
    env,
    stderr: "ignore",
  });
  const client = new Client({ name: "regression-test", version: "0.0.0" });
  await client.connect(transport);

  try {
    // ---- tool surface ---------------------------------------------------
    const { tools } = await client.listTools();
    const names = tools.map((t) => t.name).sort();
    const expected = [
      "memstate_delete",
      "memstate_delete_project",
      "memstate_get",
      "memstate_history",
      "memstate_remember",
      "memstate_search",
      "memstate_set",
    ];
    check(
      "tools/list exposes exactly the 7 memstate tools",
      JSON.stringify(names) === JSON.stringify(expected),
      `got ${names.join(", ")}`
    );

    // ---- versioned writes ------------------------------------------------
    let r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "first value with zanzibar token",
      category: "config",
      topics: ["regress"],
    });
    check("set: first write is created v1",
      !r.isError && r.data.action === "created" && r.data.stored.version === 1,
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "second value",
    });
    check("set: rewrite supersedes and returns prior version",
      !r.isError &&
        r.data.action === "superseded" &&
        r.data.stored.version === 2 &&
        r.data.superseded?.content === "first value with zanzibar token",
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "second value",
    });
    check("set: identical rewrite is unchanged (no new version)",
      !r.isError && r.data.action === "unchanged" && r.data.stored.version === 2,
      JSON.stringify(r));

    // ---- remember: explicit keypath and heading extraction ---------------
    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      keypath: "notes.explicit",
      content: "one explicit memory",
    });
    check("remember: explicit keypath stores one item",
      !r.isError && r.data.method === "explicit" && r.data.items.length === 1 &&
        r.data.items[0].keypath === "notes.explicit",
      JSON.stringify(r));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      content:
        "intro prose before headings\n\n" +
        "## Decisions\nuse sqlite\n\n" +
        "### Auth\ntokens only\n\n" +
        "## TODOs\nship it\n",
      category: "note",
    });
    const kps = r.isError ? [] : r.data.items.map((i) => i.keypath).sort();
    check("remember: heading extraction maps sections and aliases",
      !r.isError &&
        r.data.method === "headings" &&
        JSON.stringify(kps) ===
          JSON.stringify(["decisions", "decisions.auth", "preamble", "todo"]),
      JSON.stringify(kps));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      content: "prose with no headings at all",
    });
    check("remember: headingless prose lands at preamble",
      !r.isError &&
        r.data.items.length === 1 &&
        r.data.items[0].keypath === "preamble",
      JSON.stringify(r));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      content: "\n\n",
    });
    check("remember: whitespace-only content with no keypath is an error",
      r.isError && r.message.includes("headings"),
      JSON.stringify(r));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      keypath: "notes.explicit",
      content: "one explicit memory",
      bogus_field: "daemon must reject unknown fields",
    });
    check("remember: unknown request field is rejected",
      r.isError && r.message.includes("bogus_field"),
      JSON.stringify(r));

    // ---- reads ------------------------------------------------------------
    r = await call(client, "memstate_get", { project_id: PROJECT });
    const domains = r.isError ? [] : r.data.domains.map((d) => d.name).sort();
    check("get: tree lists top-level domains and counts memories",
      !r.isError &&
        JSON.stringify(domains) ===
          JSON.stringify(["config", "decisions", "notes", "preamble", "todo"]) &&
        r.data.total_memories === 6,
      JSON.stringify({ domains, total: r.data?.total_memories }));

    r = await call(client, "memstate_get", {
      project_id: PROJECT,
      keypath: "config",
    });
    check("get: keypath read returns content",
      !r.isError &&
        r.data.memories.length === 1 &&
        r.data.memories[0].content === "second value",
      JSON.stringify(r));

    r = await call(client, "memstate_get", {
      project_id: PROJECT,
      keypath: "config",
      include_content: false,
    });
    check("get: include_content=false strips content",
      !r.isError && r.data.memories[0].content === "",
      JSON.stringify(r));

    // ---- search -----------------------------------------------------------
    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
    });
    check("search: default mode is hybrid and degrades to fts without Ollama",
      !r.isError &&
        r.data.mode === "hybrid" &&
        typeof r.data.degraded === "string" &&
        r.data.results.some((h) => h.keypath === "decisions" &&
          h.sources.length === 1 && h.sources[0] === "fts"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      mode: "fts",
    });
    check("search: fts finds current content",
      !r.isError &&
        r.data.mode === "fts" &&
        r.data.results.some((h) => h.keypath === "decisions"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite zzzznonsense",
      mode: "fts",
    });
    check("search: fts requires every word",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite zzzznonsense",
      mode: "hybrid",
    });
    check("search: hybrid matches on any word",
      !r.isError && r.data.results.some((h) => h.keypath === "decisions"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "zanzibar",
    });
    check("search: superseded content is not indexed",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));
    check("search: zero hits is an empty array, never null",
      !r.isError && Array.isArray(r.data.results) && r.data.results.length === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      category: "note",
    });
    check("search: category filter matches",
      !r.isError && r.data.results.some((h) => h.keypath === "decisions"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      category: "gotcha",
    });
    check("search: category filter excludes other categories",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      keypath_prefix: "notes",
    });
    check("search: keypath_prefix filter excludes other subtrees",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      mode: "semantic",
    });
    check("search: semantic fails fast when embedder is unreachable",
      r.isError && r.message.includes("embed query"),
      JSON.stringify(r));

    // ---- history and delete -----------------------------------------------
    r = await call(client, "memstate_history", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("history: two versions after one supersede",
      !r.isError && r.data.total_versions === 2,
      JSON.stringify(r));

    r = await call(client, "memstate_delete", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("delete: single keypath tombstoned",
      !r.isError && r.data.deleted_count === 1,
      JSON.stringify(r));

    r = await call(client, "memstate_get", { project_id: PROJECT, keypath: "config" });
    check("delete: tombstoned keypath gone from reads",
      !r.isError && Array.isArray(r.data.memories) && r.data.memories.length === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_history", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("delete: history keeps all versions plus tombstone",
      !r.isError && r.data.total_versions === 3,
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "revived value",
    });
    const revived = await call(client, "memstate_get", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("delete: rewrite resurrects a tombstoned keypath",
      !r.isError &&
        !revived.isError &&
        revived.data.memories[0]?.content === "revived value",
      JSON.stringify(revived));

    // ---- recursive delete ---------------------------------------------------
    await call(client, "memstate_set", {
      project_id: PROJECT, keypath: "branches.tmp.todo", value: "a",
    });
    await call(client, "memstate_set", {
      project_id: PROJECT, keypath: "branches.tmp.notes", value: "b",
    });
    r = await call(client, "memstate_delete", {
      project_id: PROJECT,
      keypath: "branches.tmp",
      recursive: true,
    });
    check("delete: recursive removes the whole subtree",
      !r.isError &&
        r.data.deleted_count === 2 &&
        r.data.deleted_keypaths.sort().join(",") ===
          "branches.tmp.notes,branches.tmp.todo",
      JSON.stringify(r));

    // ---- project soft-delete and revival ------------------------------------
    r = await call(client, "memstate_delete_project", { project_id: PROJECT });
    check("delete_project: reports deleted memory count",
      !r.isError && r.data.deleted_count > 0,
      JSON.stringify(r));

    r = await call(client, "memstate_get", { project_id: PROJECT });
    check("delete_project: reads on a soft-deleted project fail",
      r.isError && r.message.includes("soft-deleted"),
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.beta",
      value: "revive project",
    });
    const tree = await call(client, "memstate_get", { project_id: PROJECT });
    check("delete_project: any write revives the project with memories intact",
      !r.isError && !tree.isError && tree.data.total_memories > 1,
      JSON.stringify(tree));

    // ---- error path -----------------------------------------------------------
    const bad = await client.callTool({ name: "memstate_set", arguments: {} });
    check("set: missing required fields is an error",
      bad.isError === true,
      JSON.stringify(bad));

    // ---- recall hook ----------------------------------------------------------
    // A shared-mode daemon on the same DB publishes daemon.addr next to it;
    // `memstated recall` has no MEMSTATE_ADDR here, so it must discover the
    // daemon through that file. The cwd basename slugs to PROJECT.
    await withSharedDaemon(env, async () => {
      const cwd = path.join(tmp, "regress-test");
      fs.mkdirSync(cwd);
      const recall = (session) =>
        execFileSync(DAEMON, ["recall"], {
          env,
          input: JSON.stringify({
            session_id: session,
            cwd,
            prompt: "why did we choose sqlite for the store",
          }),
        }).toString();
      const first = recall("regress_s1");
      check("recall: finds the shared daemon via daemon.addr and injects a hit",
        first.includes(`<memstate-recall project="${PROJECT}">`) &&
          first.includes("### decisions"),
        JSON.stringify(first));
      check("recall: same session does not repeat a keypath",
        recall("regress_s1") === "",
        JSON.stringify(first));
      check("recall: a new session sees the keypath again",
        recall("regress_s2").includes("### decisions"),
        "");
    });
  } finally {
    clearTimeout(watchdog);
    await client.close();
    fs.rmSync(tmp, { recursive: true, force: true });
  }

  if (failures > 0) {
    process.stdout.write(`\n${failures} regression check(s) FAILED\n`);
    process.exit(1);
  }
  process.stdout.write("\nall regression checks passed\n");
}

// withSharedDaemon starts `memstated --addr 127.0.0.1:0` with env, waits
// for its READY banner, runs fn, then stops it and waits for exit.
async function withSharedDaemon(env, fn) {
  const child = spawn(DAEMON, ["--addr", "127.0.0.1:0"], {
    env,
    stdio: ["ignore", "ignore", "pipe"],
  });
  const addr = await new Promise((resolve, reject) => {
    let buf = "";
    const timer = setTimeout(() => reject(new Error("shared daemon: no READY banner")), 5000);
    child.stderr.on("data", (chunk) => {
      buf += chunk.toString();
      const m = /MEMSTATE_READY addr=(\S+)/.exec(buf);
      if (m) {
        clearTimeout(timer);
        child.stderr.removeAllListeners("data");
        child.stderr.resume();
        resolve(m[1]);
      }
    });
    child.on("exit", (code) => reject(new Error(`shared daemon exited early (${code})`)));
  });
  const exited = new Promise((resolve) => child.on("exit", resolve));
  try {
    await fn(addr);
  } finally {
    execFileSync(DAEMON, ["stop", "--addr", addr], { env, stdio: "ignore" });
    await Promise.race([exited, new Promise((r) => setTimeout(r, 5000))]);
    if (child.exitCode === null) child.kill("SIGKILL");
  }
}

main().catch((err) => {
  process.stderr.write(`regression test error: ${err?.stack ?? err}\n`);
  process.exit(1);
});
