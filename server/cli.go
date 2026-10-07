package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

// `memstate` is the human CLI. It is the same binary as the daemon: `make
// install` links `memstate` to `memstated`, and main() dispatches on the
// program name. Reads open the SQLite file directly, like dump/search did,
// so they work without a daemon. Writes go through the shared daemon so
// embeddings, versioning and the user-scope gate behave exactly as MCP
// writes do.

var (
	cliOut io.Writer = os.Stdout
	cliErr io.Writer = os.Stderr
	cliIn  io.Reader = os.Stdin
)

// cliOpts are the flags every verb accepts.
type cliOpts struct {
	project string
	user    bool
	json    bool
	noColor bool
	db      string
	addr    string
}

func (o *cliOpts) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.project, "project", "", "project id (default: this repo's slug)")
	fs.BoolVar(&o.user, "user", false, "the reserved user scope (_user)")
	fs.BoolVar(&o.json, "json", false, "print the raw JSON shape")
	fs.BoolVar(&o.noColor, "no-color", false, "disable ANSI colors")
	fs.StringVar(&o.db, "db", "", "SQLite file for reads (default MEMSTATE_DB or ~/.memstate/memstate.db)")
	fs.StringVar(&o.addr, "addr", "", "shared daemon address (default MEMSTATE_ADDR, then daemon.addr)")
}

func newVerbFlags(name string) (*flag.FlagSet, *cliOpts) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := &cliOpts{}
	o.bind(fs)
	return fs, o
}

// resolveCLIProject picks the project for a verb: --user, else --project,
// else the repository slug of the working directory (the same rule as the
// proxy and the Python skill).
func resolveCLIProject(o cliOpts) (string, error) {
	if o.user && o.project != "" {
		return "", errors.New("use --user or --project, not both")
	}
	if o.user {
		return userProject, nil
	}
	if o.project != "" {
		return o.project, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return deriveProject(cwd), nil
}

// parseInterspersed lets flags follow positionals ("get todo --json").
// The stdlib parser stops at the first positional, so it runs again after
// each one. A "--" ends flag parsing; what follows stays positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		tail = args[i+1:]
		args = args[:i]
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return append(pos, tail...), nil
}

func cliFail(format string, a ...any) int {
	fmt.Fprintf(cliErr, "memstate: "+format+"\n", a...)
	return 1
}

func cliUsage(line string) int {
	fmt.Fprintf(cliErr, "usage: memstate %s\n", line)
	return 2
}

func printJSON(v any) {
	enc := json.NewEncoder(cliOut)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func printRawJSON(raw []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Write(raw)
	}
	buf.WriteByte('\n')
	_, _ = cliOut.Write(buf.Bytes())
}

func printCLIUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: memstate <verb> [args] [flags]

Browse and edit the memstate store. Reads open the SQLite file directly;
writes go through the shared daemon (MEMSTATE_ADDR, else daemon.addr).

  memstate tree     [KEYPATH]              keypath tree with metadata, plus the
                                           user scope for this host
  memstate get      [KEYPATH] [--raw]      content at KEYPATH and below
  memstate history  KEYPATH                every version, newest first, with tombstones
  memstate search   QUERY... [--all] [--mode hybrid|fts|semantic] [--limit N]
                    [--category C] [--topics a,b]
                                           hybrid search through the daemon;
                                           FTS over SQLite when no daemon runs
  memstate set      KEYPATH VALUE|-        write one fact ("-" reads stdin)
                    [--category C] [--topics a,b] [--source S]
  memstate edit     KEYPATH                open the current content in $EDITOR,
                                           store it when it changed
  memstate rm       KEYPATH [--recursive] [--yes]
                                           tombstone a keypath or a subtree
  memstate projects                        live projects with memory counts
  memstate status   [--addr HOST:PORT]     daemon health

Flags on every verb:
  --project ID   a project other than this repository's
  --user         the reserved user scope (preferences, profile, host.<slug>.*)
  --json         raw JSON, no color
  --no-color     plain text (also NO_COLOR, or a non-terminal stdout)
  --db PATH      SQLite file for reads (default MEMSTATE_DB, ~/.memstate/memstate.db)
  --addr H:P     daemon for writes and search (default MEMSTATE_ADDR, daemon.addr)

Daemon lifecycle, export/import, embeddings and upgrades: memstated --help
`)
}

// cmdCLI dispatches one memstate verb.
func cmdCLI(args []string) int {
	if len(args) == 0 {
		printCLIUsage(cliErr)
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "tree":
		return cliTree(rest)
	case "get", "show", "cat":
		return cliGet(rest)
	case "history", "log":
		return cliHistory(rest)
	case "search":
		return cliSearch(rest)
	case "set":
		return cliSet(rest)
	case "edit":
		return cliEdit(rest)
	case "rm", "delete":
		return cliRm(rest)
	case "projects":
		return cmdProjects(rest)
	case "status":
		return cmdStatus(rest)
	case "-h", "--help", "help":
		printCLIUsage(cliOut)
		return 0
	}
	fmt.Fprintf(cliErr, "memstate: unknown verb %q\n\n", verb)
	printCLIUsage(cliErr)
	return 2
}

// ---------- store access ----------

// openCLIStore opens the SQLite file and refuses a soft-deleted project,
// matching the daemon's read path.
func openCLIStore(o cliOpts, project string) (*Store, error) {
	store, _, err := openStoreCLI(o.db)
	if err != nil {
		return nil, err
	}
	deleted, err := store.ProjectDeleted(project)
	if err != nil {
		store.Close()
		return nil, err
	}
	if deleted {
		msg := fmt.Sprintf("project %s is deleted", project)
		if hint := projectHint(store); hint != "" {
			msg += "; " + hint
		}
		store.Close()
		return nil, errors.New(msg)
	}
	return store, nil
}

// ---------- daemon access ----------

const noDaemonHint = `no shared daemon; start one with "memstated --addr 127.0.0.1:8765" or set MEMSTATE_ADDR`

type daemonClient struct{ addr string }

func openDaemon(addrFlag string) (*daemonClient, error) {
	addr := addrFlag
	if addr == "" {
		var ok bool
		if addr, ok = discoverAddr(); !ok {
			return nil, errors.New(noDaemonHint)
		}
	}
	return &daemonClient{addr: addr}, nil
}

// call sends one request under /api/v1 and returns the raw body. A non-200
// reply becomes an error that carries the daemon's message.
func (d *daemonClient) call(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+d.addr+"/api/v1"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach daemon at %s: %v; %s", d.addr, err, noDaemonHint)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return nil, fmt.Errorf("daemon: HTTP %d: %s", resp.StatusCode, e.Error)
	}
	return raw, nil
}

// ---------- verbs ----------

func cliTree(args []string) int {
	fs, o := newVerbFlags("tree")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) > 1 {
		return cliUsage("tree [KEYPATH] [--project ID | --user] [--json]")
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	keypath := ""
	if len(pos) == 1 {
		keypath = NormalizeKeypath(pos[0])
	}
	store, err := openCLIStore(*o, project)
	if err != nil {
		return cliFail("%v", err)
	}
	defer store.Close()

	host := hostSlug()
	// The user scope rides along with a whole-project tree, like the proxy's
	// memstate_get(); a subtree or the user scope itself shows alone.
	withUser := !o.user && keypath == ""
	var userMems []*Memory
	if withUser {
		if deleted, _ := store.ProjectDeleted(userProject); !deleted {
			all, err := store.List(userProject, "")
			if err != nil {
				return cliFail("%v", err)
			}
			for _, m := range all {
				if !isOtherHost(m.Keypath, host) {
					userMems = append(userMems, m)
				}
			}
		}
	}
	if o.user {
		all, err := store.List(project, keypath)
		if err != nil {
			return cliFail("%v", err)
		}
		userMems = nil
		for _, m := range all {
			if !isOtherHost(m.Keypath, host) {
				userMems = append(userMems, m)
			}
		}
	}

	if o.json {
		root, err := store.Tree(project)
		if err != nil {
			return cliFail("%v", err)
		}
		node := root
		if keypath != "" {
			if node = subtree(root, keypath); node == nil {
				return cliFail("no memories in project %s under %s", project, keypath)
			}
		}
		domains := node.Children
		if keypath != "" {
			domains = []*TreeNode{node}
		}
		if o.user {
			domains = pruneTreeHosts(domains, host)
		}
		total := 0
		for _, d := range domains {
			countValues(d, &total)
		}
		out := map[string]any{"project_id": project, "domains": orEmpty(domains), "total_memories": total}
		if withUser {
			udom := []*TreeNode{}
			if deleted, _ := store.ProjectDeleted(userProject); !deleted {
				uroot, err := store.Tree(userProject)
				if err != nil {
					return cliFail("%v", err)
				}
				udom = pruneTreeHosts(uroot.Children, host)
			}
			utotal := 0
			for _, d := range udom {
				countValues(d, &utotal)
			}
			out["user"] = map[string]any{"host": host, "domains": orEmpty(udom), "total_memories": utotal}
		}
		printJSON(out)
		return 0
	}

	p := newPalette(o.noColor)
	if o.user {
		fmt.Fprintf(cliOut, "%s\n\n", p.dim(fmt.Sprintf("user scope (host %s) — %d keypath(s)", host, len(userMems))))
		printKeyTree(cliOut, userMems, p)
		return 0
	}
	mems, err := store.List(project, keypath)
	if err != nil {
		return cliFail("%v", err)
	}
	scope := project
	if keypath != "" {
		scope += " · " + keypath
	}
	fmt.Fprintf(cliOut, "%s\n\n", p.dim(fmt.Sprintf("%s — %d keypath(s)", scope, len(mems))))
	if len(mems) == 0 {
		if hint := projectHint(store); hint != "" {
			fmt.Fprintf(cliOut, "%s\n\n", p.dim(hint))
		}
	}
	printKeyTree(cliOut, mems, p)
	if withUser && len(userMems) > 0 {
		fmt.Fprintf(cliOut, "\n%s\n\n", p.dim(fmt.Sprintf("user scope (host %s) — %d keypath(s)", host, len(userMems))))
		printKeyTree(cliOut, userMems, p)
	}
	return 0
}

// subtree walks root down one dot-separated keypath.
func subtree(root *TreeNode, keypath string) *TreeNode {
	node := root
	for _, seg := range strings.Split(keypath, ".") {
		var next *TreeNode
		for _, c := range node.Children {
			if c.Name == seg {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		node = next
	}
	return node
}

// pruneTreeHosts keeps only this machine under the `host` domain.
func pruneTreeHosts(domains []*TreeNode, host string) []*TreeNode {
	out := make([]*TreeNode, 0, len(domains))
	for _, d := range domains {
		if d.Name == "host" {
			kept := &TreeNode{Name: d.Name, Keypath: d.Keypath, HasValue: d.HasValue, Version: d.Version}
			for _, c := range d.Children {
				if c.Name == host {
					kept.Children = append(kept.Children, c)
				}
			}
			d = kept
		}
		out = append(out, d)
	}
	return out
}

func cliGet(args []string) int {
	fs, o := newVerbFlags("get")
	raw := fs.Bool("raw", false, "content only, no metadata")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) > 1 {
		return cliUsage("get [KEYPATH] [--raw] [--project ID | --user] [--json]")
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	keypath := ""
	if len(pos) == 1 {
		keypath = NormalizeKeypath(pos[0])
	}
	store, err := openCLIStore(*o, project)
	if err != nil {
		return cliFail("%v", err)
	}
	defer store.Close()
	mems, err := store.List(project, keypath)
	if err != nil {
		return cliFail("%v", err)
	}
	if o.user {
		host := hostSlug()
		mems = slices.DeleteFunc(mems, func(m *Memory) bool { return isOtherHost(m.Keypath, host) })
	}
	if len(mems) == 0 {
		where := "project " + project
		if keypath != "" {
			where += " under " + keypath
		}
		msg := "no memories in " + where
		if hint := projectHint(store); hint != "" {
			msg += "; " + hint
		}
		return cliFail("%s", msg)
	}
	switch {
	case o.json:
		printJSON(map[string]any{"memories": mems, "total_count": len(mems)})
	case *raw:
		for i, m := range mems {
			if i > 0 {
				fmt.Fprintln(cliOut)
			}
			fmt.Fprintln(cliOut, strings.TrimRight(m.Content, "\n"))
		}
	default:
		p := newPalette(o.noColor)
		scope := project
		if keypath != "" {
			scope += " · " + keypath
		}
		fmt.Fprintf(cliOut, "%s\n\n", p.dim(fmt.Sprintf("%s — %d keypath(s)", scope, len(mems))))
		for _, m := range mems {
			printEntry(cliOut, m, false, p)
		}
	}
	return 0
}

func cliHistory(args []string) int {
	fs, o := newVerbFlags("history")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		return cliUsage("history KEYPATH [--project ID | --user] [--json]")
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	keypath := NormalizeKeypath(pos[0])
	store, _, err := openStoreCLI(o.db)
	if err != nil {
		return cliFail("%v", err)
	}
	defer store.Close()
	versions, err := store.History(project, keypath)
	if err != nil {
		return cliFail("%v", err)
	}
	if len(versions) == 0 {
		return cliFail("no history for %s in project %s", keypath, project)
	}
	if o.json {
		printJSON(map[string]any{"versions": versions, "total_versions": len(versions)})
		return 0
	}
	p := newPalette(o.noColor)
	fmt.Fprintf(cliOut, "%s\n\n", p.dim(fmt.Sprintf("%s · %s — %d version(s), newest first", project, keypath, len(versions))))
	for _, m := range versions {
		head := entryMeta(m)
		if m.Tombstone {
			head += " · " + p.yellow("deleted")
		}
		fmt.Fprintf(cliOut, "%s\n", p.bold(p.cyan(head)))
		if !m.Tombstone {
			fmt.Fprint(cliOut, renderMarkdown(m.Content, "  ", p))
		}
		fmt.Fprintln(cliOut)
	}
	return 0
}

// searchHit is one daemon search result: a memory plus fusion metadata.
type searchHit struct {
	Memory
	Score   float32  `json:"score,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

type searchOut struct {
	Mode       string      `json:"mode"`
	Query      string      `json:"query"`
	Degraded   string      `json:"degraded,omitempty"`
	Results    []searchHit `json:"results"`
	TotalFound int         `json:"total_found"`
}

func cliSearch(args []string) int {
	fs, o := newVerbFlags("search")
	all := fs.Bool("all", false, "search every project")
	mode := fs.String("mode", "hybrid", "hybrid | fts | semantic (daemon only)")
	limit := fs.Int("limit", defaultSearchLimit, "maximum results")
	category := fs.String("category", "", "only this category")
	topics := fs.String("topics", "", "comma-separated: any of these topics")
	pos, err := parseInterspersed(fs, args)
	query := strings.TrimSpace(strings.Join(pos, " "))
	if err != nil || query == "" || *limit < 1 {
		return cliUsage("search QUERY... [--all | --project ID | --user] [--mode M] [--limit N] [--category C] [--topics a,b] [--json]")
	}
	project := ""
	if !*all {
		if project, err = resolveCLIProject(*o); err != nil {
			return cliFail("%v", err)
		}
	}
	var topicList []string
	if *topics != "" {
		topicList = strings.Split(*topics, ",")
	}
	host := hostSlug()

	var out searchOut
	var raw []byte
	degraded := ""
	if o.db == "" {
		d, derr := openDaemon(o.addr)
		if derr == nil {
			body := map[string]any{"query": query, "mode": *mode, "limit": *limit, "include_content": true}
			if project != "" {
				body["project_id"] = project
			}
			if *category != "" {
				body["category"] = *category
			}
			if len(topicList) > 0 {
				body["topics"] = topicList
			}
			raw, derr = d.call("POST", "/memories/search", body)
			if derr == nil {
				if err := json.Unmarshal(raw, &out); err != nil {
					return cliFail("decode daemon reply: %v", err)
				}
			}
		}
		if derr != nil {
			if strings.Contains(derr.Error(), "HTTP 4") {
				return cliFail("%v", derr)
			}
			degraded = derr.Error()
		}
	}
	if raw == nil {
		// No daemon (or --db): the FTS side of hybrid, over SQLite.
		store, _, err := openStoreCLI(o.db)
		if err != nil {
			return cliFail("%v", err)
		}
		defer store.Close()
		mems, err := store.SearchAny(project, query, SearchFilter{Category: *category, Topics: topicList}, *limit)
		if err != nil {
			return cliFail("%v", err)
		}
		if degraded == "" {
			degraded = "fts only: reads from " + o.db
		}
		out = searchOut{Mode: "fts", Query: query, Degraded: degraded}
		for _, m := range mems {
			out.Results = append(out.Results, searchHit{Memory: *m, Sources: []string{"fts"}})
		}
	}
	if o.user {
		out.Results = slices.DeleteFunc(out.Results, func(h searchHit) bool { return isOtherHost(h.Keypath, host) })
	}
	out.TotalFound = len(out.Results)
	if out.Results == nil {
		out.Results = []searchHit{}
	}
	if o.json {
		printJSON(out)
		return 0
	}
	p := newPalette(o.noColor)
	head := fmt.Sprintf("%d match(es) for %q · %s", out.TotalFound, query, out.Mode)
	if out.Degraded != "" {
		if out.Mode == "fts" {
			head += " · " + p.yellow("fts only ("+out.Degraded+")")
		} else {
			head += " · " + p.yellow("degraded: "+out.Degraded)
		}
	}
	fmt.Fprintf(cliOut, "%s\n\n", p.dim(head))
	if out.TotalFound == 0 {
		return 1
	}
	for i := range out.Results {
		printEntry(cliOut, &out.Results[i].Memory, *all, p)
	}
	return 0
}

// storeReply is the daemon's answer to /memories/store.
type storeReply struct {
	Action     string     `json:"action"`
	Stored     *MemoryRef `json:"stored"`
	Superseded *MemoryRef `json:"superseded"`
}

func printStoreReply(raw []byte, o cliOpts) int {
	if o.json {
		printRawJSON(raw)
		return 0
	}
	var r storeReply
	if err := json.Unmarshal(raw, &r); err != nil || r.Stored == nil {
		return cliFail("decode daemon reply: %v", err)
	}
	p := newPalette(o.noColor)
	line := fmt.Sprintf("%s  %s v%d", r.Action, r.Stored.Keypath, r.Stored.Version)
	if r.Action == "superseded" && r.Superseded != nil {
		line += fmt.Sprintf(" (was v%d)", r.Superseded.Version)
	}
	fmt.Fprintln(cliOut, p.green(line))
	return 0
}

func cliSet(args []string) int {
	fs, o := newVerbFlags("set")
	category := fs.String("category", "", "kind: decision, config, status, note, gotcha, reference, learning")
	topics := fs.String("topics", "", "comma-separated subject tags")
	source := fs.String("source", "", "provenance shown in history")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 2 {
		return cliUsage("set KEYPATH VALUE|- [--category C] [--topics a,b] [--source S] [--project ID | --user]")
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	keypath := NormalizeKeypath(pos[0])
	content := pos[1]
	if content == "-" {
		b, err := io.ReadAll(cliIn)
		if err != nil {
			return cliFail("read stdin: %v", err)
		}
		content = strings.TrimRight(string(b), "\n")
	}
	if strings.TrimSpace(content) == "" {
		return cliFail("empty value; use rm to delete a keypath")
	}
	d, err := openDaemon(o.addr)
	if err != nil {
		return cliFail("%v", err)
	}
	body := map[string]any{"project_id": project, "keypath": keypath, "content": content}
	if *category != "" {
		body["category"] = *category
	}
	if *topics != "" {
		body["topics"] = strings.Split(*topics, ",")
	}
	if *source != "" {
		body["source"] = *source
	}
	raw, err := d.call("POST", "/memories/store", body)
	if err != nil {
		return cliFail("%v", err)
	}
	return printStoreReply(raw, *o)
}

func cliEdit(args []string) int {
	fs, o := newVerbFlags("edit")
	category := fs.String("category", "", "replace the category (default: keep the current one)")
	topics := fs.String("topics", "", "replace the topics (default: keep the current ones)")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		return cliUsage("edit KEYPATH [--category C] [--topics a,b] [--project ID | --user]")
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	keypath := NormalizeKeypath(pos[0])
	d, err := openDaemon(o.addr)
	if err != nil {
		return cliFail("%v", err)
	}
	// Read through the daemon too, so the editor shows the same store the
	// write will land in.
	raw, err := d.call("POST", "/keypaths", map[string]any{
		"project_id": project, "keypath": keypath, "recursive": false, "include_content": true,
	})
	if err != nil {
		return cliFail("%v", err)
	}
	var cur struct {
		Memories []*Memory `json:"memories"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		return cliFail("decode daemon reply: %v", err)
	}
	var prev *Memory
	for _, m := range cur.Memories {
		if m.Keypath == keypath {
			prev = m
			break
		}
	}
	before := ""
	if prev != nil {
		before = prev.Content
	}

	tmp, err := os.CreateTemp("", "memstate-*.md")
	if err != nil {
		return cliFail("%v", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(before); err != nil {
		tmp.Close()
		return cliFail("%v", err)
	}
	tmp.Close()

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], tmpPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return cliFail("editor %q: %v", editor, err)
	}
	afterBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return cliFail("%v", err)
	}
	after := string(afterBytes)
	if after == before {
		if o.json {
			printJSON(map[string]any{"action": "no_change", "keypath": keypath})
		} else {
			fmt.Fprintln(cliOut, newPalette(o.noColor).dim("no change  "+keypath))
		}
		return 0
	}
	if strings.TrimSpace(after) == "" {
		return cliFail("empty content; use rm to delete a keypath")
	}
	body := map[string]any{"project_id": project, "keypath": keypath, "content": after, "source": "memstate edit"}
	switch {
	case *category != "":
		body["category"] = *category
	case prev != nil && prev.Category != "":
		body["category"] = prev.Category
	}
	switch {
	case *topics != "":
		body["topics"] = strings.Split(*topics, ",")
	case prev != nil && len(prev.Topics) > 0:
		body["topics"] = prev.Topics
	}
	raw, err = d.call("POST", "/memories/store", body)
	if err != nil {
		return cliFail("%v", err)
	}
	return printStoreReply(raw, *o)
}

// stdinIsTerminal is true only for a real terminal. A plain Stat check is
// not enough: /dev/null is a character device too, and a prompt there would
// read EOF and abort instead of asking for --yes.
func stdinIsTerminal() bool {
	fd := os.Stdin.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

func cliRm(args []string) int {
	fs, o := newVerbFlags("rm")
	recursive := fs.Bool("recursive", false, "also tombstone every keypath below")
	yes := fs.Bool("yes", false, "skip the confirmation for --recursive")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		return cliUsage("rm KEYPATH [--recursive] [--yes] [--project ID | --user]")
	}
	project, err := resolveCLIProject(*o)
	if err != nil {
		return cliFail("%v", err)
	}
	keypath := NormalizeKeypath(pos[0])
	d, err := openDaemon(o.addr)
	if err != nil {
		return cliFail("%v", err)
	}
	p := newPalette(o.noColor)
	if *recursive {
		raw, err := d.call("POST", "/keypaths", map[string]any{
			"project_id": project, "keypath": keypath, "recursive": true, "include_content": false,
		})
		if err != nil {
			return cliFail("%v", err)
		}
		var cur struct {
			Memories []*Memory `json:"memories"`
		}
		if err := json.Unmarshal(raw, &cur); err != nil {
			return cliFail("decode daemon reply: %v", err)
		}
		if len(cur.Memories) == 0 {
			return cliFail("nothing to delete in project %s under %s", project, keypath)
		}
		if !*yes {
			if !stdinIsTerminal() {
				return cliFail("refusing to delete %d keypath(s) without --yes (stdin is not a terminal)", len(cur.Memories))
			}
			for _, m := range cur.Memories {
				fmt.Fprintf(cliOut, "  %s\n", p.cyan(m.Keypath))
			}
			fmt.Fprintf(cliOut, "Delete %d keypath(s)? [y/N] ", len(cur.Memories))
			answer, _ := bufio.NewReader(cliIn).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
				fmt.Fprintln(cliOut, "aborted")
				return 1
			}
		}
	}
	raw, err := d.call("POST", "/memories/delete", map[string]any{
		"project_id": project, "keypath": keypath, "recursive": *recursive,
	})
	if err != nil {
		return cliFail("%v", err)
	}
	if o.json {
		printRawJSON(raw)
		return 0
	}
	var r struct {
		DeletedCount    int      `json:"deleted_count"`
		DeletedKeypaths []string `json:"deleted_keypaths"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return cliFail("decode daemon reply: %v", err)
	}
	fmt.Fprintln(cliOut, p.green(fmt.Sprintf("deleted %d keypath(s)", r.DeletedCount)))
	for _, kp := range r.DeletedKeypaths {
		fmt.Fprintf(cliOut, "  %s\n", p.dim(kp))
	}
	return 0
}
