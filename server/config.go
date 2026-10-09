package main

// Persistent settings live in ONE file, ~/.memstate/config.env: KEY=VALUE
// lines using the MEMSTATE_* names the daemon already reads from the
// environment. loadConfigFile runs first thing in main and exports every
// key the process environment does not already set, so the precedence is
//
//	daemon flag > environment > config.env > built-in default
//
// and no other code has to know the file exists: NewEmbedder, envThreshold,
// the idle timeout, discoverAddr and the CLI all keep reading os.Getenv.
// The MCP proxy (client/src/config.ts) and the Python scripts
// (client/skill/scripts/_client.py) apply the same file with the same rule.
// MEMSTATE_CONFIG names another file; MEMSTATE_CONFIG=off skips it, which
// is how tests and smoke runs stay hermetic.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const configFileName = "config.env"

// configSettings lists the keys `memstated config` shows, in display order,
// with the built-in default that applies when nothing sets them.
var configSettings = []struct{ key, def, help string }{
	{"MEMSTATE_EMBEDDING_URL", defaultEmbeddingURL, "embedding server; a URL ending in /v1 is OpenAI-compatible (llama.cpp, LM Studio, vLLM)"},
	{"MEMSTATE_EMBED_MODEL", defaultEmbedModel, "embedding model, as the server names it"},
	{"MEMSTATE_EMBED_TIMEOUT", defaultEmbedTimeout.String(), "max time per embedding call (covers a cold model load)"},
	{"MEMSTATE_SEMANTIC_THRESHOLD", fmt.Sprint(defaultThreshold), "cosine floor for semantic and hybrid search"},
	{"MEMSTATE_IDLE_TIMEOUT", "", "shared daemon exits after this much idleness (empty: never)"},
	{"MEMSTATE_ADDR", defaultAddr, "shared daemon address"},
	{"MEMSTATE_DB", "~/.memstate/memstate.db", "SQLite file"},
}

var (
	// configFileLoaded is the path loadConfigFile read, "" when no file exists.
	configFileLoaded string
	// configFileValues is every key the file carries, whether or not it
	// took effect.
	configFileValues = map[string]string{}
	// configApplied marks the keys loadConfigFile exported, i.e. the ones
	// the environment did not already set.
	configApplied = map[string]bool{}
)

func configFilePath() string {
	if v := os.Getenv("MEMSTATE_CONFIG"); v != "" {
		return expandHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".memstate", configFileName)
}

// loadConfigFile exports the file's settings into the environment (keys
// already set win) and returns how many it applied. A missing file is
// silent; a bad line is reported on stderr and skipped, never fatal, so a
// typo cannot take the recall hook or the CLI down.
func loadConfigFile() int {
	if os.Getenv("MEMSTATE_CONFIG") == "off" {
		return 0
	}
	path := configFilePath()
	values, err := parseConfigFile(path, os.Stderr)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "memstated: %s: %v\n", path, err)
		}
		return 0
	}
	configFileLoaded = path
	applied := 0
	for k, v := range values {
		configFileValues[k] = v
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
			configApplied[k] = true
			applied++
		}
	}
	return applied
}

// parseConfigFile reads KEY=VALUE lines. Blank lines and # comments are
// skipped, a leading "export " is tolerated, and quotes around a value are
// stripped. Lines that are not MEMSTATE_KEY=value are reported to warn
// and skipped.
func parseConfigFile(path string, warn io.Writer) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !strings.HasPrefix(k, "MEMSTATE_") {
			fmt.Fprintf(warn, "memstated: %s:%d: ignored %q (want MEMSTATE_KEY=value)\n", path, n+1, line)
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, nil
}

// writeConfigValue sets key in the file, or removes it when value is "",
// keeping every other line (comments included) as it was.
func writeConfigValue(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var out []string
	done := false
	for _, raw := range strings.Split(strings.TrimRight(string(b), "\r\n"), "\n") {
		line := strings.TrimSpace(raw)
		k, _, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if ok && !strings.HasPrefix(line, "#") && strings.TrimSpace(k) == key {
			if value != "" && !done {
				out = append(out, key+"="+value)
			}
			done = true
			continue
		}
		if raw != "" || len(b) > 0 {
			out = append(out, raw)
		}
	}
	if value != "" && !done {
		out = append(out, key+"="+value)
	}
	text := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// configSource reports the effective value of key and where it came from:
// "config.env", "env", or "default". An env value that shadows a file
// entry says so, because that is the case that confuses people.
func configSource(key, def string) (value, source string) {
	env := os.Getenv(key)
	switch {
	case configApplied[key]:
		return env, "config.env"
	case env != "":
		if fileVal, ok := configFileValues[key]; ok && fileVal != env {
			return env, "env (config.env says " + fileVal + ")"
		}
		return env, "env"
	default:
		return def, "default"
	}
}

// cmdConfig is `memstated config [set KEY VALUE | unset KEY | path]`.
// With no verb it prints every setting with its value and source, then
// compares with the running daemon, so "what is actually in effect" is
// one command away.
func cmdConfig(args []string) int {
	path := configFilePath()
	if len(args) > 0 {
		switch args[0] {
		case "path":
			fmt.Println(path)
			return 0
		case "set", "unset":
			key := ""
			if len(args) > 1 {
				key = args[1]
			}
			value := ""
			if args[0] == "set" {
				if len(args) != 3 {
					fmt.Fprintln(os.Stderr, "usage: memstated config set MEMSTATE_KEY VALUE")
					return 2
				}
				value = args[2]
			} else if len(args) != 2 {
				fmt.Fprintln(os.Stderr, "usage: memstated config unset MEMSTATE_KEY")
				return 2
			}
			if !strings.HasPrefix(key, "MEMSTATE_") {
				fmt.Fprintf(os.Stderr, "memstated config: %q is not a MEMSTATE_* setting; known keys:\n", key)
				for _, s := range configSettings {
					fmt.Fprintf(os.Stderr, "  %-28s %s\n", s.key, s.help)
				}
				return 2
			}
			if err := writeConfigValue(path, key, value); err != nil {
				fmt.Fprintf(os.Stderr, "memstated config: %v\n", err)
				return 1
			}
			if value == "" {
				fmt.Printf("removed %s from %s\n", key, path)
			} else {
				fmt.Printf("%s=%s written to %s\n", key, value, path)
			}
			if addr, ok := discoverAddr(); ok {
				fmt.Printf("a daemon runs at %s with the old value: `memstated restart` applies the change\n", addr)
			}
			return 0
		default:
			fmt.Fprintln(os.Stderr, "usage: memstated config [set MEMSTATE_KEY VALUE | unset MEMSTATE_KEY | path]")
			return 2
		}
	}

	w := os.Stdout
	if configFileLoaded != "" {
		fmt.Fprintf(w, "config file   %s   (%d settings)\n", configFileLoaded, len(configFileValues))
	} else {
		fmt.Fprintf(w, "config file   %s   (not found; `memstated config set KEY VALUE` creates it)\n", path)
	}
	fmt.Fprintf(w, "precedence    daemon flag > environment > config.env > default\n\n")
	fmt.Fprintf(w, "%-28s %-40s %s\n", "SETTING", "VALUE", "SOURCE")
	shown := map[string]bool{}
	for _, s := range configSettings {
		shown[s.key] = true
		v, src := configSource(s.key, s.def)
		if v == "" {
			v = "(unset)"
		}
		fmt.Fprintf(w, "%-28s %-40s %s\n", s.key, v, src)
	}
	var extra []string
	for k := range configFileValues {
		if !shown[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		v, src := configSource(k, "")
		fmt.Fprintf(w, "%-28s %-40s %s\n", k, v, src)
	}

	addr := subAddr(nil)
	h, err := fetchHealth(addr)
	if err != nil || h.Service != healthServiceName {
		fmt.Fprintf(w, "\nno daemon answers at %s\n", addr)
		return 0
	}
	fmt.Fprintf(w, "\nrunning daemon  %s   pid %d   started as: memstated %s\n",
		addr, h.Pid, strings.Join(h.Args, " "))
	if h.ConfigFile != "" {
		fmt.Fprintf(w, "  loaded %s\n", h.ConfigFile)
	}
	want := func(key, def string) string { v, _ := configSource(key, def); return v }
	drift := false
	check := func(name, live, cfg string) {
		if sameSetting(live, cfg) {
			fmt.Fprintf(w, "  %-20s %-40s ok\n", name, live)
			return
		}
		drift = true
		fmt.Fprintf(w, "  %-20s %-40s DIFFERS: config gives %s\n", name, live, cfg)
	}
	check("embedding_url", h.EmbeddingURL, want("MEMSTATE_EMBEDDING_URL", defaultEmbeddingURL))
	check("embed_model", h.EmbedModel, want("MEMSTATE_EMBED_MODEL", defaultEmbedModel))
	check("embed_timeout", h.EmbedTimeout, want("MEMSTATE_EMBED_TIMEOUT", defaultEmbedTimeout.String()))
	check("semantic_threshold", fmt.Sprint(h.SemanticThreshold), want("MEMSTATE_SEMANTIC_THRESHOLD", fmt.Sprint(defaultThreshold)))
	check("idle_timeout", h.IdleTimeout, want("MEMSTATE_IDLE_TIMEOUT", ""))
	if drift {
		fmt.Fprintf(w, "  -> `memstated restart` starts a daemon from the config above; flags it was started with still win\n")
	}
	return 0
}

// sameSetting compares a live value with a configured one, treating
// durations ("1m0s" vs "60s") and floats ("0.6" vs "0.60") as equal when
// they mean the same thing. Empty on both sides is equal.
func sameSetting(a, b string) bool {
	if a == b {
		return true
	}
	if da, err := time.ParseDuration(a); err == nil {
		if db, err := time.ParseDuration(b); err == nil {
			return da == db
		}
	}
	var fa, fb float64
	if _, err := fmt.Sscan(a, &fa); err == nil {
		if _, err := fmt.Sscan(b, &fb); err == nil {
			return fa == fb
		}
	}
	return false
}
