/**
 * The settings file, ~/.memstate/config.env: KEY=VALUE lines with the
 * MEMSTATE_* names everything already reads from the environment.
 *
 * Same rule as the daemon (server/config.go) and the Python scripts
 * (client/skill/scripts/_client.py): a key the environment already sets
 * wins, every other key is exported, so the proxy, the daemon it starts
 * and the scripts see ONE configuration. MEMSTATE_CONFIG names another
 * file; MEMSTATE_CONFIG=off skips it (tests, smoke runs).
 */
import * as fs from "fs";
import * as os from "os";
import * as path from "path";

export function configFilePath(env: NodeJS.ProcessEnv = process.env): string {
  if (env.MEMSTATE_CONFIG) {
    return env.MEMSTATE_CONFIG.replace(/^~(?=$|[\\/])/, os.homedir());
  }
  return path.join(os.homedir(), ".memstate", "config.env");
}

/** parseConfigFile returns the file's MEMSTATE_* entries; [] when it is missing. */
export function parseConfigFile(file: string): [string, string][] {
  let text: string;
  try {
    text = fs.readFileSync(file, "utf8");
  } catch {
    return [];
  }
  const out: [string, string][] = [];
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim().replace(/^export\s+/, "");
    if (!line || line.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq === -1) continue;
    const key = line.slice(0, eq).trim();
    if (!key.startsWith("MEMSTATE_")) continue;
    const value = line.slice(eq + 1).trim().replace(/^(['"])(.*)\1$/, "$2");
    out.push([key, value]);
  }
  return out;
}

/**
 * loadConfigFile exports the file into env (keys already set win) and
 * returns the path it read, or undefined when there is no file.
 */
export function loadConfigFile(env: NodeJS.ProcessEnv = process.env): string | undefined {
  if (env.MEMSTATE_CONFIG === "off") return undefined;
  const file = configFilePath(env);
  const entries = parseConfigFile(file);
  if (!fs.existsSync(file)) return undefined;
  for (const [key, value] of entries) {
    if (!env[key]) env[key] = value;
  }
  return file;
}

/**
 * writeConfigValue sets key in the file (or removes it when value is ""),
 * keeping every other line, comments included. Mirrors the Go writer.
 */
export function writeConfigValue(file: string, key: string, value: string): void {
  let text = "";
  try {
    text = fs.readFileSync(file, "utf8");
  } catch {
    /* new file */
  }
  const out: string[] = [];
  let done = false;
  for (const raw of text.replace(/[\r\n]+$/, "").split("\n")) {
    const line = raw.trim().replace(/^export\s+/, "");
    const eq = line.indexOf("=");
    if (eq !== -1 && !line.startsWith("#") && line.slice(0, eq).trim() === key) {
      if (value && !done) out.push(`${key}=${value}`);
      done = true;
      continue;
    }
    if (raw !== "" || text !== "") out.push(raw);
  }
  if (value && !done) out.push(`${key}=${value}`);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = `${file}.tmp`;
  fs.writeFileSync(tmp, out.join("\n").replace(/\n+$/, "") + "\n");
  fs.renameSync(tmp, file);
}
