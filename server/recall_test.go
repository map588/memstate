package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestSlugProjectProperties(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9_]+$`)
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.String().Draw(t, "name")
		s := slugProject(name)
		if !valid.MatchString(s) {
			t.Fatalf("slug %q of %q has characters outside [a-z0-9_]", s, name)
		}
		if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
			t.Fatalf("slug %q of %q has an edge underscore", s, name)
		}
		if again := slugProject(s); again != s {
			t.Fatalf("slug is not idempotent: %q -> %q", s, again)
		}
	})
}

func TestDeriveProject(t *testing.T) {
	// Non-repository directory: its own name, slugged.
	plain := filepath.Join(t.TempDir(), "My-App v2")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := deriveProject(plain); got != "my_app_v2" {
		t.Fatalf("plain dir: got %q want my_app_v2", got)
	}
	if got := deriveProject(""); got != "default" {
		t.Fatalf("empty cwd: got %q want default", got)
	}

	// Repository: the top-level name wins even from a nested directory.
	repo := filepath.Join(t.TempDir(), "Repo.Name")
	nested := filepath.Join(repo, "sub", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v %s", err, out)
	}
	if got := deriveProject(nested); got != "repo_name" {
		t.Fatalf("nested repo dir: got %q want repo_name", got)
	}
}

func TestRenderRecall(t *testing.T) {
	long := strings.Repeat("x", 600)
	fts, both := []string{"fts"}, []string{"fts", "semantic"}
	hits := []recallHit{
		{Keypath: "a", Content: "seen already", Sources: both},
		{Keypath: "b", Content: long, Category: "gotcha", Sources: fts},
		{Keypath: "c", Content: "  short  ", Sources: fts},
		{Keypath: "d", Content: "fourth", Sources: both},
		{Keypath: "e", Content: "fifth", Sources: both},
	}
	text, shown := renderRecall("proj", hits, nil, map[string]bool{"a": true}, 3, 500)
	if want := []string{"b", "c", "d"}; strings.Join(shown, ",") != strings.Join(want, ",") {
		t.Fatalf("shown = %v want %v", shown, want)
	}
	// A hit below the cap fills a freed slot only with a semantic source.
	hits[3].Sources = fts
	if _, shown := renderRecall("proj", hits, nil, map[string]bool{"a": true}, 3, 500); strings.Join(shown, ",") != "b,c,e" {
		t.Fatalf("fts-only backfill must be skipped, semantic backfill taken: shown = %v", shown)
	}
	hits[4].Sources = fts
	if _, shown := renderRecall("proj", hits, nil, map[string]bool{"a": true}, 3, 500); strings.Join(shown, ",") != "b,c" {
		t.Fatalf("no semantic candidates below the cap: shown = %v", shown)
	}
	hits[3].Sources = both
	if !strings.HasPrefix(text, "<memstate-recall project=\"proj\">\n") ||
		!strings.HasSuffix(text, "</memstate-recall>\n") {
		t.Fatalf("block markers missing:\n%s", text)
	}
	if strings.Contains(text, "seen already") || strings.Contains(text, "fifth") {
		t.Fatalf("seen or over-cap hit leaked:\n%s", text)
	}
	if !strings.Contains(text, "### b [gotcha]\n"+strings.Repeat("x", 500)+"[…truncated]\n") {
		t.Fatalf("truncation or category header wrong:\n%s", text)
	}
	if !strings.Contains(text, "### c\nshort\n") {
		t.Fatalf("content should be trimmed:\n%s", text)
	}
	if text, shown := renderRecall("proj", hits[:1], nil, map[string]bool{"a": true}, 3, 500); text != "" || shown != nil {
		t.Fatalf("all-seen must render nothing, got %q %v", text, shown)
	}
}

func TestRunRecallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	t.Setenv("MEMSTATE_RECALL_DEBUG", "")
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))

	cwd := filepath.Join(t.TempDir(), "recall-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "recall_proj", "keypath": "gotchas.timeout",
		"content": "the embed timeout must cover a cold model load", "category": "gotcha",
	})

	run := func(event string) string {
		var out bytes.Buffer
		if code := runRecall(strings.NewReader(event), &out); code != 0 {
			t.Fatalf("runRecall exit %d", code)
		}
		return out.String()
	}
	event := `{"session_id":"s1","cwd":` + jsonString(cwd) +
		`,"prompt":"why does the embed timeout matter for cold loads"}`

	first := run(event)
	if !strings.Contains(first, `<memstate-recall project="recall_proj">`) ||
		!strings.Contains(first, "### gotchas.timeout [gotcha]") {
		t.Fatalf("first prompt should inject the hit, got:\n%s", first)
	}
	if second := run(event); second != "" {
		t.Fatalf("same session must not repeat a keypath, got:\n%s", second)
	}
	other := strings.Replace(event, `"s1"`, `"s2"`, 1)
	if third := run(other); !strings.Contains(third, "gotchas.timeout") {
		t.Fatalf("a new session starts with an empty seen set, got:\n%s", third)
	}

	short := `{"session_id":"s3","cwd":` + jsonString(cwd) + `,"prompt":"yes do it"}`
	if got := run(short); got != "" {
		t.Fatalf("three-word prompt must print nothing, got %q", got)
	}
	t.Setenv("MEMSTATE_NO_RECALL", "1")
	if got := run(strings.Replace(event, `"s1"`, `"s4"`, 1)); got != "" {
		t.Fatalf("MEMSTATE_NO_RECALL must print nothing, got %q", got)
	}
	t.Setenv("MEMSTATE_NO_RECALL", "")

	// Unreachable daemon: silent exit 0.
	t.Setenv("MEMSTATE_ADDR", "127.0.0.1:9")
	if got := run(strings.Replace(event, `"s1"`, `"s5"`, 1)); got != "" {
		t.Fatalf("unreachable daemon must print nothing, got %q", got)
	}

	// Seen files older than the TTL are pruned on the next run.
	old := filepath.Join(dir, "recall", "ancient")
	if err := os.WriteFile(old, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	run(strings.Replace(event, `"s1"`, `"s6"`, 1))
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale seen file should be pruned, stat err=%v", err)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestRenderRecallUserScope(t *testing.T) {
	both := []string{"fts", "semantic"}
	hits := []recallHit{
		{Keypath: "p1", Content: "project one", Sources: both},
		{Keypath: "p2", Content: "project two", Sources: both},
		{Keypath: "p3", Content: "project three", Sources: both},
	}
	user := []recallHit{
		{Keypath: "preferences.commit_style", Content: "no trailers", Category: "config", Sources: both},
		{Keypath: "profile.role", Content: "backend", Sources: both},
	}
	text, shown := renderRecall("proj", hits, user, map[string]bool{}, 3, 500)
	// One slot goes to the user scope, the rest to the project.
	if want := "p1,p2,_user/preferences.commit_style"; strings.Join(shown, ",") != want {
		t.Fatalf("shown = %v want %s", shown, want)
	}
	if !strings.Contains(text, "### preferences.commit_style [user] [config]\nno trailers\n") {
		t.Fatalf("user hit marker missing:\n%s", text)
	}
	if strings.Contains(text, "profile.role") || strings.Contains(text, "project three") {
		t.Fatalf("second user hit or fourth hit leaked:\n%s", text)
	}
	// A seen user hit frees its slot for the next user hit.
	seen := map[string]bool{"_user/preferences.commit_style": true}
	if _, shown := renderRecall("proj", hits, user, seen, 3, 500); strings.Join(shown, ",") != "p1,p2,_user/profile.role" {
		t.Fatalf("shown = %v", shown)
	}
	// A project keypath equal to a user keypath is not suppressed by it.
	seen = map[string]bool{"preferences.commit_style": true}
	if _, shown := renderRecall("proj", hits, user, seen, 3, 500); strings.Join(shown, ",") != "p1,p2,_user/preferences.commit_style" {
		t.Fatalf("seen keys must be scoped: shown = %v", shown)
	}
	// User hits alone still render.
	if text, _ := renderRecall("proj", nil, user, map[string]bool{}, 3, 500); !strings.Contains(text, "### preferences.commit_style [user]") {
		t.Fatalf("user-only render:\n%s", text)
	}
}

func TestFilterHostHits(t *testing.T) {
	hits := []recallHit{
		{Keypath: "preferences.x"},
		{Keypath: "host.mbp.env.go_bin"},
		{Keypath: "host.other.env.go_bin"},
		{Keypath: "host"},
	}
	got := filterHostHits(hits, "mbp")
	if len(got) != 3 || got[0].Keypath != "preferences.x" || got[1].Keypath != "host.mbp.env.go_bin" || got[2].Keypath != "host" {
		t.Fatalf("got %+v", got)
	}
}

func TestRunRecallUserScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))

	cwd := filepath.Join(t.TempDir(), "scope-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := func(kp, content string) {
		t.Helper()
		code, out := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": userProject, "keypath": kp, "content": content,
		})
		if code != 200 {
			t.Fatalf("seed %s: %d %v", kp, code, out)
		}
	}
	seed("preferences.commit_style", "the user wants short commit subjects and no trailers")
	seed("host."+hostSlug()+".env.go_bin", "commit binaries of go live in GOBIN under the home dir")
	seed("host.other_box.env.go_bin", "commit binaries of go live in /opt on the other box")

	var out bytes.Buffer
	event := `{"session_id":"u1","cwd":` + jsonString(cwd) +
		`,"prompt":"what commit style does the user want for go binaries"}`
	if code := runRecall(strings.NewReader(event), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	if !strings.Contains(text, "[user]") {
		t.Fatalf("user hit missing:\n%s", text)
	}
	if strings.Contains(text, "other_box") {
		t.Fatalf("other host leaked:\n%s", text)
	}
	if strings.Count(text, "[user]") != 1 {
		t.Fatalf("more than one user slot:\n%s", text)
	}
}

func TestProjectCandidates(t *testing.T) {
	sem, fts := []string{"fts", "semantic"}, []string{"fts"}
	hits := []recallHit{
		{ProjectID: "cwd_proj", Keypath: "a", Sources: sem},
		{ProjectID: "_user", Keypath: "preferences.x", Sources: sem},
		{ProjectID: "nginx_server", Keypath: "a", Sources: fts},
		{ProjectID: "nginx_server", Keypath: "b", Sources: fts},
		{ProjectID: "weak", Keypath: "a", Sources: fts},
		{ProjectID: "infra", Keypath: "a", Sources: sem},
		{ProjectID: "big", Keypath: "a", Sources: fts},
		{ProjectID: "big", Keypath: "b", Sources: fts},
		{ProjectID: "big", Keypath: "c", Sources: fts},
		{ProjectID: "also", Keypath: "a", Sources: sem},
		{ProjectID: "also", Keypath: "b", Sources: sem},
	}
	got := projectCandidates(hits, "cwd_proj")
	ids := make([]string, len(got))
	for i, c := range got {
		ids[i] = c.ProjectID
	}
	// Only semantic hits count: cwd and _user dropped; nginx_server, weak
	// and big are FTS-only word matches and vanish; also (2) ranks above
	// infra (1).
	if want := "also,infra"; strings.Join(ids, ",") != want {
		t.Fatalf("candidates %v want %s", ids, want)
	}
	if got[0].Hits != 2 || got[1].Hits != 1 {
		t.Fatalf("candidate detail: %+v", got)
	}
	// Top three by count, ties by name.
	many := []recallHit{
		{ProjectID: "c", Keypath: "a", Sources: sem},
		{ProjectID: "b", Keypath: "a", Sources: sem},
		{ProjectID: "a", Keypath: "a", Sources: sem},
		{ProjectID: "d", Keypath: "a", Sources: sem},
		{ProjectID: "d", Keypath: "b", Sources: sem},
	}
	got = projectCandidates(many, "cwd_proj")
	ids = ids[:0]
	for _, c := range got {
		ids = append(ids, c.ProjectID)
	}
	if want := "d,a,b"; strings.Join(ids, ",") != want {
		t.Fatalf("top three %v want %s", ids, want)
	}
}

func TestScopeBlockText(t *testing.T) {
	plain := t.TempDir()
	text := scopeBlock("scratch", plain, false, nil)
	if !strings.HasPrefix(text, "<memstate-scope cwd_project=\"scratch\" exists=\"false\">\n") ||
		!strings.Contains(text, "is not a git repository ("+filepath.Base(plain)+")") ||
		!strings.Contains(text, `Project "scratch" does not exist yet; a write creates it only with new_project=true.`) ||
		!strings.Contains(text, "matches no other project") ||
		!strings.Contains(text, "Default is the cwd project.") ||
		!strings.HasSuffix(text, "</memstate-scope>\n") {
		t.Fatalf("plain dir block:\n%s", text)
	}
	// An existing project gets the attribute and no creation sentence.
	text = scopeBlock("scratch", plain, true, nil)
	if !strings.HasPrefix(text, "<memstate-scope cwd_project=\"scratch\" exists=\"true\">\n") ||
		strings.Contains(text, "does not exist yet") {
		t.Fatalf("existing project block:\n%s", text)
	}
	// The home directory never has a default project for writes. Skipped
	// when the home directory itself is a git repository.
	if home, err := os.UserHomeDir(); err == nil {
		if _, isRepo := repoRoot(home); !isRepo {
			got := scopeBlock("me", home, true, nil)
			// The rule sentence must not contradict the line above it, and
			// exists= is noise where no write can land.
			if !strings.HasPrefix(got, "<memstate-scope cwd_project=\"me\">\n") ||
				strings.Contains(got, "exists=") ||
				!strings.Contains(got, "(home directory)") ||
				!strings.Contains(got, "no default project for writes") ||
				!strings.Contains(got, "Every write needs project_name") ||
				!strings.Contains(got, `scope="user"`) ||
				strings.Contains(got, "Default is the cwd project") {
				t.Fatalf("home block:\n%s", got)
			}
		}
	}
	cands := []projectCandidate{{"nginx_server", 3}, {"infra", 2}, {"one", 1}}
	text = scopeBlock("me", plain, true, cands)
	if !strings.Contains(text, "Prompt matches other projects: nginx_server (3 semantic hits), infra (2 semantic hits), one (1 semantic hit).") {
		t.Fatalf("candidates line:\n%s", text)
	}

	repo := filepath.Join(t.TempDir(), "Repo.Name")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v %s", err, out)
	}
	if got := scopeBlock("repo_name", repo, true, nil); !strings.Contains(got, "is the git repository Repo.Name.") {
		t.Fatalf("repo block:\n%s", got)
	}
}

func TestScopeBlockFirstPromptOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	ts := newTestServer(t)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))

	cwd := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := func(project, kp, content string) {
		t.Helper()
		code, out := postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": project, "keypath": kp, "content": content,
		})
		if code != 200 {
			t.Fatalf("seed: %d %v", code, out)
		}
	}
	seed("nginx_server", "config.sites", "the nginx config for the sites lives in sites-enabled")
	seed("nginx_server", "config.tls", "nginx config uses certbot for tls")
	seed("scratch", "notes.x", "the nginx config note in the scratch project")
	seed("lonely", "notes.y", "one nginx config mention only")

	run := func(session, prompt string) string {
		var out bytes.Buffer
		event := `{"session_id":` + jsonString(session) + `,"cwd":` + jsonString(cwd) +
			`,"prompt":` + jsonString(prompt) + `}`
		if code := runRecall(strings.NewReader(event), &out); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return out.String()
	}
	// No embedder here: every hit is an FTS word match, so no project
	// qualifies as a candidate, however many words it shares.
	first := run("sc1", "help me set up my nginx config")
	if !strings.Contains(first, `<memstate-scope cwd_project="scratch" exists="true">`) ||
		!strings.Contains(first, "Prompt matches no other project.") ||
		strings.Contains(first, "nginx_server (") ||
		!strings.Contains(first, `<memstate-recall project="scratch">`) {
		t.Fatalf("first prompt:\n%s", first)
	}
	if strings.Index(first, "<memstate-scope") > strings.Index(first, "<memstate-recall") {
		t.Fatalf("scope block must come first:\n%s", first)
	}
	second := run("sc1", "more about the nginx config please")
	if strings.Contains(second, "<memstate-scope") {
		t.Fatalf("scope block repeated in the same session:\n%s", second)
	}
	if again := run("sc2", "help me set up my nginx config"); !strings.Contains(again, "<memstate-scope") {
		t.Fatalf("a new session must get the scope block:\n%s", again)
	}
	seen := loadSeen(recallSeenPath("sc1"))
	if !seen[scopeMarker] || !seen["notes.x"] {
		t.Fatalf("seen file: %v", seen)
	}
	// A cwd whose project has no memories still gets the block.
	empty := filepath.Join(t.TempDir(), "nothing_here")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	event := `{"session_id":"sc3","cwd":` + jsonString(empty) + `,"prompt":"help me set up my nginx config"}`
	runRecall(strings.NewReader(event), &out)
	if !strings.Contains(out.String(), `<memstate-scope cwd_project="nothing_here" exists="false">`) ||
		strings.Contains(out.String(), "<memstate-recall") {
		t.Fatalf("empty project first prompt:\n%s", out.String())
	}
}

func TestScopeBlockSemanticCandidates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	ollama := mockOllama(t)
	defer ollama.Close()
	embedder := newTestEmbedder(t, ollama)
	ts := newTestServerWithEmbedder(t, embedder)
	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	// The mock embedder puts every text near every other, so each hit
	// carries a semantic source: this checks the plumbing from the
	// all-projects search into the block, not the ranking quality.
	t.Setenv("MEMSTATE_SEMANTIC_THRESHOLD", "0")

	cwd := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range [][2]string{
		{"nginx_server", "config.sites"}, {"nginx_server", "config.tls"}, {"lonely", "notes.y"},
	} {
		postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
			"project_id": s[0], "keypath": s[1], "content": "nginx config " + s[1],
		})
	}
	embedder.WaitForPending()

	var out bytes.Buffer
	event := `{"session_id":"sem1","cwd":` + jsonString(cwd) + `,"prompt":"help me set up my nginx config"}`
	if code := runRecall(strings.NewReader(event), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	if !strings.Contains(text, "Prompt matches other projects: nginx_server (2 semantic hits), lonely (1 semantic hit).") {
		t.Fatalf("semantic candidates:\n%s", text)
	}
}

func TestPinnedProject(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	cwd := filepath.Join(dir, "work")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := pinnedProject(cwd); got != "" {
		t.Fatalf("no pins dir: got %q", got)
	}
	pins := filepath.Join(dir, "recall", "pins")
	if err := os.MkdirAll(pins, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pins, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A live proxy (this test process) pinned another directory: ignored.
	write(strconv.Itoa(os.Getpid()), filepath.Join(dir, "elsewhere")+"\nother\n")
	if got := pinnedProject(cwd); got != "" {
		t.Fatalf("other cwd: got %q", got)
	}
	// A dead proxy pinned this directory: ignored and pruned.
	write("999999999", cwd+"\ndead_pin\n")
	if got := pinnedProject(cwd); got != "" {
		t.Fatalf("dead pid: got %q", got)
	}
	if _, err := os.Stat(filepath.Join(pins, "999999999")); !os.IsNotExist(err) {
		t.Fatalf("dead pin file not pruned: %v", err)
	}
	// A live proxy pinned this directory: that project wins.
	write(strconv.Itoa(os.Getpid()), cwd+"\nnginx_server\n")
	if got := pinnedProject(cwd); got != "nginx_server" {
		t.Fatalf("live pin: got %q", got)
	}
	// Garbage in the directory never breaks the hook.
	write("notapid", "junk")
	write(strconv.Itoa(os.Getpid()), "")
	if got := pinnedProject(cwd); got != "" {
		t.Fatalf("garbage: got %q", got)
	}
}

// A daemon on another version gets a one-time notice, even when the search
// fails, and a daemon on this version gets none.
func TestRunRecallVersionSkewNotice(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	t.Setenv("MEMSTATE_NO_RECALL", "")
	t.Setenv("MEMSTATE_RECALL_DEBUG", "")
	ts := newTestServer(t)
	cwd := filepath.Join(t.TempDir(), "skew-proj")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	postJSON(t, ts.URL+"/api/v1/memories/remember", map[string]any{
		"project_id": "skew_proj", "keypath": "gotchas.timeout",
		"content": "the embed timeout must cover a cold model load", "category": "gotcha",
	})
	// old answers /health as version 0.0.1 and relays everything else to
	// the real daemon; when failSearch is set it rejects the search like a
	// daemon that does not know include_content.
	target, _ := url.Parse(ts.URL)
	relay := httputil.NewSingleHostReverseProxy(target)
	failSearch := false
	healthBody := `{"service":"memstate","version":"0.0.1"}`
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(healthBody))
		case failSearch && r.URL.Path == "/api/v1/memories/search":
			http.Error(w, `{"error":"json: unknown field \"include_content\""}`, 400)
		default:
			relay.ServeHTTP(w, r)
		}
	}))
	defer old.Close()

	run := func(session string) string {
		var out bytes.Buffer
		event := `{"session_id":"` + session + `","cwd":` + jsonString(cwd) +
			`,"prompt":"why does the embed timeout matter for cold loads"}`
		if code := runRecall(strings.NewReader(event), &out); code != 0 {
			t.Fatalf("runRecall exit %d", code)
		}
		return out.String()
	}

	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(old.URL, "http://"))
	first := run("v1")
	if !strings.Contains(first, "<memstate-notice>") || !strings.Contains(first, "version 0.0.1") ||
		!strings.Contains(first, "runs version "+healthVersion) || !strings.Contains(first, "gotchas.timeout") {
		t.Fatalf("first prompt should carry the notice and the hit, got:\n%s", first)
	}
	if second := run("v1"); strings.Contains(second, "<memstate-notice>") {
		t.Fatalf("the notice is one per session, got:\n%s", second)
	}

	failSearch = true
	if got := run("v2"); !strings.Contains(got, "<memstate-notice>") || strings.Contains(got, "<memstate-recall") {
		t.Fatalf("a failed search must still print the notice alone, got:\n%s", got)
	}
	failSearch = false

	// One version string, another build: the common case right after
	// `make install`, when the daemon still runs the previous build.
	healthBody = `{"service":"memstate","version":"` + healthVersion + `","build":"feedface0001"}`
	if got := run("v4"); !strings.Contains(got, "<memstate-notice>") || !strings.Contains(got, "(build feedface0001)") {
		t.Fatalf("a different build must print the notice, got:\n%s", got)
	}

	t.Setenv("MEMSTATE_ADDR", strings.TrimPrefix(ts.URL, "http://"))
	if got := run("v3"); strings.Contains(got, "<memstate-notice>") || !strings.Contains(got, "gotchas.timeout") {
		t.Fatalf("same version must print hits without a notice, got:\n%s", got)
	}
}
