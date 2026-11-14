package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"pgregory.net/rapid"
)

// The tests drive the model with messages. The store is a real SQLite
// file and the daemon is the real router; only the editor process is
// replaced, by a function that writes the temp file.

// uiT is what the helpers need from a test. *testing.T and *rapid.T both
// have it.
type uiT interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// uiKey builds the key message whose name is s, and checks the name.
func uiKey(t uiT, s string) tea.KeyPressMsg {
	t.Helper()
	special := map[string]tea.KeyPressMsg{
		"enter":     {Code: tea.KeyEnter},
		"tab":       {Code: tea.KeyTab},
		"shift+tab": {Code: tea.KeyTab, Mod: tea.ModShift},
		"esc":       {Code: tea.KeyEscape},
		"up":        {Code: tea.KeyUp},
		"down":      {Code: tea.KeyDown},
		"left":      {Code: tea.KeyLeft},
		"right":     {Code: tea.KeyRight},
		"pgup":      {Code: tea.KeyPgUp},
		"pgdown":    {Code: tea.KeyPgDown},
		"backspace": {Code: tea.KeyBackspace},
		"space":     {Code: tea.KeySpace, Text: " "},
		"ctrl+u":    {Code: 'u', Mod: tea.ModCtrl},
	}
	k, ok := special[s]
	if !ok {
		r := []rune(s)
		if len(r) != 1 {
			t.Fatalf("uiKey: no key named %q", s)
		}
		k = tea.KeyPressMsg{Code: r[0], Text: s}
	}
	if k.String() != s {
		t.Fatalf("uiKey: key for %q has the name %q", s, k.String())
	}
	return k
}

// uiPump runs cmd and each command that follows from it, until none is
// left.
func uiPump(t uiT, m *uiModel, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 500 {
			t.Fatal("the message loop does not end")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, tea.QuitMsg:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func uiSend(t uiT, m *uiModel, msg tea.Msg) {
	t.Helper()
	_, cmd := m.Update(msg)
	uiPump(t, m, cmd)
}

func uiKeys(t uiT, m *uiModel, keys ...string) {
	t.Helper()
	for _, k := range keys {
		uiSend(t, m, uiKey(t, k))
	}
}

// uiType types text into the footer input.
func uiType(t uiT, m *uiModel, text string) {
	t.Helper()
	for _, r := range text {
		if r == ' ' {
			uiSend(t, m, uiKey(t, "space"))
			continue
		}
		uiSend(t, m, uiKey(t, string(r)))
	}
}

var sgrRe = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// uiScreenText is the rendered screen without the style sequences.
func uiScreenText(m *uiModel) string {
	return sgrRe.ReplaceAllString(m.View().Content, "")
}

// uiSelect puts the keypath cursor on path.
func uiSelect(t uiT, m *uiModel, path string) {
	t.Helper()
	i := slices.IndexFunc(m.nodes, func(n uiNode) bool { return n.path == path })
	if i < 0 {
		t.Fatalf("no visible node %q in %v", path, m.nodes)
	}
	m.keyCur = i
	m.focus = paneKeys
	m.refreshDetail()
}

type uiFixture struct {
	m     *uiModel
	store *Store
	// edit is what the editor does to the temp file.
	edit func(path string) error
}

// newUIFixture opens the seeded store, starts the daemon router on it and
// loads the model for project "proj" in a 120x40 window.
func newUIFixture(t *testing.T) *uiFixture {
	t.Helper()
	t.Setenv("MEMSTATE_ADDR", "")
	t.Setenv("MEMSTATE_CONFIG", "off")
	t.Setenv("MEMSTATE_DB", filepath.Join(t.TempDir(), "none.db"))
	dbPath := seedCLIStore(t)
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ts := httptest.NewServer(newRouter(store, nil, nil))
	t.Cleanup(ts.Close)

	f := &uiFixture{store: store, edit: func(string) error { return nil }}
	b := &uiBackend{store: store, dbPath: dbPath, addrFlag: strings.TrimPrefix(ts.URL, "http://"), host: hostSlug()}
	f.m = newUIModel(b, "proj", palette{})
	f.m.runEditor = func(path string, done func(error) tea.Msg) tea.Cmd {
		return func() tea.Msg { return done(f.edit(path)) }
	}
	uiSend(t, f.m, tea.WindowSizeMsg{Width: 120, Height: 40})
	uiPump(t, f.m, f.m.Init())
	return f
}

func (f *uiFixture) latest(t *testing.T, project, keypath string) *Memory {
	t.Helper()
	mem, err := f.store.GetLatest(project, keypath)
	if err != nil {
		t.Fatal(err)
	}
	return mem
}

// memoryRows is the raw memories table, to prove that nothing changed.
func (f *uiFixture) memoryRows(t *testing.T) [][]string {
	t.Helper()
	_, rows, err := f.store.RawRows("memories", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func writeFileEditor(content string) func(string) error {
	return func(path string) error { return os.WriteFile(path, []byte(content), 0o600) }
}

func TestUIBrowseAndHistory(t *testing.T) {
	f := newUIFixture(t)
	m := f.m

	var paths []string
	for _, n := range m.nodes {
		paths = append(paths, n.path)
	}
	if got := strings.Join(paths, " "); got != "decisions decisions.db todo todo.a todo.b" {
		t.Fatalf("tree lines: %s", got)
	}
	if m.project != "proj" || len(m.visibleProjects()) != 2 || m.visibleProjects()[0].ID != sharedProject {
		t.Fatalf("projects: on %q, list %v", m.project, m.visibleProjects())
	}

	uiSelect(t, m, "decisions.db")
	screen := uiScreenText(m)
	for _, want := range []string{"sqlite over postgres, WAL mode", "decisions.db", "v2", "projects (2)"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("screen lacks %q:\n%s", want, screen)
		}
	}

	// A fold hides the lines below a branch; the same key opens it.
	uiSelect(t, m, "todo")
	uiKeys(t, m, "enter")
	if len(m.nodes) != 3 || m.selectedPath() != "todo" {
		t.Fatalf("after fold: %v, cursor on %q", m.nodes, m.selectedPath())
	}
	uiKeys(t, m, "space")
	if len(m.nodes) != 5 {
		t.Fatalf("after open: %v", m.nodes)
	}

	// The filter keeps each match and its parents.
	uiKeys(t, m, "/")
	uiType(t, m, "todo.b")
	uiKeys(t, m, "enter")
	if len(m.nodes) != 2 || m.nodes[1].path != "todo.b" {
		t.Fatalf("filtered tree: %v", m.nodes)
	}
	uiKeys(t, m, "esc")
	if len(m.nodes) != 5 {
		t.Fatalf("after the filter is cleared: %v", m.nodes)
	}

	// History shows each version, newest first.
	uiSelect(t, m, "decisions.db")
	uiKeys(t, m, "v")
	if !m.showHistory || len(m.history) != 2 || m.history[0].Version != 2 {
		t.Fatalf("history: show=%v versions=%v", m.showHistory, m.history)
	}
	uiKeys(t, m, "j")
	screen = uiScreenText(m)
	if !strings.Contains(screen, "> v1 ·") || !strings.Contains(screen, "sqlite over postgres") {
		t.Fatalf("history screen:\n%s", screen)
	}
}

func TestUIEdit(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	uiSelect(t, m, "decisions.db")

	// The editor gets the current content. Category and topics stay.
	var got string
	f.edit = func(path string) error {
		b, err := os.ReadFile(path)
		got = string(b)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte("sqlite, WAL, four connections"), 0o600)
	}
	uiKeys(t, m, "e")
	if got != "sqlite over postgres, WAL mode" {
		t.Fatalf("editor got %q", got)
	}
	mem := f.latest(t, "proj", "decisions.db")
	if mem.Version != 3 || mem.Content != "sqlite, WAL, four connections" || mem.Category != "decision" || mem.Source != uiSource {
		t.Fatalf("after edit: %+v", mem)
	}
	if m.statusErr || !strings.Contains(m.status, "superseded") || m.selectedPath() != "decisions.db" {
		t.Fatalf("status %q (err=%v), cursor on %q", m.status, m.statusErr, m.selectedPath())
	}
	if m.mems["decisions.db"].Version != 3 {
		t.Fatalf("the model did not reload: %+v", m.mems["decisions.db"])
	}

	// An editor that changes nothing stores nothing.
	f.edit = func(string) error { return nil }
	uiKeys(t, m, "e")
	if v := f.latest(t, "proj", "decisions.db").Version; v != 3 || !strings.Contains(m.status, "no change") {
		t.Fatalf("no-change edit: version %d, status %q", v, m.status)
	}

	// Empty content is refused; a delete is a different key.
	f.edit = writeFileEditor("  \n")
	uiKeys(t, m, "e")
	if v := f.latest(t, "proj", "decisions.db").Version; v != 3 || !m.statusErr {
		t.Fatalf("empty edit: version %d, status %q (err=%v)", v, m.status, m.statusErr)
	}

	// A branch has no value to edit.
	uiSelect(t, m, "todo")
	uiKeys(t, m, "e")
	if !m.statusErr {
		t.Fatalf("edit on a branch: status %q", m.status)
	}
}

func TestUIMeta(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	uiSelect(t, m, "decisions.db")

	uiKeys(t, m, "m")
	if m.form == nil || m.input.Value() != "decision" {
		t.Fatalf("meta form: %+v, input %q", m.form, m.input.Value())
	}
	uiKeys(t, m, "ctrl+u")
	uiType(t, m, "note")
	uiKeys(t, m, "enter")
	uiType(t, m, "db, storage")
	uiKeys(t, m, "enter")

	mem := f.latest(t, "proj", "decisions.db")
	if mem.Version != 3 || mem.Content != "sqlite over postgres, WAL mode" || mem.Category != "note" ||
		!slices.Equal(mem.Topics, []string{"db", "storage"}) {
		t.Fatalf("after meta: %+v", mem)
	}
}

func TestUINew(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	uiSelect(t, m, "todo.a")
	f.edit = writeFileEditor("fresh content")

	uiKeys(t, m, "n")
	if m.form == nil || m.input.Value() != "todo." {
		t.Fatalf("new form: %+v, input %q", m.form, m.input.Value())
	}
	uiKeys(t, m, "ctrl+u")
	uiType(t, m, "notes.fresh")
	uiKeys(t, m, "enter")
	uiType(t, m, "note")
	uiKeys(t, m, "enter", "enter")

	mem := f.latest(t, "proj", "notes.fresh")
	if mem == nil || mem.Version != 1 || mem.Content != "fresh content" || mem.Category != "note" {
		t.Fatalf("after new: %+v (status %q)", mem, m.status)
	}
	if m.selectedPath() != "notes.fresh" {
		t.Fatalf("cursor on %q", m.selectedPath())
	}

	// n does not overwrite a keypath that has a value.
	before := f.memoryRows(t)
	uiKeys(t, m, "n", "ctrl+u")
	uiType(t, m, "todo.a")
	uiKeys(t, m, "enter", "enter", "enter")
	if !m.statusErr || !strings.Contains(m.status, "has a value") || !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
		t.Fatalf("new on an existing keypath: status %q (err=%v)", m.status, m.statusErr)
	}

	// esc leaves the form and changes nothing.
	uiKeys(t, m, "n", "esc")
	if m.form != nil || !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
		t.Fatal("esc did not cancel the form")
	}
}

// `h` and `l` move between the panes, as the left key and the right key
// do. `v` shows the versions; `h` had that job before.
func TestUIPaneKeys(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	uiSelect(t, m, "decisions.db")
	m.focus = paneProjects

	for _, want := range []int{paneKeys, paneDetail, paneDetail} {
		uiKeys(t, m, "l")
		if m.focus != want {
			t.Fatalf("after l: pane %d, want %d", m.focus, want)
		}
	}
	for _, want := range []int{paneKeys, paneProjects, paneProjects} {
		uiKeys(t, m, "h")
		if m.focus != want || m.showHistory {
			t.Fatalf("after h: pane %d, want %d (history=%v)", m.focus, want, m.showHistory)
		}
	}

	uiKeys(t, m, "l", "v")
	if !m.showHistory || len(m.history) != 2 || m.focus != paneDetail {
		t.Fatalf("v: history=%v versions=%d pane %d", m.showHistory, len(m.history), m.focus)
	}
	uiKeys(t, m, "v")
	if m.showHistory {
		t.Fatal("the second v did not close the versions")
	}

	// The list of search matches has two panes.
	uiKeys(t, m, "s")
	uiType(t, m, "sqlite")
	uiKeys(t, m, "enter")
	if m.search == nil || len(m.search.hits) == 0 {
		t.Fatalf("search: %+v, status %q", m.search, m.status)
	}
	m.focus = paneKeys
	uiKeys(t, m, "l")
	if m.focus != paneDetail || m.search == nil {
		t.Fatalf("l in the match list: pane %d", m.focus)
	}
	uiKeys(t, m, "h")
	if m.focus != paneKeys || m.search == nil {
		t.Fatalf("h in the match list: pane %d", m.focus)
	}
}

func TestUIDelete(t *testing.T) {
	f := newUIFixture(t)
	m := f.m

	// A leaf: one keypath, after "y".
	uiSelect(t, m, "todo.a")
	uiKeys(t, m, "d")
	if m.form == nil || !strings.Contains(uiScreenText(m), "todo.a") {
		t.Fatalf("delete form: %+v", m.form)
	}
	uiKeys(t, m, "y", "enter")
	if mem := f.latest(t, "proj", "todo.a"); !mem.Tombstone {
		t.Fatalf("todo.a is not deleted: %+v", mem)
	}
	if _, ok := m.mems["todo.a"]; ok || m.statusErr {
		t.Fatalf("model after delete: status %q (err=%v)", m.status, m.statusErr)
	}

	// A branch: the answer "n" deletes nothing.
	uiSelect(t, m, "todo")
	uiKeys(t, m, "d")
	if m.form == nil || !slices.Contains(m.form.lines, "  todo.b") {
		t.Fatalf("branch delete form: %+v", m.form)
	}
	uiKeys(t, m, "n", "enter")
	if mem := f.latest(t, "proj", "todo.b"); mem.Tombstone || !m.statusErr {
		t.Fatalf("an unconfirmed delete ran: %+v, status %q", mem, m.status)
	}

	// The branch again, confirmed: each keypath below gets a tombstone.
	uiKeys(t, m, "d", "y", "enter")
	if mem := f.latest(t, "proj", "todo.b"); !mem.Tombstone {
		t.Fatalf("todo.b is not deleted: %+v", mem)
	}
	if slices.ContainsFunc(m.nodes, func(n uiNode) bool { return strings.HasPrefix(n.path, "todo") }) {
		t.Fatalf("the tree still shows todo: %v", m.nodes)
	}
}

// In the shared project the tree shows only this host below "host". A
// delete or a purge on the "host" node acts on what the tree shows. The
// facts of another host are not on the screen and stay.
func TestUISharedHostNode(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	mine, other := "host."+hostSlug()+".env.go_bin", "host.other_box.env.go_bin"
	m.focus = paneProjects
	uiKeys(t, m, "g")
	if m.project != sharedProject {
		t.Fatalf("on project %q", m.project)
	}

	uiSelect(t, m, "host")
	uiKeys(t, m, "d")
	if m.form == nil || !slices.Contains(m.form.lines, "  "+mine) || slices.Contains(m.form.lines, "  "+other) {
		t.Fatalf("delete form on the host node: %+v, status %q", m.form, m.status)
	}
	uiKeys(t, m, "y", "enter")
	if mem := f.latest(t, sharedProject, mine); !mem.Tombstone || m.statusErr {
		t.Fatalf("this host is not deleted: %+v, status %q", mem, m.status)
	}
	if mem := f.latest(t, sharedProject, other); mem.Tombstone {
		t.Fatalf("the delete on the host node took another host: %+v", mem)
	}

	// The same rule for a purge.
	if _, _, err := f.store.Write(sharedProject, mine, "~/go/bin", WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	uiKeys(t, m, "r")
	uiSelect(t, m, "host")
	uiKeys(t, m, "X")
	if m.form == nil || !slices.Contains(m.form.lines, "  "+mine+"  (3 version(s))") ||
		slices.ContainsFunc(m.form.lines, func(l string) bool { return strings.Contains(l, other) }) {
		t.Fatalf("purge form on the host node: %+v, status %q", m.form, m.status)
	}
	uiType(t, m, "purge")
	uiKeys(t, m, "enter")
	if hist, _ := f.store.History(sharedProject, mine); len(hist) != 0 || m.statusErr {
		t.Fatalf("this host is not purged: %+v, status %q", hist, m.status)
	}
	if hist, _ := f.store.History(sharedProject, other); len(hist) != 1 {
		t.Fatalf("the purge on the host node took another host: %+v", hist)
	}
}

func TestUIDeleteProject(t *testing.T) {
	f := newUIFixture(t)
	m := f.m

	// The key line names the action. "D project" did not say that the key
	// deletes.
	uiSend(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if !strings.Contains(uiScreenText(m), "D delete project") {
		t.Fatalf("the key line does not name the delete:\n%s", uiScreenText(m))
	}

	// The wrong id deletes nothing.
	uiKeys(t, m, "D")
	uiType(t, m, "pro")
	uiKeys(t, m, "enter")
	if deleted, _ := f.store.ProjectDeleted("proj"); deleted || !m.statusErr {
		t.Fatalf("a wrong id deleted the project; status %q", m.status)
	}

	uiKeys(t, m, "D")
	uiType(t, m, "proj")
	uiKeys(t, m, "enter")
	if deleted, _ := f.store.ProjectDeleted("proj"); !deleted {
		t.Fatalf("project not deleted; status %q", m.status)
	}
	if m.project != sharedProject || len(m.visibleProjects()) != 1 {
		t.Fatalf("after project delete: on %q, list %v", m.project, m.visibleProjects())
	}
}

// The daemon's rules reach the status line, and a refused write changes
// nothing.
func TestUIDaemonRefusal(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	f.edit = writeFileEditor("not allowed here")
	before := f.memoryRows(t)

	// The shared scope takes only its allowlist; "todo" is not on it.
	m.focus = paneProjects
	uiKeys(t, m, "g")
	if m.project != sharedProject {
		t.Fatalf("on project %q", m.project)
	}
	uiKeys(t, m, "n", "ctrl+u")
	uiType(t, m, "todo.x")
	uiKeys(t, m, "enter", "enter", "enter")
	if !m.statusErr || !strings.Contains(m.status, "HTTP 400") {
		t.Fatalf("shared allowlist: status %q (err=%v)", m.status, m.statusErr)
	}

	// A name outside the alphabet.
	uiKeys(t, m, "n", "ctrl+u")
	uiType(t, m, "preferences.a!b")
	uiKeys(t, m, "enter", "enter", "enter")
	if !m.statusErr || !strings.Contains(m.status, "HTTP 400") {
		t.Fatalf("name rule: status %q (err=%v)", m.status, m.statusErr)
	}
	if !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
		t.Fatal("a refused write changed the store")
	}
}

func TestUIReadOnly(t *testing.T) {
	writeKeys := []string{"n", "e", "m", "d", "D"}
	check := func(t *testing.T, f *uiFixture) {
		t.Helper()
		before := f.memoryRows(t)
		uiSelect(t, f.m, "decisions.db")
		f.edit = writeFileEditor("must not be stored")
		for _, k := range writeKeys {
			uiKeys(t, f.m, k)
			if f.m.form != nil || !f.m.statusErr || !strings.Contains(f.m.status, "read-only") {
				t.Fatalf("key %s: form=%v status %q (err=%v)", k, f.m.form != nil, f.m.status, f.m.statusErr)
			}
		}
		if !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
			t.Fatal("a read-only session changed the store")
		}
		if deleted, _ := f.store.ProjectDeleted("proj"); deleted {
			t.Fatal("a read-only session deleted the project")
		}
	}

	t.Run("db flag without addr", func(t *testing.T) {
		f := newUIFixture(t)
		f.m.b.readOnly = "read-only: test"
		check(t, f)
	})
	t.Run("no daemon", func(t *testing.T) {
		f := newUIFixture(t)
		f.m.b.addrFlag = "" // discovery: no MEMSTATE_ADDR, no daemon.addr file
		check(t, f)
		if !strings.Contains(f.m.status, "no shared daemon") {
			t.Fatalf("status %q", f.m.status)
		}
	})
	t.Run("daemon gone", func(t *testing.T) {
		f := newUIFixture(t)
		f.m.b.addrFlag = "127.0.0.1:1"
		check(t, f)
	})
}

func TestUITables(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	uiKeys(t, m, "2")
	if m.rawTable != "projects" || len(m.rawRows) != 2 {
		t.Fatalf("first table: %q with %d rows", m.rawTable, len(m.rawRows))
	}
	uiKeys(t, m, "j")
	if m.rawTable != "memories" || len(m.rawRows) != 7 {
		t.Fatalf("second table: %q with %d rows", m.rawTable, len(m.rawRows))
	}
	screen := uiScreenText(m)
	for _, want := range []string{"memories (7)", "page 1/1", "read-only", "keypath", "decisions.db"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("tables screen lacks %q:\n%s", want, screen)
		}
	}

	// The row detail shows each column in full.
	uiKeys(t, m, "enter", "j", "enter")
	if !m.rowDetail {
		t.Fatal("enter did not open the row")
	}
	screen = uiScreenText(m)
	for _, want := range []string{"row 2", "content", "sqlite over postgres, WAL mode", "tombstone"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("row detail lacks %q:\n%s", want, screen)
		}
	}
	uiKeys(t, m, "esc")
	if m.rowDetail {
		t.Fatal("esc did not close the row")
	}

	// The right key shows the columns that did not fit; the row cursor
	// stays on its row.
	if m.colOff != 0 || m.colShown >= len(m.rawCols) {
		t.Fatalf("columns at 120 cells: offset %d, %d of %d shown", m.colOff, m.colShown, len(m.rawCols))
	}
	for i := 0; i < 2*len(m.rawCols); i++ {
		uiKeys(t, m, "right")
	}
	if m.colOff+m.colShown != len(m.rawCols) || m.grid.Cursor() != 1 || !strings.Contains(uiScreenText(m), "created_at") {
		t.Fatalf("after right: offset %d, %d shown, row %d", m.colOff, m.colShown, m.grid.Cursor())
	}
	for i := 0; i < 2*len(m.rawCols); i++ {
		uiKeys(t, m, "left")
	}
	if m.colOff != 0 || m.tabFocus != 0 {
		t.Fatalf("after left: offset %d, focus %d", m.colOff, m.tabFocus)
	}

	// `l` and `h` do what the right key and the left key do.
	uiKeys(t, m, "l")
	if m.tabFocus != 1 {
		t.Fatalf("l did not move to the rows: focus %d", m.tabFocus)
	}
	for i := 0; i < 2*len(m.rawCols); i++ {
		uiKeys(t, m, "l")
	}
	if m.colOff+m.colShown != len(m.rawCols) {
		t.Fatalf("after l: offset %d, %d of %d shown", m.colOff, m.colShown, len(m.rawCols))
	}
	for i := 0; i < 2*len(m.rawCols); i++ {
		uiKeys(t, m, "h")
	}
	if m.colOff != 0 || m.tabFocus != 0 {
		t.Fatalf("after h: offset %d, focus %d", m.colOff, m.tabFocus)
	}

	// The write keys do nothing on this screen.
	before := f.memoryRows(t)
	uiKeys(t, m, "d", "e", "n", "m", "D")
	if m.form != nil || !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
		t.Fatal("a write key acted on the tables screen")
	}
}

func TestUITablePaging(t *testing.T) {
	f := newUIFixture(t)
	for i := 0; i < 2*uiRawPage+5; i++ {
		if _, _, err := f.store.Write("bulk", fmt.Sprintf("k%04d", i), "x", WriteMeta{}, false); err != nil {
			t.Fatal(err)
		}
	}
	m := f.m
	uiKeys(t, m, "r", "2", "j", "enter")
	total := m.tableCount("memories")
	if m.rawTable != "memories" || total != 2*uiRawPage+12 || len(m.rawRows) != uiRawPage {
		t.Fatalf("page 1: table %q, %d rows of %d", m.rawTable, len(m.rawRows), total)
	}
	uiKeys(t, m, "pgdown", "pgdown")
	if m.rawPage != 2 || len(m.rawRows) != 12 {
		t.Fatalf("page 3: page %d, %d rows", m.rawPage, len(m.rawRows))
	}
	uiKeys(t, m, "pgdown")
	if m.rawPage != 2 {
		t.Fatalf("pgdown went past the last page: %d", m.rawPage)
	}
	uiKeys(t, m, "pgup", "pgup", "pgup")
	if m.rawPage != 0 || len(m.rawRows) != uiRawPage {
		t.Fatalf("back on page 1: page %d, %d rows", m.rawPage, len(m.rawRows))
	}
}

func TestUISearch(t *testing.T) {
	f := newUIFixture(t)
	m := f.m

	// In the project on screen. The daemon of the fixture has no
	// embedder, so its hybrid search answers with the word matches.
	uiKeys(t, m, "s")
	uiType(t, m, "postgres")
	uiKeys(t, m, "enter")
	if m.search == nil || len(m.search.hits) != 1 || m.search.hits[0].Keypath != "decisions.db" || m.statusErr {
		t.Fatalf("search: %+v, status %q", m.search, m.status)
	}
	screen := uiScreenText(m)
	for _, want := range []string{"search: postgres (1)", "proj:decisions.db", "sqlite over postgres, WAL mode", "1 match(es)"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("search screen lacks %q:\n%s", want, screen)
		}
	}
	// A write key does not act on a match; the list stays open.
	before := f.memoryRows(t)
	uiKeys(t, m, "d", "e", "X")
	if m.search == nil || m.form != nil || !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
		t.Fatal("a write key acted while the search results were open")
	}
	uiKeys(t, m, "enter")
	if m.search != nil || m.selectedPath() != "decisions.db" || m.project != "proj" {
		t.Fatalf("after enter: search open=%v, cursor on %q in %q", m.search != nil, m.selectedPath(), m.project)
	}

	// In each project: enter opens the project of the match.
	uiKeys(t, m, "S")
	uiType(t, m, "subjects")
	uiKeys(t, m, "enter")
	if m.search == nil || len(m.search.hits) != 1 || m.search.hits[0].ProjectID != sharedProject {
		t.Fatalf("search of each project: %+v, status %q", m.search, m.status)
	}
	if !strings.Contains(uiScreenText(m), sharedProject+":preferences.commit_style") {
		t.Fatalf("the list does not name the project:\n%s", uiScreenText(m))
	}
	uiKeys(t, m, "enter")
	if m.project != sharedProject || m.selectedPath() != "preferences.commit_style" {
		t.Fatalf("after enter: on %q, cursor on %q", m.project, m.selectedPath())
	}

	// No match, then esc.
	uiKeys(t, m, "s")
	uiType(t, m, "zzzzqq")
	uiKeys(t, m, "enter")
	if m.search == nil || len(m.search.hits) != 0 || !strings.Contains(uiScreenText(m), "no match for zzzzqq") {
		t.Fatalf("empty search: %+v", m.search)
	}
	uiKeys(t, m, "esc")
	if m.search != nil {
		t.Fatal("esc did not close the list")
	}

	// Without a daemon the word search runs on the store file, and the
	// status line says so. Another host's shared facts stay out.
	m.b.addrFlag = ""
	uiKeys(t, m, "S")
	uiType(t, m, "bin")
	uiKeys(t, m, "enter")
	if m.search == nil || len(m.search.hits) != 1 || !strings.Contains(m.status, "fts only") {
		t.Fatalf("offline search: %+v, status %q", m.search, m.status)
	}
	if kp := m.search.hits[0].Keypath; kp != "host."+hostSlug()+".env.go_bin" {
		t.Fatalf("offline search hit %q", kp)
	}
}

func TestUIPurge(t *testing.T) {
	f := newUIFixture(t)
	m := f.m
	// A purge is direct on the store file. It needs no daemon.
	m.b.readOnly = "read-only: test"

	// The wrong word erases nothing.
	uiSelect(t, m, "decisions.db")
	before := f.memoryRows(t)
	uiKeys(t, m, "X")
	if m.form == nil || !slices.Contains(m.form.lines, "  decisions.db  (2 version(s))") {
		t.Fatalf("purge form: %+v, status %q", m.form, m.status)
	}
	uiType(t, m, "y")
	uiKeys(t, m, "enter")
	if !m.statusErr || !slices.EqualFunc(before, f.memoryRows(t), slices.Equal[[]string]) {
		t.Fatalf("an unconfirmed purge ran; status %q", m.status)
	}

	uiKeys(t, m, "X")
	uiType(t, m, "purge")
	uiKeys(t, m, "enter")
	if hist, _ := f.store.History("proj", "decisions.db"); len(hist) != 0 || m.statusErr {
		t.Fatalf("after purge: history %+v, status %q (err=%v)", hist, m.status, m.statusErr)
	}
	if _, ok := m.mems["decisions.db"]; ok || !strings.Contains(m.status, "purged 1 keypath(s), 2 version(s)") {
		t.Fatalf("model after purge: status %q", m.status)
	}
	for _, row := range f.memoryRows(t) {
		if strings.Contains(strings.Join(row, "|"), "sqlite over postgres") {
			t.Fatalf("purged content is still in the store: %v", row)
		}
	}

	// A branch: each keypath below, a tombstoned one too.
	if _, _, err := f.store.Delete("proj", "todo.a"); err != nil {
		t.Fatal(err)
	}
	uiKeys(t, m, "r")
	uiSelect(t, m, "todo")
	uiKeys(t, m, "X")
	if m.form == nil || !slices.Contains(m.form.lines, "  todo.a  (2 version(s))") || !slices.Contains(m.form.lines, "  todo.b  (1 version(s))") {
		t.Fatalf("branch purge form: %+v", m.form)
	}
	uiType(t, m, "purge")
	uiKeys(t, m, "enter")
	for _, kp := range []string{"todo.a", "todo.b"} {
		if hist, _ := f.store.History("proj", kp); len(hist) != 0 {
			t.Fatalf("%s survived the branch purge: %+v", kp, hist)
		}
	}

	// From the raw memories table: the keypath of the selected row.
	uiKeys(t, m, "2", "j", "enter")
	if m.rawTable != "memories" || len(m.rawRows) == 0 {
		t.Fatalf("raw table %q with %d rows", m.rawTable, len(m.rawRows))
	}
	project, keypath := m.rawRows[0][1], m.rawRows[0][2]
	uiKeys(t, m, "X")
	if m.form == nil || !strings.Contains(uiScreenText(m), keypath) {
		t.Fatalf("raw purge form: %+v\n%s", m.form, uiScreenText(m))
	}
	uiType(t, m, "purge")
	uiKeys(t, m, "enter")
	if hist, _ := f.store.History(project, keypath); len(hist) != 0 {
		t.Fatalf("%s/%s survived the raw purge: %+v", project, keypath, hist)
	}
	// X on a table that has no keypath column is refused.
	uiKeys(t, m, "esc", "g", "enter", "X")
	if m.rawTable != "projects" || m.form != nil || !m.statusErr {
		t.Fatalf("X on %q: form=%v status %q", m.rawTable, m.form != nil, m.status)
	}
}

// Stored text is not terminal input. This content clears the screen and
// sets the window title when a viewer prints it as it is.
func TestUIEscapeSequencesInContent(t *testing.T) {
	const evil = "\x1b[2J\x1b]0;x\x07 after\r\n\ttab \x9b31m \x7f"
	clean := sanitizeTerm(evil)
	if strings.ContainsAny(clean, "\x1b\x07\r\t\x7f") || strings.ContainsRune(clean, 0x9b) {
		t.Fatalf("sanitizeTerm left a control character: %q", clean)
	}
	if !strings.Contains(clean, "␛[2J") || !strings.Contains(clean, "after\n") {
		t.Fatalf("sanitizeTerm lost text: %q", clean)
	}

	f := newUIFixture(t)
	if _, _, err := f.store.Write("proj", "notes.evil", evil, WriteMeta{}, false); err != nil {
		t.Fatal(err)
	}
	m := f.m
	uiKeys(t, m, "r")
	uiSelect(t, m, "notes.evil")
	for _, screen := range []string{uiScreenText(m), func() string {
		uiKeys(t, m, "v")
		return uiScreenText(m)
	}(), func() string {
		uiKeys(t, m, "2", "j", "enter", "G", "enter")
		return uiScreenText(m)
	}()} {
		if !strings.Contains(screen, "after") {
			t.Fatalf("the content is not on the screen:\n%q", screen)
		}
		if i := strings.IndexFunc(screen, isTermControl); i >= 0 {
			t.Fatalf("control character %q on the screen at %d:\n%q", screen[i], i, screen)
		}
	}
}

// isTermControl is true for each control character other than a newline.
func isTermControl(r rune) bool {
	return (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f)
}

func TestUINeedsTerminal(t *testing.T) {
	prev := uiHasTerminal
	uiHasTerminal = func() bool { return false }
	defer func() { uiHasTerminal = prev }()
	code, out, errOut := runCLIErr(t, "ui", "--project", "proj")
	if code != 2 || out != "" || !strings.Contains(errOut, "terminal") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, _ := runCLIErr(t, "ui", "extra"); code != 2 {
		t.Fatalf("a positional argument: exit %d", code)
	}
}

// For each store, each window size and each sequence of keys: Update and
// View do not panic, each cursor is inside its list, the screen fits the
// window, and no control character from the store reaches the screen.
func TestUIKeysProperty(t *testing.T) {
	t.Setenv("MEMSTATE_ADDR", "")
	dir := t.TempDir()
	keys := []string{
		"j", "k", "g", "G", "up", "down", "left", "right", "pgup", "pgdown",
		"tab", "shift+tab", "enter", "space", "esc", "h", "l", "v", "r", "/", "?", "1", "2",
		"n", "e", "m", "d", "D", "a", "o", ".", "backspace", "ctrl+u",
		"s", "S", "X",
	}
	run := 0
	rapid.Check(t, func(rt *rapid.T) {
		run++
		store, err := OpenStore(filepath.Join(dir, fmt.Sprintf("p%d.db", run)))
		if err != nil {
			rt.Fatal(err)
		}
		defer store.Close()
		projects := []string{"alpha", "beta-2", "проект", sharedProject}
		paths := []string{"a", "a.b", "a.b.c", "notes.x", "todo", "日本.語"}
		for i, n := 0, rapid.IntRange(0, 12).Draw(rt, "writes"); i < n; i++ {
			pid := rapid.SampledFrom(projects).Draw(rt, "project")
			kp := rapid.SampledFrom(paths).Draw(rt, "keypath")
			if pid == sharedProject {
				kp = "preferences.p" + fmt.Sprint(i)
			}
			content := rapid.String().Draw(rt, "content")
			tomb := strings.TrimSpace(content) == ""
			if _, _, err := store.Write(pid, kp, content, WriteMeta{Source: rapid.String().Draw(rt, "source")}, tomb); err != nil {
				// The store refuses a source of more than one line.
				continue
			}
		}

		b := &uiBackend{store: store, dbPath: "p.db", readOnly: "read-only: property test", host: hostSlug()}
		m := newUIModel(b, "alpha", palette{on: rapid.Bool().Draw(rt, "color")})
		uiPump(rt, m, m.Init())

		for i, n := 0, rapid.IntRange(1, 60).Draw(rt, "steps"); i < n; i++ {
			if rapid.IntRange(0, 9).Draw(rt, "resize") == 0 {
				uiSend(rt, m, tea.WindowSizeMsg{
					Width:  rapid.IntRange(0, 170).Draw(rt, "width"),
					Height: rapid.IntRange(0, 50).Draw(rt, "height"),
				})
			} else {
				uiSend(rt, m, uiKey(rt, rapid.SampledFrom(keys).Draw(rt, "key")))
			}

			inside := func(name string, cur, n int) {
				if cur < 0 || (n > 0 && cur >= n) || (n == 0 && cur != 0) {
					rt.Fatalf("%s cursor %d outside a list of %d", name, cur, n)
				}
			}
			inside("project", m.projCur, len(m.visibleProjects()))
			inside("keypath", m.keyCur, len(m.nodes))
			inside("history", m.histCur, len(m.history))
			inside("table", m.tabCur, len(m.tables))
			if m.search != nil {
				inside("search", m.search.cur, len(m.search.hits))
			}

			screen := uiScreenText(m)
			if i := strings.IndexFunc(screen, isTermControl); i >= 0 {
				rt.Fatalf("control character %q on the screen:\n%q", screen[i], screen)
			}
			lines := strings.Split(screen, "\n")
			if screen != "" && len(lines) > m.height {
				rt.Fatalf("%d lines in a window of %d lines:\n%s", len(lines), m.height, screen)
			}
			for _, line := range lines {
				if w := lipgloss.Width(line); w > m.width {
					rt.Fatalf("a line of %d cells in a window of %d:\n%q", w, m.width, line)
				}
			}
		}
	})
}
