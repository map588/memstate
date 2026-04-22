#!/usr/bin/env node
/**
 * memstate-local CLI — setup and init subcommands.
 *
 * Usage:
 *   memstate-local-mcp setup   — write Claude Code / Cursor / etc config
 *   memstate-local-mcp init    — write agent instruction files in cwd
 */

const command = process.argv[2];

async function main(): Promise<void> {
  switch (command) {
    case "setup": {
      const { main: setupMain } = await import("./setup.js");
      await setupMain();
      break;
    }
    case "init": {
      const { main: initMain } = await import("./init.js");
      await initMain();
      break;
    }
    default: {
      console.log(
        `
memstate-local-mcp CLI

Usage:
  memstate-local-mcp setup   Write MCP config into detected AI agents
  memstate-local-mcp init    Write agent instruction files in the current project

When no subcommand is given, stdin/stdout are used as an MCP server.
      `.trim()
      );
      break;
    }
  }
}

main().catch((err) => {
  console.error(`Error: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
