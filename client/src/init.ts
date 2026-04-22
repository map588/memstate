#!/usr/bin/env node
/**
 * memstate-local init — write agent instruction files for local memstate use.
 *
 * Unlike the hosted version, this does not fetch anything from the network.
 * The instructions are bundled and describe the local tool surface.
 */

import * as fs from "fs";
import * as path from "path";

const INSTRUCTIONS = `# Memstate (local) — memory usage

This project uses a **local** memstated daemon for persistent, versioned
memory across sessions. Data lives on this machine only (SQLite).

## Required at start of every task

Load existing context before acting:
\`\`\`
memstate_get(project_id="<your_project>")
\`\`\`

## Required at end of every task

Save a summary of what you did:
\`\`\`
memstate_remember(
  project_id="<your_project>",
  keypath="task.summary.<YYYY-MM-DD>",
  content="## Task Summary\\n- What was done\\n- Key decisions\\n- Files touched",
  source="agent"
)
\`\`\`

Keypaths are **explicit** in local mode — no auto-extraction. Pick a
descriptive dot-notation path.

## Tool reference

| Tool | Purpose |
|------|---------|
| memstate_get | Browse project tree or fetch one keypath |
| memstate_remember | Store a markdown summary at a keypath |
| memstate_set | Store a short value at a keypath |
| memstate_search | Full-text find (FTS5) when keypath is unknown |
| memstate_history | See how a keypath changed over time |
| memstate_delete | Tombstone a keypath (history preserved) |
| memstate_delete_project | Soft-delete an entire project |

## Project naming

Short snake_case that matches your repo or topic (e.g. \`my_app\`,
\`api_service\`). All related memories share the same project_id.
`;

type RuleTarget = {
  filename: string;
  label: string;
};

const RULE_TARGETS: RuleTarget[] = [
  { filename: "AGENTS.md", label: "OpenAI Codex / generic" },
  { filename: "CLAUDE.md", label: "Claude Code" },
  { filename: ".cursor/rules/memstate.mdc", label: "Cursor" },
  { filename: ".clinerules", label: "Cline" },
  { filename: ".windsurfrules", label: "Windsurf" },
];

function writeRuleFile(cwd: string, target: RuleTarget): "wrote" | "kept" | "failed" {
  const fullPath = path.join(cwd, target.filename);
  try {
    if (fs.existsSync(fullPath)) return "kept";
    fs.mkdirSync(path.dirname(fullPath), { recursive: true });
    fs.writeFileSync(fullPath, INSTRUCTIONS, "utf-8");
    return "wrote";
  } catch {
    return "failed";
  }
}

export async function main(): Promise<void> {
  const cwd = process.cwd();
  console.log(`memstate-local init — writing agent rule files in ${cwd}\n`);
  for (const target of RULE_TARGETS) {
    const status = writeRuleFile(cwd, target);
    const badge =
      status === "wrote" ? "✓ wrote" :
      status === "kept"  ? "— kept existing" :
                           "✗ failed";
    console.log(`  ${badge.padEnd(18)} ${target.filename}  (${target.label})`);
  }
  console.log(
    "\nDone. Restart your agent so it picks up the new rules.\n" +
      "See the daemon docs under ../server/ for the underlying store."
  );
}

// Allow direct execution.
if (process.argv[1] && (process.argv[1].endsWith("init.js") || process.argv[1].endsWith("init.ts"))) {
  main().catch((err) => {
    console.error(`\ninit failed: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(1);
  });
}
