package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Rendering of `memstate ui`. Each string from the store goes through
// sanitizeTerm or oneLine before it gets here or in here; the only escape
// sequences in the output are the styles that this file and dump.go add.

// The smallest terminal that the layout fits.
const (
	uiMinWidth  = 50
	uiMinHeight = 8
)

// uiTablesListWidth is the outer width of the table list on screen 2.
const uiTablesListWidth = 28

var (
	uiSelFocused = lipgloss.NewStyle().Reverse(true)
	uiSelBlurred = lipgloss.NewStyle().Bold(true)
	uiBold       = lipgloss.NewStyle().Bold(true)
	uiFaint      = lipgloss.NewStyle().Faint(true)
	uiErrStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	uiOKStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	uiBorder     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	uiBorderOn   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6"))
)

func uiTableStyles() table.Styles {
	return table.Styles{
		Header:   lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle().Reverse(true),
	}
}

// fit cuts plain text to w cells. It must not get styled text: a cut
// inside an escape sequence would leave the sequence open.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if len(r) > w {
		r = r[:w]
	}
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// box draws a bordered pane of the outer size w x h. lines are styled
// lines that already fit; box adds no wrap.
func box(title string, lines []string, w, h int, focused bool) string {
	style := uiBorder
	if focused {
		style = uiBorderOn
	}
	inner := max(h-2, 1)
	body := append([]string{uiBold.Render(fit(title, w-2))}, lines...)
	if len(body) > inner {
		body = body[:inner]
	}
	return style.Width(w).Height(h).MaxHeight(h).Render(strings.Join(body, "\n"))
}

// listWindow returns the first visible index of a list of n lines in a
// window of h lines, with the cursor near the middle.
func listWindow(cur, n, h int) int {
	if h <= 0 || n <= h {
		return 0
	}
	return min(max(cur-h/2, 0), n-h)
}

// browserDims returns the outer widths of the three panes and their
// height.
func (m *uiModel) browserDims() (projW, keyW, detW, paneH int) {
	paneH = max(m.height-3, 3)
	projW = min(max(m.width/5, 16), 32)
	keyW = min(max(m.width/3, 20), 48)
	detW = max(m.width-projW-keyW, 10)
	return
}

// layout gives each component its size after a resize.
func (m *uiModel) layout() {
	_, _, detW, paneH := m.browserDims()
	m.detail.SetWidth(max(detW-2, 1))
	m.detail.SetHeight(max(paneH-3, 1))
	gridW := max(m.width-uiTablesListWidth-2, 1)
	m.rowView.SetWidth(gridW)
	m.rowView.SetHeight(max(paneH-3, 1))
	// The footer gives half of the line to the label and half to the input.
	m.input.SetWidth(max(m.width/2-3, 5))
	m.setGrid()
}

// setGrid puts the raw rows into the table widget. The widget does not cut
// its header to its width, so setGrid shows a window of columns that fits:
// it starts at colOff, and the left and right keys move it. The row cursor
// stays on its row.
func (m *uiModel) setGrid() {
	_, _, _, paneH := m.browserDims()
	avail := max(m.width-uiTablesListWidth-2, 1)
	const pad, maxCol, minCol = 2, 40, 4

	natural := make([]int, len(m.rawCols))
	for i, c := range m.rawCols {
		natural[i] = lipgloss.Width(c)
	}
	cells := make([][]string, len(m.rawRows))
	for r, raw := range m.rawRows {
		cells[r] = make([]string, len(natural))
		for i := range natural {
			if i < len(raw) {
				cells[r][i] = oneLine(raw[i])
			}
			natural[i] = max(natural[i], min(lipgloss.Width(cells[r][i]), maxCol))
		}
	}

	m.colOff = clampCur(m.colOff, len(natural))
	var cols []table.Column
	used := 0
	for i := m.colOff; i < len(natural) && used < avail; i++ {
		w := natural[i]
		if used+w+pad > avail {
			// The last column gets what is left, when that is of use.
			w = avail - used - pad
			if w < minCol && len(cols) > 0 {
				break
			}
			w = max(w, 1)
		}
		cols = append(cols, table.Column{Title: m.rawCols[i], Width: w})
		used += w + pad
	}
	m.colShown = len(cols)
	rows := make([]table.Row, len(cells))
	for r := range cells {
		rows[r] = cells[r][m.colOff : m.colOff+len(cols)]
	}

	cur := m.grid.Cursor()
	// Rows first: the widget renders its rows when the columns change, and
	// an old row with more cells than the new columns is out of range.
	m.grid.SetRows(nil)
	m.grid.SetColumns(cols)
	m.grid.SetRows(rows)
	m.grid.SetWidth(avail)
	m.grid.SetHeight(max(paneH-3, 2))
	m.grid.SetCursor(clampCur(cur, len(rows)))
}

// rowText is the row detail: each column of one raw row, in full.
func (m *uiModel) rowText(row []string) string {
	var b strings.Builder
	for i, cell := range row {
		if i < len(m.rawCols) {
			b.WriteString(uiBold.Render(m.rawCols[i]))
			b.WriteByte('\n')
		}
		b.WriteString(sanitizeTerm(cell))
		b.WriteString("\n\n")
	}
	return lipgloss.Wrap(b.String(), m.rowView.Width(), "")
}

// versionLine is one line of the history list.
func versionLine(v *Memory) string {
	line := entryMeta(v) // starts with the version
	if v.Source != "" {
		line += "  " + v.Source
	}
	if v.Tombstone {
		line += "  [tombstone]"
	}
	return oneLine(line)
}

// detailContent builds the text of the detail pane.
func (m *uiModel) detailContent() string {
	if m.form != nil && len(m.form.lines) > 0 {
		lines := make([]string, len(m.form.lines))
		for i, l := range m.form.lines {
			lines[i] = oneLine(l)
		}
		return strings.Join(lines, "\n")
	}
	if sr := m.search; sr != nil {
		if sr.cur >= len(sr.hits) {
			return "no match for " + oneLine(sr.query)
		}
		hit := sr.hits[sr.cur]
		meta := entryMeta(&hit.Memory)
		if len(hit.Sources) > 0 {
			meta += " · found by " + strings.Join(hit.Sources, ", ")
		}
		return m.pal.bold(m.pal.cyan(oneLine(hit.ProjectID+":"+hit.Keypath))) + "\n" +
			m.pal.dim(oneLine(meta)) + "\n\n" +
			renderMarkdown(sanitizeTerm(hit.Content), "", m.pal)
	}
	if !m.loaded {
		if m.gotProjects && len(m.projects) == 0 {
			return "no projects in this store"
		}
		return "loading " + oneLine(m.project)
	}
	node := m.selectedNode()
	if node == nil {
		if m.keyFilter != "" {
			return "no keypath matches the filter"
		}
		return "no memories in project " + oneLine(m.project)
	}
	var b strings.Builder
	if m.showHistory {
		b.WriteString(m.pal.bold("history of "+oneLine(node.keypath)) + "\n\n")
		for i, v := range m.history {
			mark := "  "
			if i == m.histCur {
				mark = "> "
			}
			b.WriteString(mark + versionLine(v) + "\n")
		}
		if m.histCur < len(m.history) {
			v := m.history[m.histCur]
			b.WriteString("\n")
			if v.Tombstone {
				b.WriteString(m.pal.dim("(deleted here: this version is a tombstone)") + "\n")
			} else {
				b.WriteString(renderMarkdown(sanitizeTerm(v.Content), "", m.pal))
			}
		}
		return b.String()
	}
	mem := m.mems[node.keypath]
	if node.keypath == "" || mem == nil {
		below := 0
		prefix := node.path + "."
		for kp := range m.mems {
			if strings.HasPrefix(kp, prefix) {
				below++
			}
		}
		return fmt.Sprintf("%s\n\n%d keypath(s) below. This name has no value of its own.",
			m.pal.bold(m.pal.blue(oneLine(node.path))), below)
	}
	meta := entryMeta(mem)
	if mem.Source != "" {
		meta += " · " + mem.Source
	}
	b.WriteString(m.pal.bold(m.pal.cyan(oneLine(mem.Keypath))) + "\n")
	b.WriteString(m.pal.dim(oneLine(meta)) + "\n\n")
	b.WriteString(renderMarkdown(sanitizeTerm(mem.Content), "", m.pal))
	return b.String()
}

// refreshDetail gives the detail pane its text, wrapped at words to the
// width of the pane. The scroll position stays while the text and the
// width are the same.
func (m *uiModel) refreshDetail() {
	text, w := m.detailContent(), m.detail.Width()
	if text == m.detailText && w == m.detailWidth {
		return
	}
	m.detailText, m.detailWidth = text, w
	m.detail.SetContent(lipgloss.Wrap(text, w, ""))
	m.detail.GotoTop()
}

// ---------- view ----------

func (m *uiModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m *uiModel) render() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	if m.width < uiMinWidth || m.height < uiMinHeight {
		return fit(fmt.Sprintf("memstate ui: the terminal is too small (%dx%d, need %dx%d)",
			m.width, m.height, uiMinWidth, uiMinHeight), m.width)
	}
	var body string
	switch {
	case m.help:
		body = m.helpView()
	case m.screen == screenTables:
		body = m.tablesView()
	default:
		body = m.browserView()
	}
	return m.headerView() + "\n" + body + "\n" + m.footerView()
}

func (m *uiModel) headerView() string {
	tabs := "[1] browser  [2] tables"
	left := "memstate ui · " + oneLine(m.b.dbPath)
	return uiBold.Render(fit(left, m.width-len(tabs)-2)) + "  " + uiFaint.Render(tabs)
}

func (m *uiModel) footerView() string {
	var line string
	switch {
	case m.form != nil:
		f := m.form
		// A form with context lines has its title on the pane above.
		label := fmt.Sprintf("(%d/%d) %s: ", f.idx+1, len(f.labels), f.labels[f.idx])
		if len(f.lines) == 0 {
			label = oneLine(f.title) + " " + label
		}
		label = fit(label, m.width/2)
		line = uiBold.Render(label) + m.input.View()
	case m.filtering:
		line = uiBold.Render("filter: ") + m.input.View()
	case m.statusErr:
		line = uiErrStyle.Render(fit(m.status, m.width))
	default:
		line = uiOKStyle.Render(fit(m.status, m.width))
	}
	var hint string
	switch {
	case m.form != nil:
		hint = "enter next · esc cancel"
	case m.filtering:
		hint = "enter keep · esc clear"
	case m.screen == screenBrowser && m.search != nil:
		hint = "enter open the match · esc close the list · s search again · tab pane · q quit"
	case m.screen == screenTables:
		hint = "tab pane · ←/→ columns · enter row · pgup/pgdn page · X purge (memories) · r reload · 1 browser · ? keys · q quit"
	default:
		hint = "? keys · tab pane · / filter · s search · h history · n new · e edit · m meta · d delete · X purge · D delete project · 2 tables · q quit"
	}
	return line + "\n" + uiFaint.Render(fit(hint, m.width))
}

// listLines renders a window of n list entries. entry returns the plain
// text of one entry.
func listLines(n, cur, w, h int, focused bool, entry func(i int) string) []string {
	top := listWindow(cur, n, h)
	lines := make([]string, 0, h)
	for i := top; i < n && i < top+h; i++ {
		text := fit(entry(i), w)
		switch {
		case i == cur && focused:
			text = uiSelFocused.Render(text)
		case i == cur:
			text = uiSelBlurred.Render(text)
		}
		lines = append(lines, text)
	}
	return lines
}

func (m *uiModel) browserView() string {
	projW, keyW, detW, paneH := m.browserDims()
	rows := paneH - 3

	vis := m.visibleProjects()
	projTitle := fmt.Sprintf("projects (%d)", len(vis))
	if m.projFilter != "" {
		projTitle += " /" + oneLine(m.projFilter)
	}
	projLines := listLines(len(vis), m.projCur, projW-2, rows, m.focus == paneProjects, func(i int) string {
		return fmt.Sprintf("%s (%d)", oneLine(vis[i].ID), vis[i].MemoryCount)
	})

	keyTitle := fmt.Sprintf("%s (%d)", oneLine(m.project), len(m.mems))
	if m.keyFilter != "" {
		keyTitle += " /" + oneLine(m.keyFilter)
	}
	keyLines := listLines(len(m.nodes), m.keyCur, keyW-2, rows, m.focus == paneKeys, func(i int) string {
		n := m.nodes[i]
		mark := "  "
		switch {
		case n.hasChildren && m.collapsed[n.path] && m.keyFilter == "":
			mark = "▸ "
		case n.hasChildren:
			mark = "▾ "
		}
		text := strings.Repeat("  ", n.depth) + mark + oneLine(n.name)
		if mem := m.mems[n.keypath]; n.keypath != "" && mem != nil {
			text += fmt.Sprintf("  v%d", mem.Version)
		}
		return text
	})

	if sr := m.search; sr != nil {
		keyTitle = fmt.Sprintf("search: %s (%d)", oneLine(sr.query), len(sr.hits))
		keyLines = listLines(len(sr.hits), sr.cur, keyW-2, rows, m.focus == paneKeys, func(i int) string {
			if sr.project == "" {
				return oneLine(sr.hits[i].ProjectID + ":" + sr.hits[i].Keypath)
			}
			return oneLine(sr.hits[i].Keypath)
		})
	}

	detTitle := "memory"
	switch {
	case m.form != nil && len(m.form.lines) > 0:
		detTitle = oneLine(m.form.title)
	case m.search != nil:
		detTitle = "match"
	case m.showHistory:
		detTitle = "history"
	}
	detLines := strings.Split(m.detail.View(), "\n")

	return lipgloss.JoinHorizontal(lipgloss.Top,
		box(projTitle, projLines, projW, paneH, m.focus == paneProjects),
		box(keyTitle, keyLines, keyW, paneH, m.focus == paneKeys),
		box(detTitle, detLines, detW, paneH, m.focus == paneDetail),
	)
}

func (m *uiModel) tablesView() string {
	_, _, _, paneH := m.browserDims()
	rows := paneH - 3
	gridW := m.width - uiTablesListWidth

	listLns := listLines(len(m.tables), m.tabCur, uiTablesListWidth-2, rows, m.tabFocus == 0, func(i int) string {
		return fmt.Sprintf("%s (%d)", m.tables[i].Name, m.tables[i].Count)
	})

	var title string
	var body []string
	switch {
	case m.form != nil && len(m.form.lines) > 0:
		// A purge from this screen: show what the form asks about.
		title = oneLine(m.form.title)
		for _, l := range m.form.lines {
			body = append(body, fit(oneLine(l), gridW-2))
		}
	case m.rowDetail:
		title = fmt.Sprintf("%s · row %d", m.rawTable, m.rawPage*uiRawPage+m.grid.Cursor()+1)
		body = strings.Split(m.rowView.View(), "\n")
	default:
		count := m.tableCount(m.rawTable)
		pages := max((count+uiRawPage-1)/uiRawPage, 1)
		title = fmt.Sprintf("%s · page %d/%d · columns %d-%d of %d · read-only", m.rawTable, m.rawPage+1, pages,
			min(m.colOff+1, len(m.rawCols)), m.colOff+m.colShown, len(m.rawCols))
		if len(m.rawRows) == 0 {
			body = []string{uiFaint.Render("no rows")}
		} else {
			body = strings.Split(m.grid.View(), "\n")
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		box("tables", listLns, uiTablesListWidth, paneH, m.tabFocus == 0),
		box(title, body, gridW, paneH, m.tabFocus == 1),
	)
}

const uiHelpText = `Keys

  1 / 2            browser screen / raw tables screen
  tab, shift+tab   next / previous pane        left, right also move
  j k, arrows      move        g G  first / last       pgup pgdn  page
  /                filter the projects or the keypaths by name (esc clears)
  s / S            search the content of this project / of each project;
                   enter opens a match, esc closes the list
  enter, space     fold or open a branch of the keypath tree
  h                version history of the keypath (esc closes)
  r                read the store again
  q, ctrl+c        quit

Writes (browser screen; each one goes through the shared daemon)

  n   new memory: keypath, category, topics, then $EDITOR
  e   edit the content in $VISUAL / $EDITOR; category and topics stay
  m   change category and topics; the content stays
  d   delete: a tombstone on the keypath, or on each keypath below a branch
  D   soft-delete the project

Purge (direct on the store file; needs no daemon; no undo)

  X   erase the keypath, or each keypath below a branch: the content, each
      version, the search row and the vectors. On the raw memories table,
      X erases the keypath of the selected row, a deleted one too.

Raw tables

  left, right      show the columns to the left / to the right
  enter            open the row with each column in full
  pgup, pgdn       previous / next page of 100 rows

Press a key to close this page.`

func (m *uiModel) helpView() string {
	_, _, _, paneH := m.browserDims()
	lines := strings.Split(uiHelpText, "\n")
	for i := range lines {
		lines[i] = fit(lines[i], m.width-2)
	}
	return box("memstate ui", lines, m.width, paneH, true)
}
