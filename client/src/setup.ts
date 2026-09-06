#!/usr/bin/env node
/**
 * memstate setup — write MCP server config into detected AI agents.
 *
 * No API key involved — the MCP proxy spawns its own daemon.
 * Generates a config entry that runs this package via node+dist, pointing at
 * the Go daemon under ../server/memstated (or $MEMSTATE_BIN).
 */

import * as fs from "fs";
import * as path from "path";
import * as os from "os";
import * as readline from "readline";
import { execSync } from "child_process";

/**
 * The config entry written into each agent. Runs the MCP proxy.
 * The proxy resolves the Go binary at runtime via MEMSTATE_BIN or the
 * sibling `server/memstated` path, so this config is machine-agnostic.
 * The embed model, when chosen, travels as a proxy flag so the proxy
 * passes it to every daemon it starts.
 */
function mcpConfigTemplate(entryPath: string, embedModel: string): Record<string, unknown> {
  return {
    command: "node",
    args: proxyArgs(entryPath, embedModel),
  };
}

function proxyArgs(entryPath: string, embedModel: string): string[] {
  return embedModel ? [entryPath, "--embed-model", embedModel] : [entryPath];
}

const DEFAULT_EMBED_MODEL = "nomic-embed-text";

/** listOllamaModels returns the model names a local Ollama serves, or [] when it is unreachable. */
async function listOllamaModels(): Promise<string[]> {
  const base = process.env.MEMSTATE_OLLAMA_URL || "http://127.0.0.1:11434";
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 1500);
    const res = await fetch(`${base}/api/tags`, { signal: controller.signal });
    clearTimeout(timer);
    if (!res.ok) return [];
    const json = (await res.json()) as { models?: { name?: string }[] };
    return (json.models ?? []).map((m) => m.name ?? "").filter(Boolean);
  } catch {
    return [];
  }
}

/**
 * chooseEmbedModel picks the embedding model for the generated config.
 * A --embed-model flag on the setup command wins. Otherwise the user
 * picks from the models Ollama serves (embedding models listed first),
 * or types a name when Ollama is not running. Empty input keeps the
 * daemon default.
 */
async function chooseEmbedModel(rl: Prompt, flagValue: string | undefined): Promise<string> {
  if (flagValue !== undefined) return flagValue;
  const current = process.env.MEMSTATE_EMBED_MODEL || DEFAULT_EMBED_MODEL;
  const models = await listOllamaModels();
  const embedFirst = [
    ...models.filter((m) => m.includes("embed")),
    ...models.filter((m) => !m.includes("embed")),
  ];
  console.log("\nEmbedding model for semantic search (needs Ollama).");
  if (embedFirst.length === 0) {
    console.log("Ollama not reachable; type a model name, or press Enter to keep the default.");
  } else {
    embedFirst.forEach((m, i) => console.log(`  ${i + 1}. ${m}`));
    console.log("Pick a number, type a model name, or press Enter to keep the default.");
  }
  const answer = (await rl.ask(`Model [${current}]: `)).trim();
  if (answer === "") return current === DEFAULT_EMBED_MODEL ? "" : current;
  const n = Number(answer);
  if (Number.isInteger(n) && n >= 1 && n <= embedFirst.length) return embedFirst[n - 1];
  return answer;
}

interface AgentConfig {
  name: string;
  configPaths: string[];
  configKey: string;
  isJsonFile: boolean;
  /** If true, prefer `claude mcp add` over direct JSON edits. */
  useCli?: boolean;
}

function expandHome(p: string): string {
  if (p.startsWith("~/") || p === "~") {
    return path.join(os.homedir(), p.slice(2));
  }
  return p;
}

function claudeCliAvailable(): boolean {
  try {
    execSync("claude --version", { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

function installViaClaudeCli(entryPath: string, embedModel: string): { success: boolean; message: string } {
  try {
    try {
      execSync("claude mcp remove memstate --scope user", { stdio: "ignore" });
    } catch {
      // not present, fine
    }
    const args = proxyArgs(entryPath, embedModel).map((a) => JSON.stringify(a)).join(" ");
    execSync(`claude mcp add --scope user -- memstate node ${args}`, {
      stdio: "pipe",
    });
    return { success: true, message: "✓ Configured via `claude mcp add` (user scope)" };
  } catch (err) {
    return {
      success: false,
      message: `✗ claude CLI failed: ${err instanceof Error ? err.message : String(err)}`,
    };
  }
}

function getAgentConfigs(): AgentConfig[] {
  const home = os.homedir();
  const isWindows = process.platform === "win32";
  const appData = process.env.APPDATA || path.join(home, "AppData", "Roaming");

  return [
    {
      name: "Claude Code",
      configPaths: [path.join(home, ".claude.json")],
      configKey: "mcpServers",
      isJsonFile: true,
      useCli: true,
    },
    {
      name: "Claude Desktop",
      configPaths: isWindows
        ? [path.join(appData, "Claude", "claude_desktop_config.json")]
        : [
            path.join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"),
            path.join(home, ".config", "Claude", "claude_desktop_config.json"),
          ],
      configKey: "mcpServers",
      isJsonFile: true,
    },
    {
      name: "Cursor",
      configPaths: [path.join(home, ".cursor", "mcp.json")],
      configKey: "mcpServers",
      isJsonFile: true,
    },
    {
      name: "Windsurf",
      configPaths: [path.join(home, ".codeium", "windsurf", "mcp_config.json")],
      configKey: "mcpServers",
      isJsonFile: true,
    },
  ];
}

function detectInstalledAgents(agents: AgentConfig[]): AgentConfig[] {
  return agents.filter((agent) =>
    agent.configPaths.some((p) => {
      const expanded = expandHome(p);
      return fs.existsSync(path.dirname(expanded));
    })
  );
}

function readJsonConfig(filePath: string): Record<string, unknown> {
  try {
    if (fs.existsSync(filePath)) {
      return JSON.parse(fs.readFileSync(filePath, "utf-8"));
    }
  } catch {
    // ignore parse errors
  }
  return {};
}

function writeJsonConfig(filePath: string, config: Record<string, unknown>): void {
  const dir = path.dirname(filePath);
  if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(filePath, JSON.stringify(config, null, 2) + "\n", "utf-8");
}

function configureAgent(
  agent: AgentConfig,
  entryPath: string,
  embedModel: string
): { success: boolean; path: string; message: string } {
  if (agent.useCli && claudeCliAvailable()) {
    const result = installViaClaudeCli(entryPath, embedModel);
    return { ...result, path: "~/.claude.json (via claude CLI)" };
  }

  const configPath =
    agent.configPaths.find((p) => fs.existsSync(expandHome(p))) || agent.configPaths[0];
  const expandedPath = expandHome(configPath);

  try {
    const config = readJsonConfig(expandedPath);
    const mcpServers = (config[agent.configKey] as Record<string, unknown>) || {};
    mcpServers["memstate"] = mcpConfigTemplate(entryPath, embedModel);
    config[agent.configKey] = mcpServers;
    writeJsonConfig(expandedPath, config);
    return { success: true, path: expandedPath, message: "✓ Configured" };
  } catch (err) {
    return {
      success: false,
      path: expandedPath,
      message: `✗ Failed: ${err instanceof Error ? err.message : String(err)}`,
    };
  }
}

/**
 * Prompt is a line reader that keeps answers which arrive before a
 * question is asked. With piped stdin (`printf '1\ny\n' | memstate-mcp
 * setup`) readline emits every line at once and then closes, so a plain
 * rl.question would lose the second answer. Lines are queued instead,
 * and a question after end-of-input resolves to "".
 */
class Prompt {
  private queue: string[] = [];
  private waiters: ((line: string) => void)[] = [];
  private closed = false;
  readonly rl: readline.Interface;

  constructor() {
    this.rl = readline.createInterface({ input: process.stdin, output: process.stdout });
    this.rl.on("line", (line) => {
      const w = this.waiters.shift();
      if (w) w(line);
      else this.queue.push(line);
    });
    this.rl.on("close", () => {
      this.closed = true;
      for (const w of this.waiters.splice(0)) w("");
    });
  }

  ask(question: string): Promise<string> {
    process.stdout.write(question);
    const queued = this.queue.shift();
    if (queued !== undefined) {
      process.stdout.write(queued + "\n");
      return Promise.resolve(queued);
    }
    if (this.closed) return Promise.resolve("");
    return new Promise((resolve) => this.waiters.push(resolve));
  }

  close(): void {
    this.rl.close();
  }
}

/** setupFlags reads `--embed-model NAME` (or `--embed-model=NAME`) from the setup argv. */
function setupFlags(argv: string[]): { embedModel?: string } {
  const out: { embedModel?: string } = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--embed-model") out.embedModel = argv[++i] ?? "";
    else if (arg.startsWith("--embed-model=")) out.embedModel = arg.slice("--embed-model=".length);
  }
  return out;
}

export async function main(): Promise<void> {
  const rl = new Prompt();
  const flags = setupFlags(process.argv.slice(2));

  // Resolve where THIS package's compiled entry lives, so the generated
  // agent config doesn't depend on npx or PATH.
  const entryPath = path.resolve(__dirname, "index.js");
  if (!fs.existsSync(entryPath)) {
    console.error(`memstate: expected dist/index.js at ${entryPath} — did you run \`npm run build\`?`);
    rl.close();
    process.exit(1);
  }

  console.log("\nmemstate setup");
  console.log("────────────────────");
  console.log("Writing MCP config that points each agent at this package's dist/index.js.\n");

  const allAgents = getAgentConfigs();
  const detected = detectInstalledAgents(allAgents);

  const embedModel = await chooseEmbedModel(rl, flags.embedModel);

  if (detected.length === 0) {
    console.log("No agents auto-detected. Add an entry like this to your agent's MCP config:\n");
    console.log(JSON.stringify({ "memstate": mcpConfigTemplate(entryPath, embedModel) }, null, 2));
    rl.close();
    return;
  }

  console.log(`Detected agent(s):`);
  detected.forEach((a, i) => console.log(`  ${i + 1}. ${a.name}`));

  const answer = await rl.ask("\nConfigure all detected agents? (Y/n): ");
  if (answer.trim().toLowerCase() === "n") {
    console.log("Skipped.");
    rl.close();
    return;
  }

  for (const agent of detected) {
    process.stdout.write(`  ${agent.name.padEnd(20)} `);
    const result = configureAgent(agent, entryPath, embedModel);
    console.log(result.message);
  }

  console.log(
    "\nDone.\n\n" +
      "Next steps:\n" +
      "  1. Restart your agent so it picks up the new MCP server.\n" +
      "  2. Build the Go daemon: cd ../server && go build -o memstated .\n" +
      "  3. First tool call auto-spawns the daemon; logs at ~/.memstate/memstated.log.\n" +
      (embedModel
        ? `  4. Semantic search uses ${embedModel}; run \`ollama pull ${embedModel}\` if needed.\n\n`
        : "\n") +
      "Convention: use snake_case for project_id and keypath segments\n" +
      "(e.g. `memstate_mcp`, not `memstate-mcp` or `MemstateMCP`).\n"
  );
  rl.close();
}

if (process.argv[1] && (process.argv[1].endsWith("setup.js") || process.argv[1].endsWith("setup.ts"))) {
  main().catch((err) => {
    console.error(`\nSetup failed: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(1);
  });
}
