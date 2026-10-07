package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// runCLI runs one memstate verb with cliOut captured. It returns the exit
// code and everything the verb printed to stdout.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	prev := cliOut
	cliOut = &buf
	defer func() { cliOut = prev }()
	code := cmdCLI(args)
	return code, buf.String()
}

// runCLIErr is runCLI with stderr captured too.
func runCLIErr(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	prevOut, prevErr := cliOut, cliErr
	cliOut, cliErr = &out, &errb
	defer func() { cliOut, cliErr = prevOut, prevErr }()
	code := cmdCLI(args)
	return code, out.String(), errb.String()
}

func decodeCLI(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func TestResolveCLIProject(t *testing.T) {
	if got, err := resolveCLIProject(cliOpts{project: "my_app"}); err != nil || got != "my_app" {
		t.Fatalf("--project: %q %v", got, err)
	}
	if got, err := resolveCLIProject(cliOpts{user: true}); err != nil || got != userProject {
		t.Fatalf("--user: %q %v", got, err)
	}
	if _, err := resolveCLIProject(cliOpts{user: true, project: "x"}); err == nil {
		t.Fatal("--user with --project must be an error")
	}
	cwd, _ := os.Getwd()
	if got, err := resolveCLIProject(cliOpts{}); err != nil || got != deriveProject(cwd) {
		t.Fatalf("default: %q %v (want %q)", got, err, deriveProject(cwd))
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	j := fs.Bool("json", false, "")
	n := fs.Int("limit", 0, "")
	pos, err := parseInterspersed(fs, []string{"todo", "--json", "extra", "--limit", "3"})
	if err != nil || !*j || *n != 3 || strings.Join(pos, ",") != "todo,extra" {
		t.Fatalf("pos=%v json=%v limit=%d err=%v", pos, *j, *n, err)
	}
	// "--" ends flag parsing; hyphen-leading terms stay positional.
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	j = fs.Bool("json", false, "")
	pos, err = parseInterspersed(fs, []string{"--json", "--", "--limit", "x"})
	if err != nil || !*j || strings.Join(pos, ",") != "--limit,x" {
		t.Fatalf("pos=%v json=%v err=%v", pos, *j, err)
	}
}

// seedCLIStore writes a small project plus user-scope rows to a temp DB and
// returns the DB path.
func seedCLIStore(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cli.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	must := func(pid, kp, content string, meta WriteMeta) {
		t.Helper()
		if _, _, err := s.Write(pid, kp, content, meta, false); err != nil {
			t.Fatalf("seed %s/%s: %v", pid, kp, err)
		}
	}
	must("proj", "decisions.db", "sqlite over postgres", WriteMeta{Category: "decision"})
	must("proj", "decisions.db", "sqlite over postgres, WAL mode", WriteMeta{Category: "decision"})
	must("proj", "todo.a", "write the cli", WriteMeta{})
	must("proj", "todo.b", "ship it", WriteMeta{})
	must(userProject, "preferences.commit_style", "short subjects", WriteMeta{Category: "config"})
	must(userProject, "host."+hostSlug()+".env.go_bin", "~/.go/bin", WriteMeta{})
	must(userProject, "host.other_box.env.go_bin", "/opt/go/bin", WriteMeta{})
	return dbPath
}

func TestCLITreeGetHistory(t *testing.T) {
	// No shared daemon must be reachable through the environment.
	t.Setenv("MEMSTATE_ADDR", "")
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "none.db"))
	db := seedCLIStore(t)

	code, out := runCLI(t, "tree", "--project", "proj", "--db", db, "--json")
	if code != 0 {
		t.Fatalf("tree exit %d: %s", code, out)
	}
	tree := decodeCLI(t, out)
	if tree["project_id"] != "proj" || tree["total_memories"].(float64) != 3 {
		t.Fatalf("tree shape: %v", tree)
	}
	user := tree["user"].(map[string]any)
	if user["host"] != hostSlug() || user["total_memories"].(float64) != 2 {
		t.Fatalf("user block must be pruned to this host: %v", user)
	}
	if strings.Contains(out, "other_box") {
		t.Fatalf("other host leaked into tree:\n%s", out)
	}

	// Human tree: keypath names, no content, user block header.
	code, out = runCLI(t, "tree", "--project", "proj", "--db", db, "--no-color")
	if code != 0 || !strings.Contains(out, "decisions") || strings.Contains(out, "WAL mode") ||
		!strings.Contains(out, "user scope") {
		t.Fatalf("human tree:\n%s", out)
	}

	// tree KEYPATH narrows to the subtree.
	code, out = runCLI(t, "tree", "todo", "--project", "proj", "--db", db, "--json")
	tree = decodeCLI(t, out)
	if code != 0 || tree["total_memories"].(float64) != 2 {
		t.Fatalf("tree todo: %v", tree)
	}

	// get: content, flags after positionals.
	code, out = runCLI(t, "get", "decisions.db", "--project", "proj", "--db", db, "--json")
	got := decodeCLI(t, out)
	if code != 0 || got["total_count"].(float64) != 1 {
		t.Fatalf("get json: %v", got)
	}
	code, out = runCLI(t, "get", "decisions.db", "--project", "proj", "--db", db, "--raw")
	if code != 0 || strings.TrimSpace(out) != "sqlite over postgres, WAL mode" {
		t.Fatalf("get --raw: %q", out)
	}
	code, _, errText := runCLIErr(t, "get", "nope.x", "--project", "proj", "--db", db)
	if code != 1 || !strings.Contains(errText, "no memories") {
		t.Fatalf("get missing: code=%d err=%q", code, errText)
	}

	// --user reads the reserved project.
	code, out = runCLI(t, "get", "preferences", "--user", "--db", db, "--raw")
	if code != 0 || !strings.Contains(out, "short subjects") {
		t.Fatalf("get --user: %q", out)
	}

	// history: newest first.
	code, out = runCLI(t, "history", "decisions.db", "--project", "proj", "--db", db, "--json")
	hist := decodeCLI(t, out)
	versions := hist["versions"].([]any)
	if code != 0 || len(versions) != 2 || versions[0].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("history: %v", hist)
	}
	code, out = runCLI(t, "history", "decisions.db", "--project", "proj", "--db", db, "--no-color")
	if code != 0 || !strings.Contains(out, "v2") || !strings.Contains(out, "v1") {
		t.Fatalf("history human:\n%s", out)
	}
}

func TestCLIWritesNeedDaemon(t *testing.T) {
	t.Setenv("MEMSTATE_ADDR", "127.0.0.1:9") // nothing listens here
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "none.db"))
	code, _, errText := runCLIErr(t, "set", "todo.x", "value", "--project", "proj")
	if code != 1 || !strings.Contains(errText, "daemon") {
		t.Fatalf("set without daemon: code=%d err=%q", code, errText)
	}
	t.Setenv("MEMSTATE_ADDR", "")
	code, _, errText = runCLIErr(t, "rm", "todo.x", "--project", "proj")
	if code != 1 || !strings.Contains(errText, "MEMSTATE_ADDR") {
		t.Fatalf("rm without daemon: code=%d err=%q", code, errText)
	}
}

func TestCLISetEditRmViaDaemon(t *testing.T) {
	ts := newTestServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")
	t.Setenv("MEMSTATE_ADDR", addr)
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "none.db"))

	code, out := runCLI(t, "set", "todo.x", "first", "--project", "proj", "--category", "status", "--json")
	if code != 0 || decodeCLI(t, out)["action"] != "created" {
		t.Fatalf("set create: %d %s", code, out)
	}
	code, out = runCLI(t, "set", "todo.x", "second", "--project", "proj", "--category", "status", "--no-color")
	if code != 0 || !strings.Contains(out, "superseded") || !strings.Contains(out, "v2") {
		t.Fatalf("set supersede: %d %s", code, out)
	}
	// VALUE "-" reads stdin.
	prevIn := cliIn
	cliIn = strings.NewReader("from stdin\n")
	code, out = runCLI(t, "set", "todo.y", "-", "--project", "proj", "--json")
	cliIn = prevIn
	if code != 0 || decodeCLI(t, out)["action"] != "created" {
		t.Fatalf("set stdin: %d %s", code, out)
	}

	// The daemon's gate message reaches the terminal.
	code, _, errText := runCLIErr(t, "set", "todo.x", "nope", "--user")
	if code != 1 || !strings.Contains(errText, "allowed shapes") {
		t.Fatalf("set --user todo: code=%d err=%q", code, errText)
	}

	// edit: the editor appends a line; category carries over.
	if runtime.GOOS == "windows" {
		t.Skip("shell-script editor")
	}
	editor := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf '\\nappended\\n' >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", editor)
	code, out = runCLI(t, "edit", "todo.x", "--project", "proj", "--json")
	edited := decodeCLI(t, out)
	if code != 0 || edited["action"] != "superseded" {
		t.Fatalf("edit: %d %s", code, out)
	}
	stored := edited["stored"].(map[string]any)
	if stored["version"].(float64) != 3 || stored["category"] != "status" {
		t.Fatalf("edit result: %v", stored)
	}
	if _, has := stored["content"]; has {
		t.Fatalf("edit reply echoes content: %v", stored)
	}
	// The daemon search asks for include_content, so the stored text shows.
	code, out = runCLI(t, "search", "appended", "--project", "proj", "--json")
	got := decodeCLI(t, out)
	if code != 0 || !strings.Contains(got["results"].([]any)[0].(map[string]any)["content"].(string), "appended") {
		t.Fatalf("edit did not store the appended line: %d %s", code, out)
	}
	// An editor that changes nothing writes nothing.
	t.Setenv("EDITOR", "true")
	code, out = runCLI(t, "edit", "todo.x", "--project", "proj", "--no-color")
	if code != 0 || !strings.Contains(out, "no change") {
		t.Fatalf("edit unchanged: %d %s", code, out)
	}

	// rm --recursive needs --yes when stdin is not a terminal.
	code, _, errText = runCLIErr(t, "rm", "todo", "--recursive", "--project", "proj")
	if code != 1 || !strings.Contains(errText, "--yes") {
		t.Fatalf("rm recursive without --yes: code=%d err=%q", code, errText)
	}
	code, out = runCLI(t, "rm", "todo", "--recursive", "--yes", "--project", "proj", "--json")
	if code != 0 || decodeCLI(t, out)["deleted_count"].(float64) != 2 {
		t.Fatalf("rm recursive: %d %s", code, out)
	}
	_, hist := postJSON(t, ts.URL+"/api/v1/memories/history", map[string]any{
		"project_id": "proj", "keypath": "todo.x",
	})
	versions := hist["versions"].([]any)
	if versions[0].(map[string]any)["tombstone"] != true {
		t.Fatalf("rm must tombstone: %v", versions[0])
	}
	// Single rm asks nothing.
	runCLI(t, "set", "todo.z", "v", "--project", "proj")
	code, out = runCLI(t, "rm", "todo.z", "--project", "proj", "--json")
	if code != 0 || decodeCLI(t, out)["deleted_count"].(float64) != 1 {
		t.Fatalf("rm single: %d %s", code, out)
	}
}

func TestCLISearchFallsBackToFTS(t *testing.T) {
	t.Setenv("MEMSTATE_ADDR", "")
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "none.db"))
	db := seedCLIStore(t)

	// No daemon: FTS over SQLite, degraded reason set.
	code, out := runCLI(t, "search", "sqlite", "postgres", "--project", "proj", "--db", db, "--json")
	res := decodeCLI(t, out)
	if code != 0 || res["mode"] != "fts" || res["degraded"] == nil || res["total_found"].(float64) != 1 {
		t.Fatalf("fts fallback: %d %v", code, res)
	}
	code, out = runCLI(t, "search", "sqlite", "--project", "proj", "--db", db, "--no-color")
	if code != 0 || !strings.Contains(out, "fts only") {
		t.Fatalf("fts note missing:\n%s", out)
	}
	// --user drops other hosts.
	code, out = runCLI(t, "search", "go_bin", "--user", "--db", db, "--json")
	res = decodeCLI(t, out)
	if code != 0 || res["total_found"].(float64) != 1 {
		t.Fatalf("user search host filter: %v", res)
	}

	// With a daemon: the daemon answers (hybrid, degraded to fts without Ollama).
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "proj", "keypath": "notes.x", "content": "the daemon answered this",
	})
	code, out = runCLI(t, "search", "daemon answered", "--project", "proj", "--json")
	res = decodeCLI(t, out)
	if code != 0 || res["mode"] != "hybrid" || res["total_found"].(float64) != 1 {
		t.Fatalf("daemon search: %d %v", code, res)
	}
}

func TestCLIUsageAndUnknownVerb(t *testing.T) {
	code, _, errText := runCLIErr(t, "frobnicate")
	if code != 2 || !strings.Contains(errText, "tree") {
		t.Fatalf("unknown verb: code=%d err=%q", code, errText)
	}
	code, _, errText = runCLIErr(t)
	if code != 2 || !strings.Contains(errText, "Usage") {
		t.Fatalf("no verb: code=%d err=%q", code, errText)
	}
	code, _, _ = runCLIErr(t, "--help")
	if code != 0 {
		t.Fatalf("--help exit %d", code)
	}
}

func TestMemstateArgv0(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink dispatch")
	}
	bin := buildDaemon(t)
	link := filepath.Join(t.TempDir(), "memstate")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	env := append(cleanEnv(), "MEMSTATE_DB="+filepath.Join(t.TempDir(), "t.db"))

	cmd := exec.Command(link, "--help")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "memstate tree") {
		t.Fatalf("memstate --help: %v\n%s", err, out)
	}

	// No verb: usage and exit 2, never a daemon.
	cmd = exec.Command(link)
	cmd.Env = env
	done := make(chan error, 1)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
			t.Fatalf("bare memstate: %v\n%s", err, buf.String())
		}
		if strings.Contains(buf.String(), "MEMSTATE_READY") {
			t.Fatalf("bare memstate started a daemon:\n%s", buf.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("bare memstate did not exit; it started the daemon")
	}
}
