package main

import (
	"bytes"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// noDaemonStart makes a test fail when the hook starts a daemon that the
// test did not plan. The executable of a test is not a daemon.
func noDaemonStart(t *testing.T) {
	t.Helper()
	startDaemon = func(exe, addr string, prev *healthResponse) error {
		t.Errorf("unplanned daemon start at %s", addr)
		return errors.New("unplanned daemon start")
	}
	t.Cleanup(func() { startDaemon = startDetachedDaemon })
}

// The PreCompact output starts with the fixed text, stays below the 10,000
// characters at which Claude Code cuts a hook's output, carries the user's
// text when there is one, and keeps each keypath name on its own line.
func TestCompactInstructionsProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		custom := rapid.String().Draw(t, "custom")
		names := rapid.SliceOf(rapid.String()).Draw(t, "names")
		out := compactInstructions(custom, names)
		if !strings.HasPrefix(out, compactInstructionsText) {
			t.Fatalf("output must start with the fixed text:\n%s", out)
		}
		if n := utf16Len(out); n >= 10000 {
			t.Fatalf("output is %d UTF-16 units, Claude Code cuts at 10000", n)
		}
		rest := strings.TrimPrefix(out, compactInstructionsText)
		if trimmed := strings.TrimSpace(custom); trimmed == "" {
			if len(names) == 0 && rest != "" {
				t.Fatalf("no user text and no names: output must be the fixed text only, got %q", rest)
			}
		} else if !strings.HasSuffix(rest, cutRunes(trimmed, compactMaxCustomRunes)+"\n") {
			t.Fatalf("output must end with the user's text %q:\n%s", trimmed, rest)
		}
		if len(names) > 0 && !strings.Contains(rest, "\n"+safeInline(names[0])+"\n") {
			t.Fatalf("the first name %q must be on its own line:\n%s", names[0], rest)
		}
	})
}

// The longest inputs still leave the output below the cut: the user's
// text is cut and marked, and the list of names is cut and marked.
func TestCompactInstructionsLongInput(t *testing.T) {
	names := make([]string, 600)
	for i := range names {
		names[i] = "decisions." + strings.Repeat("n", 40) + strconv.Itoa(i)
	}
	out := compactInstructions(strings.Repeat("😀", 3*compactMaxCustomRunes), names)
	if n := utf16Len(out); n >= 10000 {
		t.Fatalf("output is %d UTF-16 units, Claude Code cuts at 10000", n)
	}
	for _, mark := range []string{"[…truncated]", "(the list is cut here)", names[0] + "\n"} {
		if !strings.Contains(out, mark) {
			t.Errorf("output lacks %q", mark)
		}
	}
	if strings.Contains(out, names[len(names)-1]) {
		t.Errorf("600 names cannot fit; the last name must be cut")
	}
}

// Regression: Claude Code 2.1.325 sent compact_summary with an <analysis>
// block and <summary> tags, and the hook stored the scratch work too.
func TestCompactSummaryText(t *testing.T) {
	raw := "<analysis>\nShort session. Scratch work of the model.\n</analysis>\n\n" +
		"<summary>\n## Task\nUser asked to record two facts.\n\n## Notes\n- Keyword: PINEAPPLE.\n\nPINEAPPLE\n</summary>\n"
	want := "## Task\nUser asked to record two facts.\n\n## Notes\n- Keyword: PINEAPPLE.\n\nPINEAPPLE"
	if got := compactSummaryText(raw); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	cases := map[string]string{
		"<analysis>scratch</analysis>\n## Task\nno tags": "## Task\nno tags",
		"<analysis>only scratch</analysis>":              "",
		"<summary></summary>":                            "",
		"<summary>a</summary> and <summary>b</summary>":  "a</summary> and <summary>b",
	}
	for in, want := range cases {
		if got := compactSummaryText(in); got != want {
			t.Errorf("compactSummaryText(%q) = %q, want %q", in, got, want)
		}
	}
	// Text without the tags comes through whole.
	rapid.Check(t, func(t *rapid.T) {
		text := rapid.String().Draw(t, "text")
		if strings.Contains(text, "<analysis>") || strings.Contains(text, "<summary>") {
			t.Skip("has a tag")
		}
		if got := compactSummaryText(text); got != strings.TrimSpace(text) {
			t.Fatalf("compactSummaryText(%q) = %q", text, got)
		}
	})
}

const compactSummaryExample = `Text before the first heading.

## Session
The task. What is done.

## Decisions
### port_choice
Use 4242, because 8080 was taken.
### Embed Model
Keep qwen3.

## Gotchas
### idle_exit
The daemon stops after 30 minutes.

## Todo
A todo line with no topic.
### ship_it
Tag the release.

## Questions

## Notes
### how_recall_works
#### detail
The hook searches with the prompt.

## Files
### main
Not an area of the hook.
`

func TestSplitCompactSummary(t *testing.T) {
	session, memories := splitCompactSummary(compactSummaryExample)
	want := []Section{
		{"decisions.port_choice", "Use 4242, because 8080 was taken."},
		{"decisions.embed_model", "Keep qwen3."},
		{"gotchas.idle_exit", "The daemon stops after 30 minutes."},
		{"todo.ship_it", "Tag the release."},
		{"notes.how_recall_works.detail", "The hook searches with the prompt."},
	}
	if len(memories) != len(want) {
		t.Fatalf("got %d memories, want %d: %v", len(memories), len(want), memories)
	}
	for i, w := range want {
		if memories[i] != w {
			t.Errorf("memory %d: got %+v, want %+v", i, memories[i], w)
		}
	}
	wantSession := "Text before the first heading.\n\n" +
		"## session\nThe task. What is done.\n\n" +
		"## todo\nA todo line with no topic.\n\n" +
		"## files.main\nNot an area of the hook."
	if session != wantSession {
		t.Fatalf("session:\n%s\nwant:\n%s", session, wantSession)
	}

	// For any text: each memory lies below an area, so the hook never
	// writes an area keypath such as `decisions` or `todo` itself, and no
	// text is lost: each section is a memory or a part of the session.
	rapid.Check(t, func(t *rapid.T) {
		lines := rapid.SliceOf(rapid.OneOf(
			rapid.SampledFrom([]string{"## Decisions", "## Todo", "## Notes", "## Session", "## Gotchas",
				"### topic_a", "### Topic B", "#### deep", "# Title", "```", ""}),
			rapid.StringMatching(`[a-z ]{1,20}`),
		)).Draw(t, "lines")
		text := strings.Join(lines, "\n")
		session, memories := splitCompactSummary(text)
		for _, m := range memories {
			area, topic, found := strings.Cut(m.Keypath, ".")
			if _, ok := compactAreas[area]; !ok || !found || topic == "" {
				t.Fatalf("memory at %q is not below an area", m.Keypath)
			}
			if strings.TrimSpace(m.Content) == "" {
				t.Fatalf("memory at %q is empty", m.Keypath)
			}
		}
		for _, s := range ExtractHeadings(text, "") {
			if _, ok := compactArea(s.Keypath); !ok && !strings.Contains(session, s.Content) {
				t.Fatalf("section %q is in no memory and not in the session text:\n%s", s.Keypath, session)
			}
		}
	})
}

func TestCompactSource(t *testing.T) {
	cases := []struct {
		ev   compactEvent
		want string
	}{
		{compactEvent{Trigger: "manual", SessionID: "abc-123"}, "claude-code manual compact, session abc-123"},
		{compactEvent{Trigger: "auto"}, "claude-code auto compact"},
		{compactEvent{Trigger: "x\ny", SessionID: "a\nb c"}, "claude-code compact, session abc"},
		{compactEvent{SessionID: strings.Repeat("s", 300)}, "claude-code compact, session " + strings.Repeat("s", compactMaxSessionID)},
	}
	for _, c := range cases {
		if got := compactSource(c.ev); got != c.want {
			t.Errorf("compactSource(%+v) = %q, want %q", c.ev, got, c.want)
		}
	}
}

// The address rule of the proxy: MEMSTATE_ADDR, then a live daemon.addr,
// then the default address, which a custom database does not get.
func TestSharedDaemonAddr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	t.Setenv("MEMSTATE_ADDR", "127.0.0.1:1234")
	t.Setenv("MEMSTATE_DB", filepath.Join(home, "custom.db"))
	if addr, err := sharedDaemonAddr(); err != nil || addr != "127.0.0.1:1234" {
		t.Fatalf("MEMSTATE_ADDR: got %q, %v", addr, err)
	}
	t.Setenv("MEMSTATE_ADDR", "")
	if addr, err := sharedDaemonAddr(); err == nil || !strings.Contains(err.Error(), "MEMSTATE_DB") {
		t.Fatalf("custom database with no published daemon: got %q, %v", addr, err)
	}
	t.Setenv("MEMSTATE_DB", "")
	if addr, err := sharedDaemonAddr(); err != nil || addr != defaultAddr {
		t.Fatalf("default database: got %q, %v", addr, err)
	}
}

func TestEnsureSharedDaemon(t *testing.T) {
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "t.db"))
	var started []string
	var startErr error
	startDaemon = func(exe, addr string, prev *healthResponse) error {
		if prev != nil {
			t.Errorf("a fresh start takes no report of an old daemon")
		}
		started = append(started, addr)
		return startErr
	}
	t.Cleanup(func() { startDaemon = startDetachedDaemon })

	// A daemon answers: no start.
	ts := newTestServer(t)
	live := strings.TrimPrefix(ts.URL, "http://")
	t.Setenv("MEMSTATE_ADDR", live)
	if addr, err := ensureSharedDaemon(); err != nil || addr != live || len(started) != 0 {
		t.Fatalf("live daemon: got %q, %v, starts %v", addr, err, started)
	}

	// Nothing answers at an address of this machine: one start there.
	free := reserveAndRelease(t)
	t.Setenv("MEMSTATE_ADDR", free)
	if addr, err := ensureSharedDaemon(); err != nil || addr != free || len(started) != 1 || started[0] != free {
		t.Fatalf("no daemon: got %q, %v, starts %v", addr, err, started)
	}
	startErr = errors.New("port taken")
	if _, err := ensureSharedDaemon(); err == nil || !strings.Contains(err.Error(), "could not start the shared daemon: port taken") {
		t.Fatalf("failed start: got %v", err)
	}

	// An address of another machine (TEST-NET-1): no start from here.
	started = nil
	t.Setenv("MEMSTATE_ADDR", "192.0.2.1:8765")
	if _, err := ensureSharedDaemon(); err == nil || !strings.Contains(err.Error(), "not on this machine") || len(started) != 0 {
		t.Fatalf("remote address: got %v, starts %v", err, started)
	}
}

func TestCompactHookEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMSTATE_DB", filepath.Join(dir, "t.db"))
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	noDaemonStart(t)
	ts := newTestServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")
	t.Setenv("MEMSTATE_ADDR", addr)
	day := time.Date(2026, 11, 15, 9, 30, 0, 0, time.Local)
	compactNow = func() time.Time { return day }
	t.Cleanup(func() { compactNow = time.Now })
	const sessionKey = "task.compact.2026_11_15"

	mkdir := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	repo := mkdir("Compact-Repo")
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v %s", err, out)
	}
	plain := mkdir("plain-dir")

	// run returns the exit code, stdout and stderr of one hook event.
	run := func(event string) (int, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := runCompactHook(strings.NewReader(event), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	pre := func(cwd, extra string) string {
		return `{"session_id":"sess-1","cwd":` + jsonString(cwd) + `,"hook_event_name":"PreCompact"` + extra + `}`
	}
	post := func(cwd, summary, extra string) string {
		return `{"session_id":"sess-1","cwd":` + jsonString(cwd) +
			`,"hook_event_name":"PostCompact","trigger":"manual","compact_summary":` +
			jsonString(summary) + extra + `}`
	}
	// versionsAt returns the stored versions of a keypath, newest first.
	versionsAt := func(base, project, keypath string) []map[string]any {
		t.Helper()
		_, body := postJSON(t, base+"/api/v1/memories/history",
			map[string]any{"project_id": project, "keypath": keypath})
		list, _ := body["versions"].([]any)
		out := make([]map[string]any, len(list))
		for i, v := range list {
			out[i] = v.(map[string]any)
		}
		return out
	}
	versions := func(project, keypath string) []map[string]any {
		t.Helper()
		return versionsAt(ts.URL, project, keypath)
	}
	projectExists := func(project string) bool {
		t.Helper()
		ok, err := recallProjectExists(addr, project)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// PreCompact: the instructions, with the user's text. No write, and
	// no names for a project that holds nothing.
	code, out, errOut := run(pre(repo, `,"trigger":"manual","custom_instructions":"keep the embed work"`))
	if code != 0 || errOut != "" || !strings.HasPrefix(out, compactInstructionsText) ||
		!strings.HasSuffix(out, "keep the embed work\n") || strings.Contains(out, "Keypaths that memstate holds") {
		t.Fatalf("PreCompact: code %d stderr %q stdout:\n%s", code, errOut, out)
	}
	// An auto compact sends null for the instructions.
	code, out, _ = run(pre(repo, `,"trigger":"auto","custom_instructions":null`))
	if code != 0 || out != compactInstructionsText {
		t.Fatalf("PreCompact auto: code %d stdout:\n%s", code, out)
	}
	if projectExists("compact_repo") {
		t.Fatal("PreCompact must not write")
	}

	// PostCompact in a git repository: each section of an area is one
	// memory, the rest is the session text, and the write creates the
	// cwd project. A memory that exists under a name is superseded.
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "compact_repo", "keypath": "decisions.port_choice", "content": "Use 8080."})
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "compact_repo", "keypath": "decisions", "content": "the area itself"})
	code, out, errOut = run(post(repo, "<analysis>scratch</analysis>\n<summary>\n"+compactSummaryExample+"</summary>\n", ""))
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("PostCompact: code %d stdout %q stderr %q", code, out, errOut)
	}
	for keypath, want := range map[string][2]string{
		"decisions.embed_model":         {"Keep qwen3.", "decision"},
		"gotchas.idle_exit":             {"The daemon stops after 30 minutes.", "gotcha"},
		"todo.ship_it":                  {"Tag the release.", "status"},
		"notes.how_recall_works.detail": {"The hook searches with the prompt.", "note"},
	} {
		vs := versions("compact_repo", keypath)
		if len(vs) != 1 || vs[0]["content"] != want[0] || vs[0]["category"] != want[1] ||
			vs[0]["source"] != "claude-code manual compact, session sess-1" {
			t.Fatalf("%s: %v", keypath, vs)
		}
		if topics, _ := vs[0]["topics"].([]any); len(topics) != 1 || topics[0] != compactTopic {
			t.Fatalf("%s topics: %v", keypath, vs[0]["topics"])
		}
	}
	if vs := versions("compact_repo", "decisions.port_choice"); len(vs) != 2 ||
		vs[0]["content"] != "Use 4242, because 8080 was taken." {
		t.Fatalf("an existing keypath must get a new version: %v", vs)
	}
	if vs := versions("compact_repo", "decisions"); len(vs) != 1 || vs[0]["content"] != "the area itself" {
		t.Fatalf("the hook wrote the area keypath decisions: %v", vs)
	}
	for _, keypath := range []string{"todo", "files.main", "session"} {
		if vs := versions("compact_repo", keypath); len(vs) != 0 {
			t.Fatalf("the hook wrote %s: %v", keypath, vs)
		}
	}
	vs := versions("compact_repo", sessionKey)
	wantSession := "Text before the first heading.\n\n## session\nThe task. What is done.\n\n" +
		"## todo\nA todo line with no topic.\n\n## files.main\nNot an area of the hook.\n\n" +
		"## filed\n- decisions.port_choice\n- decisions.embed_model\n- gotchas.idle_exit\n- todo.ship_it\n- notes.how_recall_works.detail"
	if len(vs) != 1 || vs[0]["content"] != wantSession || vs[0]["category"] != compactSessionCat {
		t.Fatalf("session memory:\n%v\nwant content:\n%s", vs, wantSession)
	}

	// PreCompact now names the keypaths that the model can update, and
	// not the area keypath or the session text.
	_, out, _ = run(pre(repo, ""))
	wantNames := "\nKeypaths that memstate holds for this project:\n" +
		"decisions.embed_model\ndecisions.port_choice\ngotchas.idle_exit\nnotes.how_recall_works.detail\ntodo.ship_it\n"
	if out != compactInstructionsText+wantNames {
		t.Fatalf("PreCompact names:\n%s", strings.TrimPrefix(out, compactInstructionsText))
	}

	// A second compact on the same day supersedes the session text. A
	// summary with no sections is session text only.
	if code, _, errOut = run(post(repo, "second summary", "")); code != 0 {
		t.Fatalf("second PostCompact: code %d stderr %q", code, errOut)
	}
	if vs = versions("compact_repo", sessionKey); len(vs) != 2 || vs[0]["content"] != "second summary" {
		t.Fatalf("second compact: %v", vs)
	}

	// A section that the daemon refuses does not stop the others, and
	// the hook reports it.
	long := strings.Repeat("x", maxKeypathBytes)
	code, _, errOut = run(post(repo, "## Notes\n### "+long+"\nbody\n### kept\nbody", ""))
	if code != 2 || !strings.Contains(errOut, "compact summary not stored: 1 of 3 memories: notes."+long) {
		t.Fatalf("refused section: code %d stderr %q", code, errOut)
	}
	if vs = versions("compact_repo", "notes.kept"); len(vs) != 1 {
		t.Fatalf("the section after a refused one was not stored: %v", vs)
	}
	if vs = versions("compact_repo", sessionKey); len(vs) != 3 || vs[0]["content"] != "## filed\n- notes.kept" {
		t.Fatalf("session text after a refused section: %v", vs)
	}

	// These events store nothing and report nothing: a subagent's
	// compact, an empty summary, an event of another kind, input that is
	// not a hook event.
	for name, event := range map[string]string{
		"subagent":      post(repo, "subagent summary", `,"agent_id":"a1","agent_type":"Explore"`),
		"empty summary": post(repo, "  \n", ""),
		"only analysis": post(repo, "<analysis>scratch</analysis>", ""),
		"other event":   `{"cwd":` + jsonString(repo) + `,"hook_event_name":"Stop","compact_summary":"x"}`,
		"empty object":  `{}`,
		"not json":      `not json`,
	} {
		if code, out, errOut = run(event); code != 0 || out != "" || errOut != "" {
			t.Fatalf("%s: code %d stdout %q stderr %q", name, code, out, errOut)
		}
	}
	if vs = versions("compact_repo", sessionKey); len(vs) != 3 {
		t.Fatalf("an ignored event wrote a version: %d versions", len(vs))
	}
	// A subagent's PreCompact gets no instructions.
	if code, out, _ = run(pre(repo, `,"agent_id":"a1"`)); code != 0 || out != "" {
		t.Fatalf("subagent PreCompact: code %d stdout %q", code, out)
	}

	// Outside a repository a write does not create a project.
	code, _, errOut = run(post(plain, "plain summary", ""))
	if code != 2 || !strings.Contains(errOut, "compact summary not stored") ||
		!strings.Contains(errOut, `"plain_dir" does not exist`) {
		t.Fatalf("plain dir, no project: code %d stderr %q", code, errOut)
	}
	if projectExists("plain_dir") {
		t.Fatal("the hook created a project outside a repository")
	}
	// An existing project of a plain directory gets the summary.
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "plain_dir", "keypath": "notes.a", "content": "x"})
	if code, _, errOut = run(post(plain, "plain summary", "")); code != 0 {
		t.Fatalf("plain dir, project exists: code %d stderr %q", code, errOut)
	}
	if vs = versions("plain_dir", sessionKey); len(vs) != 1 {
		t.Fatalf("plain dir: want 1 version, got %d", len(vs))
	}

	// The home directory has no project, also when a project with its
	// name exists.
	postJSON(t, ts.URL+"/api/v1/memories/store", map[string]any{
		"project_id": "home", "keypath": "notes.a", "content": "x"})
	code, _, errOut = run(post(home, "home summary", ""))
	if code != 2 || !strings.Contains(errOut, "home directory has no project") {
		t.Fatalf("home dir: code %d stderr %q", code, errOut)
	}
	if vs = versions("home", sessionKey); len(vs) != 0 {
		t.Fatalf("home dir: the hook wrote %d versions", len(vs))
	}
	// PreCompact there still prints the instructions.
	if code, out, _ = run(pre(home, "")); code != 0 || out != compactInstructionsText {
		t.Fatalf("home dir PreCompact: code %d stdout:\n%s", code, out)
	}

	// A live proxy pinned another project for the repository: the pin
	// gets the summary when it exists, and nothing is written when not.
	pins := filepath.Join(dir, "recall", "pins")
	if err := os.MkdirAll(pins, 0o755); err != nil {
		t.Fatal(err)
	}
	pin := func(project string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pins, strconv.Itoa(os.Getpid())),
			[]byte(repo+"\n"+project+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pin("no_such_project")
	code, _, errOut = run(post(repo, "pinned summary", ""))
	if code != 2 || !strings.Contains(errOut, `pinned project "no_such_project" does not exist`) {
		t.Fatalf("missing pin: code %d stderr %q", code, errOut)
	}
	pin("plain_dir")
	if code, _, errOut = run(post(repo, "pinned summary", "")); code != 0 {
		t.Fatalf("pin: code %d stderr %q", code, errOut)
	}
	if vs = versions("plain_dir", sessionKey); len(vs) != 2 {
		t.Fatalf("pin: want 2 versions in plain_dir, got %d", len(vs))
	}
	if vs = versions("compact_repo", sessionKey); len(vs) != 3 {
		t.Fatalf("pin: the cwd project changed, %d versions", len(vs))
	}
	if err := os.Remove(filepath.Join(pins, strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}

	// No daemon answers and the start fails: PostCompact reports it,
	// PreCompact still prints.
	down := reserveAndRelease(t)
	t.Setenv("MEMSTATE_ADDR", down)
	startDaemon = func(exe, addr string, prev *healthResponse) error { return errors.New("no binary") }
	code, _, errOut = run(post(repo, "lost summary", ""))
	if code != 2 || !strings.Contains(errOut, "compact summary not stored: could not start the shared daemon: no binary") {
		t.Fatalf("no daemon: code %d stderr %q", code, errOut)
	}
	if code, out, _ = run(pre(repo, "")); code != 0 || out != compactInstructionsText {
		t.Fatalf("no daemon PreCompact: code %d stdout:\n%s", code, out)
	}

	// No daemon answers and the start works: the hook stores the summary
	// in the daemon that it started.
	var second *httptest.Server
	startDaemon = func(exe, addr string, prev *healthResponse) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		second = httptest.NewUnstartedServer(newRouter(newTestStore(t), nil, nil))
		second.Listener.Close()
		second.Listener = ln
		second.Start()
		t.Cleanup(second.Close)
		return nil
	}
	if code, _, errOut = run(post(repo, "## Decisions\n### after_start\nstored", "")); code != 0 {
		t.Fatalf("started daemon: code %d stderr %q", code, errOut)
	}
	if second == nil {
		t.Fatal("the hook did not start a daemon")
	}
	if vs = versionsAt(second.URL, "compact_repo", "decisions.after_start"); len(vs) != 1 || vs[0]["content"] != "stored" {
		t.Fatalf("started daemon: %v", vs)
	}
}
