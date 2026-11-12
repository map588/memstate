#!/usr/bin/env node
// sha256sums.mjs DIR writes DIR/SHA256SUMS: one line for each file in DIR,
// "<sha256 in hex>  <file name>", the format of the `sha256sum` program.
//
// `make release` runs it on dist/, so the file becomes a release asset.
// `memstated upgrade`, install.sh and install.ps1 check each download
// against it. Node does the work because the Makefile also runs on native
// Windows, which has no sha256sum.
import { createHash } from "node:crypto";
import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const OUT = "SHA256SUMS";
const dir = process.argv[2];
if (!dir) {
  console.error("usage: sha256sums.mjs DIR");
  process.exit(2);
}

const names = readdirSync(dir)
  .filter((name) => name !== OUT && statSync(join(dir, name)).isFile())
  .sort();
if (names.length === 0) {
  console.error(`sha256sums.mjs: no files in ${dir}`);
  process.exit(1);
}
const lines = names.map((name) => {
  const digest = createHash("sha256").update(readFileSync(join(dir, name))).digest("hex");
  return `${digest}  ${name}\n`;
});
// Always "\n", on Windows too: the readers split on it.
writeFileSync(join(dir, OUT), lines.join(""));
