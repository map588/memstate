package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-isatty"
)

// `memstate ui` is the terminal browser. It follows the rule of the other
// verbs: reads open the SQLite file directly, writes go through the shared
// daemon. This file holds the entry point and every function that touches
// the store, the daemon or a file. The model (tui_model.go) only changes
// state, so tests drive it with messages and no terminal.

// uiRawPage is the number of raw table rows on one page.
const uiRawPage = 100

// uiSource is the provenance that a write from the browser carries.
const uiSource = "memstate ui"

// uiBackend is the data access of the browser.
type uiBackend struct {
	store    *Store
	dbPath   string
	addrFlag string // --addr; empty means discover the daemon at each write
	readOnly string // not empty: the reason why this session cannot write
	host     string // this machine's slug, to hide other hosts in _shared
}

// daemon returns a client for a daemon that answers now. The browser calls
// it before each write, so a daemon that starts later is found.
func (b *uiBackend) daemon() (*daemonClient, error) {
	if b.readOnly != "" {
		return nil, errors.New(b.readOnly)
	}
	d, err := openDaemon(b.addrFlag)
	if err != nil {
		return nil, fmt.Errorf("read-only: %v", err)
	}
	if !looksLikeOurDaemon(d.addr) {
		return nil, fmt.Errorf("read-only: no memstate daemon answers at %s", d.addr)
	}
	return d, nil
}

// uiHasTerminal is true when stdin and stdout are a terminal. It is a
// variable because `go test` can give a test the developer's terminal.
var uiHasTerminal = func() bool {
	fd := os.Stdout.Fd()
	return stdinIsTerminal() && (isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd))
}

const uiUsageLine = "ui [--project ID | --shared] [--db PATH] [--addr HOST:PORT] [--no-color]"

func cliUI(args []string) int {
	fs, o := newVerbFlags("ui")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 0 || o.json {
		return cliUsage(uiUsageLine)
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	if !uiHasTerminal() {
		fmt.Fprintln(cliErr, "memstate: ui needs a terminal on stdin and stdout; use tree, get or history in a script")
		return 2
	}
	store, dbPath, err := openStoreCLI(o.db)
	if err != nil {
		return cliFail("%v", err)
	}
	defer store.Close()

	b := &uiBackend{store: store, dbPath: dbPath, addrFlag: o.addr, host: hostSlug()}
	if o.db != "" && o.addr == "" {
		// The discovered daemon serves the default store. A write would go
		// to a different file than the one on the screen.
		b.readOnly = "read-only: --db names the store file; add --addr HOST:PORT of the daemon that serves it to write"
	}
	noColor := o.noColor || os.Getenv("NO_COLOR") != ""
	var opts []tea.ProgramOption
	if o.noColor {
		opts = append(opts, tea.WithEnvironment(append(os.Environ(), "NO_COLOR=1")))
	}
	if _, err := tea.NewProgram(newUIModel(b, project, palette{on: !noColor}), opts...).Run(); err != nil {
		return cliFail("ui: %v", err)
	}
	return 0
}

// sanitizeTerm makes stored text safe to print. Stored text comes from
// agents and from the pages they read, so it is not trusted terminal input:
// an escape sequence in a note must not clear the screen, set the window
// title or move the cursor. A newline stays, a tab becomes four spaces
// (the layout counts cells), a carriage return goes, and each other control
// character becomes its visible picture.
func sanitizeTerm(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\n') || c >= 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
		case r < 0x20:
			b.WriteRune(0x2400 + r) // control pictures: ESC shows as ␛
		case r == 0x7f:
			b.WriteRune(0x2421)
		case r >= 0x80 && r <= 0x9f:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// oneLine is sanitizeTerm for a cell or a list entry: no line break.
func oneLine(s string) string {
	return strings.ReplaceAll(sanitizeTerm(s), "\n", "↵")
}

// splitTopics reads the comma-separated topics field of a form.
func splitTopics(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---------- messages ----------

type projectsLoadedMsg struct{ projects []*Project }

type treeLoadedMsg struct {
	project string
	mems    []*Memory
}

type historyLoadedMsg struct {
	project, keypath string
	versions         []*Memory
}

type tablesLoadedMsg struct{ counts []RawTableCount }

type rowsLoadedMsg struct {
	table string
	page  int
	cols  []string
	rows  [][]string
}

type uiErrMsg struct{ err error }

// uiOp names a write that needs a form before it can start.
type uiOp int

const (
	opNew uiOp = iota
	opMeta
	opDeleteProject
)

// writableMsg says that a daemon answers, so the form of op can open.
type writableMsg struct {
	op               uiOp
	project, keypath string
}

// uiEditJob is one editor round trip: the temp file, and what to store
// when the editor ends.
type uiEditJob struct {
	project, keypath string
	path             string // temp file
	before           string // content that the editor got
	category         string
	topics           []string
}

type editReadyMsg struct{ job uiEditJob }

type editorDoneMsg struct {
	job uiEditJob
	err error
}

type deleteReadyMsg struct {
	project, keypath string
	recursive        bool
	keypaths         []string
}

// writeDoneMsg ends each write. The model reloads and puts the cursor on
// keypath when it is not empty.
type writeDoneMsg struct {
	status           string
	project, keypath string
}

// ---------- commands: reads (SQLite) ----------

func (b *uiBackend) loadProjects() tea.Msg {
	ps, err := b.store.ListProjects()
	if err != nil {
		return uiErrMsg{err}
	}
	return projectsLoadedMsg{ps}
}

func (b *uiBackend) loadTree(project string) tea.Cmd {
	return func() tea.Msg {
		mems, err := b.store.List(project, "")
		if err != nil {
			return uiErrMsg{err}
		}
		return treeLoadedMsg{project, mems}
	}
}

func (b *uiBackend) loadHistory(project, keypath string) tea.Cmd {
	return func() tea.Msg {
		vs, err := b.store.History(project, keypath)
		if err != nil {
			return uiErrMsg{err}
		}
		return historyLoadedMsg{project, keypath, vs}
	}
}

func (b *uiBackend) loadTables() tea.Msg {
	counts, err := b.store.RawTableCounts()
	if err != nil {
		return uiErrMsg{err}
	}
	return tablesLoadedMsg{counts}
}

func (b *uiBackend) loadRows(table string, page int) tea.Cmd {
	return func() tea.Msg {
		cols, rows, err := b.store.RawRows(table, page*uiRawPage, uiRawPage)
		if err != nil {
			return uiErrMsg{err}
		}
		return rowsLoadedMsg{table, page, cols, rows}
	}
}

// ---------- commands: writes (daemon) ----------

func (b *uiBackend) checkWritable(op uiOp, project, keypath string) tea.Cmd {
	return func() tea.Msg {
		if _, err := b.daemon(); err != nil {
			return uiErrMsg{err}
		}
		return writableMsg{op, project, keypath}
	}
}

// currentMemories reads the live memories at keypath through the daemon,
// so that a write starts from the store it will go to.
func currentMemories(d *daemonClient, project, keypath string, recursive, content bool) ([]*Memory, error) {
	raw, err := d.call("POST", "/keypaths", map[string]any{
		"project_id": project, "keypath": keypath, "recursive": recursive, "include_content": content,
	})
	if err != nil {
		return nil, err
	}
	var cur struct {
		Memories []*Memory `json:"memories"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		return nil, fmt.Errorf("decode daemon reply: %v", err)
	}
	return cur.Memories, nil
}

func currentMemory(d *daemonClient, project, keypath string) (*Memory, error) {
	mems, err := currentMemories(d, project, keypath, false, true)
	if err != nil {
		return nil, err
	}
	for _, m := range mems {
		if m.Keypath == keypath {
			return m, nil
		}
	}
	return nil, nil
}

// storeStatus posts one memory and returns the status line for the reply.
func storeStatus(d *daemonClient, project, keypath, content, category string, topics []string) (string, error) {
	body := map[string]any{"project_id": project, "keypath": keypath, "content": content, "source": uiSource}
	if category != "" {
		body["category"] = category
	}
	if len(topics) > 0 {
		body["topics"] = topics
	}
	raw, err := d.call("POST", "/memories/store", body)
	if err != nil {
		return "", err
	}
	var r storeReply
	if err := json.Unmarshal(raw, &r); err != nil || r.Stored == nil {
		return "", fmt.Errorf("decode daemon reply: %v", err)
	}
	line := fmt.Sprintf("%s  %s v%d", r.Action, r.Stored.Keypath, r.Stored.Version)
	if r.Action == "superseded" && r.Superseded != nil {
		line += fmt.Sprintf(" (was v%d)", r.Superseded.Version)
	}
	return line, nil
}

// prepareEdit writes the editor's temp file. With create it refuses a
// keypath that has a value; without it, it refuses one that has none and
// keeps the category and the topics of the current version.
func (b *uiBackend) prepareEdit(job uiEditJob, create bool) tea.Cmd {
	return func() tea.Msg {
		d, err := b.daemon()
		if err != nil {
			return uiErrMsg{err}
		}
		prev, err := currentMemory(d, job.project, job.keypath)
		if err != nil {
			return uiErrMsg{err}
		}
		switch {
		case create && prev != nil:
			return uiErrMsg{fmt.Errorf("%s has a value; use e to edit it", job.keypath)}
		case !create && prev == nil:
			return uiErrMsg{fmt.Errorf("no value at %s in project %s", job.keypath, job.project)}
		case !create:
			job.before, job.category, job.topics = prev.Content, prev.Category, prev.Topics
		}
		tmp, err := os.CreateTemp("", "memstate-*.md")
		if err != nil {
			return uiErrMsg{err}
		}
		job.path = tmp.Name()
		_, werr := tmp.WriteString(job.before)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(job.path)
			return uiErrMsg{werr}
		}
		return editReadyMsg{job}
	}
}

// saveEdit stores what the editor left in the temp file, and removes the
// file.
func (b *uiBackend) saveEdit(job uiEditJob, editorErr error) tea.Cmd {
	return func() tea.Msg {
		defer os.Remove(job.path)
		if editorErr != nil {
			return uiErrMsg{fmt.Errorf("editor: %v", editorErr)}
		}
		raw, err := os.ReadFile(job.path)
		if err != nil {
			return uiErrMsg{err}
		}
		after := string(raw)
		if after == job.before {
			return writeDoneMsg{"no change  " + job.keypath, job.project, job.keypath}
		}
		if strings.TrimSpace(after) == "" {
			return uiErrMsg{errors.New("empty content, nothing stored; use d to delete a keypath")}
		}
		d, err := b.daemon()
		if err != nil {
			return uiErrMsg{err}
		}
		status, err := storeStatus(d, job.project, job.keypath, after, job.category, job.topics)
		if err != nil {
			return uiErrMsg{err}
		}
		return writeDoneMsg{status, job.project, job.keypath}
	}
}

// saveMeta stores the current content again with a new category and new
// topics. The store makes a version when the metadata differs.
func (b *uiBackend) saveMeta(project, keypath, category string, topics []string) tea.Cmd {
	return func() tea.Msg {
		d, err := b.daemon()
		if err != nil {
			return uiErrMsg{err}
		}
		prev, err := currentMemory(d, project, keypath)
		if err != nil {
			return uiErrMsg{err}
		}
		if prev == nil {
			return uiErrMsg{fmt.Errorf("no value at %s in project %s", keypath, project)}
		}
		status, err := storeStatus(d, project, keypath, prev.Content, category, topics)
		if err != nil {
			return uiErrMsg{err}
		}
		return writeDoneMsg{status, project, keypath}
	}
}

// prepareDelete lists what a delete will tombstone, for the confirmation.
func (b *uiBackend) prepareDelete(project, keypath string, recursive bool) tea.Cmd {
	return func() tea.Msg {
		d, err := b.daemon()
		if err != nil {
			return uiErrMsg{err}
		}
		mems, err := currentMemories(d, project, keypath, recursive, false)
		if err != nil {
			return uiErrMsg{err}
		}
		var kps []string
		for _, m := range mems {
			if recursive || m.Keypath == keypath {
				kps = append(kps, m.Keypath)
			}
		}
		if len(kps) == 0 {
			return uiErrMsg{fmt.Errorf("nothing to delete in project %s under %s", project, keypath)}
		}
		return deleteReadyMsg{project, keypath, recursive, kps}
	}
}

func (b *uiBackend) deleteKeypath(project, keypath string, recursive bool) tea.Cmd {
	return func() tea.Msg {
		d, err := b.daemon()
		if err != nil {
			return uiErrMsg{err}
		}
		raw, err := d.call("POST", "/memories/delete", map[string]any{
			"project_id": project, "keypath": keypath, "recursive": recursive,
		})
		if err != nil {
			return uiErrMsg{err}
		}
		var r struct {
			DeletedCount int `json:"deleted_count"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return uiErrMsg{fmt.Errorf("decode daemon reply: %v", err)}
		}
		return writeDoneMsg{fmt.Sprintf("deleted %d keypath(s) under %s", r.DeletedCount, keypath), project, ""}
	}
}

func (b *uiBackend) deleteProject(project string) tea.Cmd {
	return func() tea.Msg {
		d, err := b.daemon()
		if err != nil {
			return uiErrMsg{err}
		}
		if _, err := d.call("POST", "/projects/delete", map[string]any{"project_id": project}); err != nil {
			return uiErrMsg{err}
		}
		return writeDoneMsg{"deleted project " + project + " (soft delete; a write to it brings it back)", "", ""}
	}
}
