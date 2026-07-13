package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// dump/search are CLI-only, like export/import: human inspection workflows
// over the local SQLite file. No HTTP route, no MCP tool — the model already
// has memstate_get/memstate_search for programmatic access.

// ---------- ANSI rendering ----------

// palette wraps text in ANSI escapes when enabled. Colors stay in the basic
// 16-color range so they respect the user's terminal theme.
type palette struct{ on bool }

func (p palette) wrap(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// wrapPair uses a targeted off-code instead of the full \x1b[0m reset so
// spans can nest: inline code inside a bold span restores the default
// foreground ([39m) without cancelling bold, and vice versa ([22m).
func (p palette) wrapPair(on, off, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + on + "m" + s + "\x1b[" + off + "m"
}

func (p palette) boldSpan(s string) string { return p.wrapPair("1", "22", s) }
func (p palette) codeSpan(s string) string { return p.wrapPair("36", "39", s) }

func (p palette) bold(s string) string    { return p.wrap("1", s) }
func (p palette) dim(s string) string     { return p.wrap("2", s) }
func (p palette) cyan(s string) string    { return p.wrap("36", s) }
func (p palette) green(s string) string   { return p.wrap("32", s) }
func (p palette) yellow(s string) string  { return p.wrap("33", s) }
func (p palette) blue(s string) string    { return p.wrap("34", s) }
func (p palette) magenta(s string) string { return p.wrap("35", s) }

func newPalette(noColorFlag bool) palette {
	if noColorFlag || os.Getenv("NO_COLOR") != "" {
		return palette{}
	}
	fi, err := os.Stdout.Stat()
	return palette{on: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

var (
	inlineCodeRe = regexp.MustCompile("`[^`]+`")
	boldSpanRe   = regexp.MustCompile(`\*\*[^*]+\*\*`)
	listMarkerRe = regexp.MustCompile(`^(\s*)([-*+]|\d+\.)(\s+)`)
	mdHeadingRe  = regexp.MustCompile(`^#{1,6}\s`)
	fenceRe      = regexp.MustCompile("^(```|~~~)")
)

// renderMarkdown gives stored content a light syntax-highlighting pass:
// headings, fenced code blocks, inline code, bold spans, list markers,
// blockquotes. Line-based on purpose — content is agent-written markdown,
// not something worth a full parser.
func renderMarkdown(content string, indent string, p palette) string {
	var b strings.Builder
	fence := "" // opening marker when inside a fenced block, else ""
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		b.WriteString(indent)
		trimmed := strings.TrimSpace(line)
		switch {
		case fence == "" && fenceRe.MatchString(trimmed):
			fence = trimmed[:3]
			b.WriteString(p.dim(line))
		case fence != "" && strings.HasPrefix(trimmed, fence):
			// Only the matching marker closes the block — a ~~~ line inside
			// a ``` fence is content, not a toggle.
			fence = ""
			b.WriteString(p.dim(line))
		case fence != "":
			b.WriteString(p.green(line))
		case mdHeadingRe.MatchString(line):
			b.WriteString(p.bold(p.magenta(line)))
		case strings.HasPrefix(trimmed, ">"):
			b.WriteString(p.dim(line))
		default:
			if m := listMarkerRe.FindStringSubmatch(line); m != nil {
				b.WriteString(m[1] + p.yellow(m[2]) + m[3])
				line = line[len(m[0]):]
			}
			line = inlineCodeRe.ReplaceAllStringFunc(line, p.codeSpan)
			line = boldSpanRe.ReplaceAllStringFunc(line, p.boldSpan)
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// entryMeta builds the dim metadata suffix: version, category, topics, date.
func entryMeta(m *Memory) string {
	parts := []string{fmt.Sprintf("v%d", m.Version)}
	if m.Category != "" {
		parts = append(parts, m.Category)
	}
	if len(m.Topics) > 0 {
		parts = append(parts, strings.Join(m.Topics, ", "))
	}
	parts = append(parts, time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04"))
	return strings.Join(parts, " · ")
}

func printEntry(m *Memory, showProject bool, p palette) {
	kp := m.Keypath
	if showProject {
		kp = m.ProjectID + ":" + kp
	}
	fmt.Printf("%s  %s\n", p.bold(p.cyan(kp)), p.dim(entryMeta(m)))
	fmt.Print(renderMarkdown(m.Content, "  ", p))
	fmt.Println()
}

// printKeyTree renders sorted keypaths as an indented tree. A segment that
// only exists as a prefix of deeper keys prints as a branch; a segment with
// its own row prints as a leaf with metadata.
func printKeyTree(mems []*Memory, p palette) {
	var prev []string
	for _, m := range mems {
		parts := strings.Split(m.Keypath, ".")
		common := 0
		for common < len(parts)-1 && common < len(prev) && parts[common] == prev[common] {
			common++
		}
		for i := common; i < len(parts)-1; i++ {
			fmt.Printf("%s%s\n", strings.Repeat("  ", i), p.bold(p.blue(parts[i])))
		}
		fmt.Printf("%s%s  %s\n",
			strings.Repeat("  ", len(parts)-1),
			p.cyan(parts[len(parts)-1]),
			p.dim(entryMeta(m)))
		prev = parts
	}
}

// projectHint appends the live project list to a not-found error so the user
// doesn't need a second command to recover.
func projectHint(store *Store) string {
	ps, err := store.ListProjects()
	if err != nil || len(ps) == 0 {
		return ""
	}
	ids := make([]string, len(ps))
	for i, pr := range ps {
		ids[i] = pr.ID
	}
	return "live projects: " + strings.Join(ids, ", ")
}

// flagAfterPositional catches "dump PROJECT --keys": stdlib flag parsing
// stops at the first positional arg, so a trailing flag would silently be
// taken as a keypath or query term. Reject it with a usage hint instead.
// An explicit "--" terminator in the raw args opts out, so hyphen-leading
// terms stay expressible: memstated search -- --idle-timeout
func flagAfterPositional(raw, positionals []string) bool {
	if slices.Contains(raw, "--") {
		return false
	}
	for _, a := range positionals {
		if strings.HasPrefix(a, "-") && a != "-" {
			return true
		}
	}
	return false
}

// ---------- subcommands ----------

func cmdDump(args []string) int {
	fs := flag.NewFlagSet("dump", flag.ExitOnError)
	db := fs.String("db", "", "SQLite file (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	keys := fs.Bool("keys", false, "list keypaths only, as a tree (no content)")
	noColor := fs.Bool("no-color", false, "disable ANSI colors")
	_ = fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 || flagAfterPositional(args, fs.Args()) {
		fmt.Fprintln(os.Stderr, "usage: memstated dump [--keys] [--db PATH] PROJECT [KEYPATH] (flags before args)")
		return 2
	}
	project := fs.Arg(0)
	keypath := NormalizeKeypath(fs.Arg(1))
	p := newPalette(*noColor)

	store, _, err := openStoreCLI(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated dump: %v\n", err)
		return 1
	}
	defer store.Close()

	// Match the daemon's read path: soft-deleted projects are invisible
	// until a write revives them.
	deleted, err := store.ProjectDeleted(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated dump: %v\n", err)
		return 1
	}
	if deleted {
		fmt.Fprintf(os.Stderr, "memstated dump: project %s is deleted\n", project)
		if hint := projectHint(store); hint != "" {
			fmt.Fprintf(os.Stderr, "memstated dump: %s\n", hint)
		}
		return 1
	}

	mems, err := store.List(project, keypath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated dump: %v\n", err)
		return 1
	}
	if len(mems) == 0 {
		where := "project " + project
		if keypath != "" {
			where += " under " + keypath
		}
		fmt.Fprintf(os.Stderr, "memstated dump: no memories in %s\n", where)
		if hint := projectHint(store); hint != "" {
			fmt.Fprintf(os.Stderr, "memstated dump: %s\n", hint)
		}
		return 1
	}

	scope := project
	if keypath != "" {
		scope += " · " + keypath
	}
	fmt.Printf("%s\n\n", p.dim(fmt.Sprintf("%s — %d keypath(s)", scope, len(mems))))
	if *keys {
		printKeyTree(mems, p)
		return 0
	}
	for _, m := range mems {
		printEntry(m, false, p)
	}
	return 0
}

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	db := fs.String("db", "", "SQLite file (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	project := fs.String("project", "", "restrict to one project (default: all)")
	limit := fs.Int("limit", 20, "maximum results")
	noColor := fs.Bool("no-color", false, "disable ANSI colors")
	_ = fs.Parse(args)
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" || flagAfterPositional(args, fs.Args()) || *limit < 1 {
		fmt.Fprintln(os.Stderr, "usage: memstated search [--project ID] [--limit N] [--db PATH] QUERY... (flags before args, limit >= 1)")
		return 2
	}
	p := newPalette(*noColor)

	store, _, err := openStoreCLI(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated search: %v\n", err)
		return 1
	}
	defer store.Close()

	mems, err := store.Search(*project, query, SearchFilter{}, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memstated search: %v\n", err)
		return 1
	}
	if len(mems) == 0 {
		fmt.Fprintf(os.Stderr, "memstated search: no matches for %q\n", query)
		return 1
	}
	fmt.Printf("%s\n\n", p.dim(fmt.Sprintf("%d match(es) for %q", len(mems), query)))
	for _, m := range mems {
		printEntry(m, *project == "", p)
	}
	return 0
}
