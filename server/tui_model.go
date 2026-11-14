package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// The model of `memstate ui`. Update changes state only: it reads no file
// and no socket. Each read and each write is a tea.Cmd from tui.go that
// comes back as a message.

type uiScreen int

const (
	screenBrowser uiScreen = iota
	screenTables
)

// Panes of the browser screen, in tab order.
const (
	paneProjects = iota
	paneKeys
	paneDetail
)

// uiNode is one visible line of the keypath tree. path is the dotted name
// of the node; keypath is not empty only when the node has a value.
type uiNode struct {
	path, name, keypath string
	depth               int
	hasChildren         bool
}

// uiForm asks for one or more values on the footer line. submit gets the
// values after the last one; an error from it goes to the status line.
type uiForm struct {
	title  string
	labels []string
	values []string
	idx    int
	lines  []string // context for the detail pane, e.g. the keypaths to delete
	submit func(values []string) (tea.Cmd, error)
}

type uiModel struct {
	b         *uiBackend
	pal       palette
	runEditor func(path string, done func(error) tea.Msg) tea.Cmd

	width, height int
	screen        uiScreen
	help          bool
	status        string
	statusErr     bool

	// Browser screen.
	focus       int
	gotProjects bool // the project list has arrived at least one time
	projects    []*Project
	projFilter  string
	projCur     int
	project     string // the project on screen
	loaded      bool   // the memories of project have arrived
	mems        map[string]*Memory
	root        *TreeNode
	collapsed   map[string]bool
	keyFilter   string
	nodes       []uiNode
	keyCur      int
	wantPath    string // put the cursor here when the tree arrives

	showHistory bool
	history     []*Memory
	histCur     int

	detail      viewport.Model
	detailText  string
	detailWidth int

	// Tables screen.
	tabFocus  int // 0 = table list, 1 = rows
	tables    []RawTableCount
	tabCur    int
	rawTable  string
	rawPage   int
	rawCols   []string
	rawRows   [][]string
	grid      table.Model
	colOff    int  // first column on screen
	colShown  int  // number of columns on screen
	gridTop   bool // put the row cursor on the first row when the page arrives
	rowDetail bool
	rowView   viewport.Model

	// Footer input: a form, or the filter of the focused pane.
	form      *uiForm
	filtering bool
	input     textinput.Model
}

func newUIModel(b *uiBackend, project string, pal palette) *uiModel {
	in := textinput.New()
	in.Prompt = ""
	// No blink: a blink is a timer message twice a second for no gain.
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	in.Focus()

	detail := viewport.New()
	detail.SoftWrap = true
	rowView := viewport.New()
	rowView.SoftWrap = true

	grid := table.New(table.WithFocused(true))
	grid.SetStyles(uiTableStyles())

	return &uiModel{
		b:         b,
		pal:       pal,
		project:   project,
		focus:     paneKeys,
		collapsed: map[string]bool{},
		detail:    detail,
		rowView:   rowView,
		grid:      grid,
		input:     in,
		runEditor: func(path string, done func(error) tea.Msg) tea.Cmd {
			cmd, _ := editorCommand(path)
			return tea.ExecProcess(cmd, done)
		},
	}
}

func (m *uiModel) Init() tea.Cmd {
	return tea.Batch(m.b.loadProjects, m.b.loadTables)
}

func (m *uiModel) setStatus(s string, isErr bool) {
	m.status, m.statusErr = oneLine(s), isErr
}

// ---------- pure helpers ----------

func clampCur(cur, n int) int {
	if n <= 0 {
		return 0
	}
	return min(max(cur, 0), n-1)
}

// moveCur applies a movement key to a list cursor. ok is false when the
// key is not a movement key.
func moveCur(cur, n, page int, key string) (int, bool) {
	switch key {
	case "j", "down":
		cur++
	case "k", "up":
		cur--
	case "g", "home":
		cur = 0
	case "G", "end":
		cur = n - 1
	case "pgdown":
		cur += max(page, 1)
	case "pgup":
		cur -= max(page, 1)
	default:
		return cur, false
	}
	return clampCur(cur, n), true
}

// visibleProjects is the project list after the filter, the shared scope
// first.
func (m *uiModel) visibleProjects() []*Project {
	f := strings.ToLower(m.projFilter)
	out := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		if f == "" || strings.Contains(strings.ToLower(p.ID), f) {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b *Project) int {
		switch {
		case a.ID == sharedProject && b.ID != sharedProject:
			return -1
		case b.ID == sharedProject && a.ID != sharedProject:
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (m *uiModel) selectedNode() *uiNode {
	if m.keyCur < 0 || m.keyCur >= len(m.nodes) {
		return nil
	}
	return &m.nodes[m.keyCur]
}

func (m *uiModel) selectedPath() string {
	if n := m.selectedNode(); n != nil {
		return n.path
	}
	return ""
}

// rebuildNodes makes the visible lines from the tree, the folds and the
// filter. A filter shows each match with its parents and ignores folds.
func (m *uiModel) rebuildNodes() {
	m.nodes = m.nodes[:0]
	if m.root == nil {
		return
	}
	f := strings.ToLower(m.keyFilter)
	var walk func(n *TreeNode, prefix string, depth int) bool
	walk = func(n *TreeNode, prefix string, depth int) bool {
		path := n.Name
		if prefix != "" {
			path = prefix + "." + n.Name
		}
		at := len(m.nodes)
		node := uiNode{path: path, name: n.Name, depth: depth, hasChildren: len(n.Children) > 0}
		if n.HasValue {
			node.keypath = n.Keypath
		}
		m.nodes = append(m.nodes, node)
		matched := f == "" || strings.Contains(strings.ToLower(path), f)
		if f != "" || !m.collapsed[path] {
			for _, c := range n.Children {
				if walk(c, path, depth+1) {
					matched = true
				}
			}
		}
		if !matched {
			m.nodes = m.nodes[:at]
		}
		return matched
	}
	for _, c := range m.root.Children {
		walk(c, "", 0)
	}
}

// selectPath puts the cursor on path and opens the folds above it. When
// path is gone, the cursor stays where it was, inside the list.
func (m *uiModel) selectPath(path string) {
	if path != "" {
		parts := strings.Split(path, ".")
		for i := 1; i < len(parts); i++ {
			delete(m.collapsed, strings.Join(parts[:i], "."))
		}
	}
	m.rebuildNodes()
	if i := slices.IndexFunc(m.nodes, func(n uiNode) bool { return n.path == path }); i >= 0 {
		m.keyCur = i
	}
	m.keyCur = clampCur(m.keyCur, len(m.nodes))
}

// openProject shows another project and asks for its memories.
func (m *uiModel) openProject(id string) tea.Cmd {
	m.project = id
	m.loaded = false
	m.mems, m.root, m.nodes = nil, nil, nil
	m.keyCur, m.keyFilter = 0, ""
	m.collapsed = map[string]bool{}
	m.showHistory, m.history, m.histCur = false, nil, 0
	return m.b.loadTree(id)
}

// reload reads everything again. want is the keypath for the cursor; when
// it is empty the cursor stays on its path.
func (m *uiModel) reload(want string) tea.Cmd {
	m.wantPath = want
	cmds := []tea.Cmd{m.b.loadProjects, m.b.loadTables}
	if m.project != "" {
		cmds = append(cmds, m.b.loadTree(m.project))
	}
	if m.rawTable != "" {
		cmds = append(cmds, m.b.loadRows(m.rawTable, m.rawPage))
	}
	return tea.Batch(cmds...)
}

func (m *uiModel) tableCount(name string) int {
	for _, t := range m.tables {
		if t.Name == name {
			return t.Count
		}
	}
	return 0
}

// sync runs after each message. It keeps each cursor inside its list,
// opens the project or the table under the cursor, and refreshes the
// detail pane.
func (m *uiModel) sync() tea.Cmd {
	var cmds []tea.Cmd
	vis := m.visibleProjects()
	m.projCur = clampCur(m.projCur, len(vis))
	if len(vis) > 0 && vis[m.projCur].ID != m.project {
		cmds = append(cmds, m.openProject(vis[m.projCur].ID))
	}
	m.keyCur = clampCur(m.keyCur, len(m.nodes))
	m.histCur = clampCur(m.histCur, len(m.history))
	m.tabCur = clampCur(m.tabCur, len(m.tables))
	if m.screen == screenTables && len(m.tables) > 0 && m.tables[m.tabCur].Name != m.rawTable {
		m.rawTable, m.rawPage = m.tables[m.tabCur].Name, 0
		m.rawCols, m.rawRows, m.rowDetail = nil, nil, false
		m.colOff, m.gridTop = 0, true
		m.setGrid()
		cmds = append(cmds, m.b.loadRows(m.rawTable, 0))
	}
	m.refreshDetail()
	return tea.Batch(cmds...)
}

// ---------- update ----------

func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
	case tea.KeyPressMsg:
		cmd = m.key(msg)

	case projectsLoadedMsg:
		first := !m.gotProjects
		m.gotProjects = true
		m.projects = msg.projects
		vis := m.visibleProjects()
		if i := slices.IndexFunc(vis, func(p *Project) bool { return p.ID == m.project }); i >= 0 {
			m.projCur = i
			if first {
				cmd = m.b.loadTree(m.project)
			}
		}
	case treeLoadedMsg:
		if msg.project != m.project {
			break
		}
		keep := m.wantPath
		if keep == "" {
			keep = m.selectedPath()
		}
		m.wantPath = ""
		m.loaded = true
		m.mems = make(map[string]*Memory, len(msg.mems))
		m.root = &TreeNode{Name: msg.project}
		for _, mem := range msg.mems {
			m.mems[mem.Keypath] = mem
			insertPath(m.root, strings.Split(mem.Keypath, "."), mem.Keypath, mem.Version)
		}
		if msg.project == sharedProject {
			m.root.Children = pruneTreeHosts(m.root.Children, m.b.host)
		}
		m.selectPath(keep)
		if n := m.selectedNode(); m.showHistory && n != nil && n.keypath != "" {
			cmd = m.b.loadHistory(m.project, n.keypath)
		} else {
			m.showHistory, m.history = false, nil
		}
	case historyLoadedMsg:
		if n := m.selectedNode(); m.showHistory && n != nil && msg.project == m.project && msg.keypath == n.keypath {
			m.history = msg.versions
		}
	case tablesLoadedMsg:
		m.tables = msg.counts
	case rowsLoadedMsg:
		if msg.table == m.rawTable && msg.page == m.rawPage {
			m.rawCols, m.rawRows = msg.cols, msg.rows
			m.setGrid()
			if m.gridTop {
				m.grid.SetCursor(0)
				m.gridTop = false
			}
		}

	case writableMsg:
		m.openOpForm(msg)
	case editReadyMsg:
		job := msg.job
		cmd = m.runEditor(job.path, func(err error) tea.Msg { return editorDoneMsg{job, err} })
	case editorDoneMsg:
		cmd = m.b.saveEdit(msg.job, msg.err)
	case deleteReadyMsg:
		m.openDeleteForm(msg)
	case writeDoneMsg:
		m.setStatus(msg.status, false)
		want := ""
		if msg.project == m.project {
			want = msg.keypath
		}
		cmd = m.reload(want)
	case uiErrMsg:
		m.setStatus(msg.err.Error(), true)
	}
	return m, tea.Batch(cmd, m.sync())
}

func (m *uiModel) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return tea.Quit
	}
	if m.form != nil {
		return m.formKey(k)
	}
	if m.filtering {
		return m.filterKey(k)
	}
	if m.help {
		m.help = false
		return nil
	}
	m.status, m.statusErr = "", false
	switch s {
	case "q":
		return tea.Quit
	case "?":
		m.help = true
		return nil
	case "1":
		m.screen = screenBrowser
		return nil
	case "2":
		m.screen = screenTables
		return nil
	case "r":
		return m.reload("")
	}
	if m.screen == screenTables {
		return m.tablesKey(k)
	}
	return m.browserKey(k)
}

func (m *uiModel) browserKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	node := m.selectedNode()
	switch s {
	case "tab":
		m.focus = (m.focus + 1) % 3
	case "shift+tab":
		m.focus = (m.focus + 2) % 3
	case "left":
		m.focus = max(m.focus-1, paneProjects)
	case "right":
		m.focus = min(m.focus+1, paneDetail)
	case "/":
		if m.focus == paneDetail {
			m.focus = paneKeys
		}
		m.filtering = true
		if m.focus == paneProjects {
			m.input.SetValue(m.projFilter)
		} else {
			m.input.SetValue(m.keyFilter)
		}
		m.input.CursorEnd()
	case "esc":
		switch {
		case m.showHistory:
			m.showHistory, m.history = false, nil
		case m.focus == paneProjects:
			m.projFilter = ""
		default:
			path := m.selectedPath()
			m.keyFilter = ""
			m.selectPath(path)
		}
	case "h":
		if m.showHistory {
			m.showHistory, m.history = false, nil
			break
		}
		if node == nil || node.keypath == "" {
			m.setStatus("no value here; history is for a keypath that has a value", true)
			break
		}
		m.showHistory, m.history, m.histCur = true, nil, 0
		m.focus = paneDetail
		return m.b.loadHistory(m.project, node.keypath)

	case "n":
		if m.project == "" {
			m.setStatus("no project", true)
			break
		}
		prefill := ""
		if node != nil {
			if i := strings.LastIndex(node.path, "."); i >= 0 {
				prefill = node.path[:i+1]
			}
		}
		return m.b.checkWritable(opNew, m.project, prefill)
	case "e":
		if node == nil || node.keypath == "" {
			m.setStatus("no value here; n makes a new memory", true)
			break
		}
		return m.b.prepareEdit(uiEditJob{project: m.project, keypath: node.keypath}, false)
	case "m":
		if node == nil || node.keypath == "" {
			m.setStatus("no value here; category and topics belong to a value", true)
			break
		}
		return m.b.checkWritable(opMeta, m.project, node.keypath)
	case "d":
		if node == nil {
			m.setStatus("nothing to delete", true)
			break
		}
		return m.b.prepareDelete(m.project, node.path, node.hasChildren)
	case "D":
		if m.project == "" || len(m.visibleProjects()) == 0 {
			m.setStatus("no project", true)
			break
		}
		return m.b.checkWritable(opDeleteProject, m.project, "")

	default:
		return m.browserMove(k, node)
	}
	return nil
}

func (m *uiModel) browserMove(k tea.KeyPressMsg, node *uiNode) tea.Cmd {
	s := k.String()
	_, _, _, paneH := m.browserDims()
	page := paneH - 3
	switch m.focus {
	case paneProjects:
		if s == "enter" {
			m.focus = paneKeys
			break
		}
		m.projCur, _ = moveCur(m.projCur, len(m.visibleProjects()), page, s)
	case paneKeys:
		if s == "enter" || s == "space" {
			if node == nil {
				break
			}
			if !node.hasChildren {
				m.focus = paneDetail
				break
			}
			if m.keyFilter != "" {
				break // a filter shows each match; a fold has no effect
			}
			m.collapsed[node.path] = !m.collapsed[node.path]
			m.selectPath(node.path)
			break
		}
		if cur, ok := moveCur(m.keyCur, len(m.nodes), page, s); ok {
			if cur != m.keyCur {
				m.showHistory, m.history = false, nil
			}
			m.keyCur = cur
		}
	case paneDetail:
		if m.showHistory {
			m.histCur, _ = moveCur(m.histCur, len(m.history), page, s)
			break
		}
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(k)
		return cmd
	}
	return nil
}

func (m *uiModel) tablesKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if m.rowDetail {
		if s == "esc" || s == "enter" || s == "left" {
			m.rowDetail = false
			return nil
		}
		var cmd tea.Cmd
		m.rowView, cmd = m.rowView.Update(k)
		return cmd
	}
	switch s {
	case "tab", "shift+tab":
		m.tabFocus ^= 1
	case "esc":
		m.tabFocus = 0
	case "left":
		if m.tabFocus == 1 && m.colOff > 0 {
			m.colOff--
			m.setGrid()
			break
		}
		m.tabFocus = 0
	case "right":
		if m.tabFocus == 1 && m.colOff+m.colShown < len(m.rawCols) {
			m.colOff++
			m.setGrid()
			break
		}
		m.tabFocus = 1
	case "enter":
		if m.tabFocus == 0 {
			m.tabFocus = 1
			break
		}
		if row := m.grid.Cursor(); row >= 0 && row < len(m.rawRows) {
			m.rowDetail = true
			m.rowView.SetContent(m.rowText(m.rawRows[row]))
			m.rowView.GotoTop()
		}
	case "pgdown":
		if m.rawTable != "" && (m.rawPage+1)*uiRawPage < m.tableCount(m.rawTable) {
			m.rawPage++
			m.gridTop = true
			return m.b.loadRows(m.rawTable, m.rawPage)
		}
	case "pgup":
		if m.rawTable != "" && m.rawPage > 0 {
			m.rawPage--
			m.gridTop = true
			return m.b.loadRows(m.rawTable, m.rawPage)
		}
	default:
		if m.tabFocus == 0 {
			m.tabCur, _ = moveCur(m.tabCur, len(m.tables), 1, s)
			break
		}
		var cmd tea.Cmd
		m.grid, cmd = m.grid.Update(k)
		return cmd
	}
	return nil
}

// ---------- footer input ----------

func (m *uiModel) openForm(f *uiForm) {
	m.form = f
	m.input.SetValue(f.values[0])
	m.input.CursorEnd()
}

func (m *uiModel) formKey(k tea.KeyPressMsg) tea.Cmd {
	f := m.form
	switch k.String() {
	case "esc":
		m.form = nil
		m.setStatus("cancelled; nothing changed", false)
		return nil
	case "enter":
		f.values[f.idx] = m.input.Value()
		if f.idx+1 < len(f.labels) {
			f.idx++
			m.input.SetValue(f.values[f.idx])
			m.input.CursorEnd()
			return nil
		}
		m.form = nil
		cmd, err := f.submit(f.values)
		if err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
		return cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

func (m *uiModel) filterKey(k tea.KeyPressMsg) tea.Cmd {
	var cmd tea.Cmd
	switch k.String() {
	case "enter":
		m.filtering = false
		return nil
	case "esc":
		m.filtering = false
		m.input.SetValue("")
	default:
		m.input, cmd = m.input.Update(k)
	}
	if m.focus == paneProjects {
		m.projFilter = m.input.Value()
		return cmd
	}
	path := m.selectedPath()
	m.keyFilter = m.input.Value()
	m.selectPath(path)
	return cmd
}

// openOpForm opens the form of a write, after the daemon answered.
func (m *uiModel) openOpForm(msg writableMsg) {
	project, keypath := msg.project, msg.keypath
	switch msg.op {
	case opNew:
		m.openForm(&uiForm{
			title:  "new memory in " + project,
			labels: []string{"keypath", "category (can be empty)", "topics, comma-separated (can be empty)"},
			values: []string{keypath, "", ""},
			submit: func(v []string) (tea.Cmd, error) {
				kp := NormalizeKeypath(v[0])
				if kp == "" {
					return nil, errors.New("no keypath; nothing stored")
				}
				job := uiEditJob{project: project, keypath: kp,
					category: strings.TrimSpace(v[1]), topics: splitTopics(v[2])}
				return m.b.prepareEdit(job, true), nil
			},
		})
	case opMeta:
		category, topics := "", ""
		if mem := m.mems[keypath]; mem != nil && project == m.project {
			category, topics = mem.Category, strings.Join(mem.Topics, ", ")
		}
		m.openForm(&uiForm{
			title:  "category and topics of " + keypath,
			labels: []string{"category (empty clears it)", "topics, comma-separated (empty clears them)"},
			values: []string{category, topics},
			submit: func(v []string) (tea.Cmd, error) {
				return m.b.saveMeta(project, keypath, strings.TrimSpace(v[0]), splitTopics(v[1])), nil
			},
		})
	case opDeleteProject:
		m.openForm(&uiForm{
			title:  "delete project " + project,
			labels: []string{"type the project id to delete it"},
			values: []string{""},
			lines: []string{
				"This is a soft delete of project " + project + ".",
				"Its memories and their history stay in the store, hidden.",
				"A later write to " + project + " brings the project back.",
			},
			submit: func(v []string) (tea.Cmd, error) {
				if v[0] != project {
					return nil, errors.New("the id does not match; nothing deleted")
				}
				return m.b.deleteProject(project), nil
			},
		})
	}
}

func (m *uiModel) openDeleteForm(msg deleteReadyMsg) {
	lines := []string{fmt.Sprintf("Delete from project %s (each keypath gets a tombstone; history stays):", msg.project), ""}
	for _, kp := range msg.keypaths {
		lines = append(lines, "  "+kp)
	}
	m.openForm(&uiForm{
		title:  "delete " + msg.keypath,
		labels: []string{fmt.Sprintf("type y to delete %d keypath(s)", len(msg.keypaths))},
		values: []string{""},
		lines:  lines,
		submit: func(v []string) (tea.Cmd, error) {
			if a := strings.ToLower(strings.TrimSpace(v[0])); a != "y" && a != "yes" {
				return nil, errors.New("not confirmed; nothing deleted")
			}
			return m.b.deleteKeypath(msg.project, msg.keypath, msg.recursive), nil
		},
	})
}
