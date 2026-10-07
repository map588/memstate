package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// `memstated recall` is the UserPromptSubmit hook for Claude Code. It reads
// the hook event on stdin, searches the project's memories with the prompt
// text, and prints the best unseen hits so they land in the model's context
// before it answers. Every failure path exits 0 with nothing on stdout: a
// hook that fails must never block the prompt.

const (
	recallMinWords  = 4   // shorter prompts ("yes", "continue") carry no topic
	recallMaxHits   = 3   // hits printed per prompt
	recallMaxChars  = 500 // content cut per hit
	recallSearchLim = 10  // candidates fetched, so dedupe still leaves hits
	recallTimeout   = 3 * time.Second
	recallSeenTTL   = 7 * 24 * time.Hour
)

type hookEvent struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
}

type recallHit struct {
	ProjectID string   `json:"project_id"`
	Keypath   string   `json:"keypath"`
	Content   string   `json:"content"`
	Category  string   `json:"category"`
	Sources   []string `json:"sources"`
}

var (
	slugRE      = regexp.MustCompile(`[^a-z0-9]+`)
	sessionIDRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

func cmdRecall(args []string) int {
	return runRecall(os.Stdin, os.Stdout)
}

// runRecall is cmdRecall with injectable streams for tests.
func runRecall(stdin io.Reader, stdout io.Writer) int {
	debug := func(format string, a ...any) {
		if os.Getenv("MEMSTATE_RECALL_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "memstated recall: "+format+"\n", a...)
		}
	}
	if os.Getenv("MEMSTATE_NO_RECALL") != "" {
		return 0
	}
	var ev hookEvent
	if err := json.NewDecoder(stdin).Decode(&ev); err != nil {
		debug("decode hook event: %v", err)
		return 0
	}
	if !recallEligible(ev.Prompt) {
		return 0
	}
	addr, ok := discoverAddr()
	if !ok {
		debug("no shared daemon found")
		return 0
	}
	// The cwd project is the default. A live proxy in this directory may
	// have pinned another project; recall follows the pin, the scope block
	// keeps describing the directory.
	cwdProject := deriveProject(ev.Cwd)
	project := cwdProject
	if pinned := pinnedProject(ev.Cwd); pinned != "" {
		project = pinned
	}
	hits, err := recallSearch(addr, project, ev.Prompt)
	if err != nil {
		debug("search: %v", err)
		return 0
	}
	seenPath := recallSeenPath(ev.SessionID)
	seen := loadSeen(seenPath)

	// First eligible prompt of the session: show the cwd project and the
	// other projects this prompt matches, so the model can judge whether
	// the work belongs elsewhere. The marker in the seen file makes this a
	// one-time block.
	var out strings.Builder
	var shown []string
	if !seen[scopeMarker] {
		all, err := recallSearch(addr, "", ev.Prompt)
		if err != nil {
			debug("all-projects search: %v", err)
			all = nil
		}
		exists, err := recallProjectExists(addr, cwdProject)
		if err != nil {
			debug("projects: %v", err)
		}
		out.WriteString(scopeBlock(cwdProject, ev.Cwd, exists, projectCandidates(all, cwdProject)))
		shown = append(shown, scopeMarker)
	}

	// The user scope is a bonus: a failure there must not hide project hits.
	userHits, err := recallSearch(addr, userProject, ev.Prompt)
	if err != nil {
		debug("user scope search: %v", err)
		userHits = nil
	}
	userHits = filterHostHits(userHits, hostSlug())
	text, hitKeys := renderRecall(project, hits, userHits, seen, recallMaxHits, recallMaxChars)
	out.WriteString(text)
	shown = append(shown, hitKeys...)
	if out.Len() == 0 {
		return 0
	}
	fmt.Fprint(stdout, out.String())
	if err := appendSeen(seenPath, shown); err != nil {
		debug("record seen: %v", err)
	}
	pruneSeen(filepath.Dir(seenPath), recallSeenTTL)
	return 0
}

// recallEligible reports whether a prompt carries enough words to search on.
func recallEligible(prompt string) bool {
	return len(strings.Fields(prompt)) >= recallMinWords
}

// repoRoot returns the top-level directory of the git repository that
// contains cwd, and false when cwd is not inside a repository.
func repoRoot(cwd string) (string, bool) {
	if cwd == "" {
		return "", false
	}
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(out))
	return root, root != ""
}

// deriveProject maps a working directory to a project id with the same rule
// as the TS proxy and the Python skill: the git repository name (the
// directory name outside a repository), lowercased, with every run of
// characters outside [a-z0-9] replaced by "_" and edge underscores trimmed.
func deriveProject(cwd string) string {
	base := ""
	if root, ok := repoRoot(cwd); ok {
		base = filepath.Base(root)
	}
	if base == "" {
		base = filepath.Base(cwd)
	}
	return slugProject(base)
}

// scopeMarker is the seen-file line that records that the scope block was
// shown to this session. It never collides with a keypath.
const scopeMarker = "#scope"

// projectCandidate is another project whose memories match the prompt.
type projectCandidate struct {
	ProjectID string
	Hits      int // hits the semantic side returned
}

// projectCandidates groups all-project hits by project, drops the cwd
// project and the user scope, and counts only hits the semantic side
// returned. The FTS side of hybrid matches any word, so "config" or "set"
// pulls unrelated projects; a cosine match above the threshold is the
// evidence that a project is about the prompt. At most three, by hit count.
func projectCandidates(hits []recallHit, cwdProject string) []projectCandidate {
	byProject := map[string]*projectCandidate{}
	var order []string
	for _, h := range hits {
		if h.ProjectID == "" || h.ProjectID == cwdProject || h.ProjectID == userProject {
			continue
		}
		if !slices.Contains(h.Sources, "semantic") {
			continue
		}
		c, ok := byProject[h.ProjectID]
		if !ok {
			c = &projectCandidate{ProjectID: h.ProjectID}
			byProject[h.ProjectID] = c
			order = append(order, h.ProjectID)
		}
		c.Hits++
	}
	var out []projectCandidate
	for _, id := range order {
		out = append(out, *byProject[id])
	}
	slices.SortStableFunc(out, func(a, b projectCandidate) int {
		if a.Hits != b.Hits {
			return b.Hits - a.Hits
		}
		return strings.Compare(a.ProjectID, b.ProjectID)
	})
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// scopeBlock is the first-prompt block: the cwd project, how it was derived,
// the other projects the prompt matches, and the rule for overriding. The
// cwd stays the default; the model overrides only on clear evidence, and a
// new name is a deliberate second step (new_project=true in the proxy).
func scopeBlock(project, cwd string, exists bool, cands []projectCandidate) string {
	var b strings.Builder
	root, inRepo := repoRoot(cwd)
	home, err := os.UserHomeDir()
	isHome := !inRepo && err == nil && filepath.Clean(cwd) == filepath.Clean(home)
	// No write can land in the home directory's project, so whether it
	// exists is noise there.
	if isHome {
		fmt.Fprintf(&b, "<memstate-scope cwd_project=%q>\n", project)
	} else {
		fmt.Fprintf(&b, "<memstate-scope cwd_project=%q exists=\"%t\">\n", project, exists)
	}
	if inRepo {
		fmt.Fprintf(&b, "The working directory is the git repository %s.\n", filepath.Base(root))
	} else if isHome {
		// The home directory names the user, not a project. The proxy
		// refuses writes to its default there; say so before the first write.
		b.WriteString("The working directory is not a git repository (home directory). " +
			"There is no default project for writes here: pin one with project_name, " +
			"or use scope=\"user\" for facts about this machine.\n")
	} else {
		fmt.Fprintf(&b, "The working directory is not a git repository (%s).", filepath.Base(cwd))
		if !exists {
			fmt.Fprintf(&b, " Project %q does not exist yet; a write creates it only with new_project=true.", project)
		}
		b.WriteString("\n")
	}
	if len(cands) == 0 {
		b.WriteString("Prompt matches no other project.\n")
	} else {
		parts := make([]string, len(cands))
		for i, c := range cands {
			s := fmt.Sprintf("%s (%d semantic hit", c.ProjectID, c.Hits)
			if c.Hits != 1 {
				s += "s"
			}
			parts[i] = s + ")"
		}
		fmt.Fprintf(&b, "Prompt matches other projects: %s.\n", strings.Join(parts, ", "))
	}
	// The rule sentence must agree with the line above it: the home
	// directory has no default for writes, every other directory does.
	if isHome {
		b.WriteString("Reads use the cwd project. Every write needs project_name=<one of the ids " +
			"above, or another id from memstate_get(list_projects=true)> or scope=\"user\". " +
			"Do not invent a new name unless nothing fits; then add new_project=true.\n")
	} else {
		b.WriteString("Default is the cwd project. Override only when this prompt is clearly about " +
			"another subject: pass project_name=<one of the ids above, or another id from " +
			"memstate_get(list_projects=true)> on your first memstate call. Do not invent a " +
			"new name unless nothing fits; then add new_project=true.\n")
	}
	b.WriteString("</memstate-scope>\n")
	return b.String()
}

func slugProject(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if s == "" {
		return "default"
	}
	return s
}

// recallSearch runs a hybrid search on the daemon at addr. Any non-200
// reply is an error, including "unknown mode" from a daemon that predates
// hybrid search.
// pinnedProject returns the project that a live MCP proxy running in cwd
// pinned with project_name, or "". The proxy writes one file per process
// under <db dir>/recall/pins, named by its PID and holding "cwd\nproject\n",
// and removes it on exit. A file whose PID is dead is pruned here. When
// several live proxies share a cwd, the newest file wins.
func pinnedProject(cwd string) string {
	dir := filepath.Join(filepath.Dir(defaultDBPath()), "recall", "pins")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !processAlive(pid) {
			_ = os.Remove(path)
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.SplitN(strings.TrimRight(string(b), "\n"), "\n", 2)
		if len(lines) != 2 || lines[1] == "" || !sameDir(lines[0], cwd) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = lines[1], info.ModTime()
		}
	}
	return best
}

// sameDir compares two directory paths with symlinks resolved, so a cwd
// reported as /var/x and one as /private/var/x (macOS) are equal.
func sameDir(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Clean(r)
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// recallProjectExists reports whether the daemon lists project as live.
func recallProjectExists(addr, project string) (bool, error) {
	client := &http.Client{Timeout: recallTimeout}
	resp, err := client.Get("http://" + addr + "/api/v1/projects")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	for _, p := range out.Projects {
		if p.ID == project {
			return true, nil
		}
	}
	return false, nil
}

func recallSearch(addr, project, prompt string) ([]recallHit, error) {
	req := map[string]any{
		"query":           prompt,
		"mode":            "hybrid",
		"limit":           recallSearchLim,
		"include_content": true,
	}
	if project != "" {
		req["project_id"] = project
	}
	body, _ := json.Marshal(req)
	client := &http.Client{Timeout: recallTimeout}
	resp, err := client.Post("http://"+addr+"/api/v1/memories/search",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Results []recallHit `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// userSeenPrefix scopes seen-file keys for user-scope hits, so a project
// keypath with the same name is not suppressed by them.
const userSeenPrefix = userProject + "/"

// filterHostHits drops user-scope hits that describe another machine:
// anything under host.<slug> where slug is not this host.
func filterHostHits(hits []recallHit, host string) []recallHit {
	out := hits[:0:0]
	for _, h := range hits {
		if !isOtherHost(h.Keypath, host) {
			out = append(out, h)
		}
	}
	return out
}

// pickRecall selects up to max unseen hits. Hits ranked below max fill the
// slots that seen hits free up, but only when the semantic side returned
// them: an FTS-only hit that deep is a common-word match, not a topic match.
func pickRecall(hits []recallHit, seen map[string]bool, seenPrefix string, max int) []recallHit {
	var out []recallHit
	for i, h := range hits {
		if len(out) == max {
			break
		}
		if seen[seenPrefix+h.Keypath] {
			continue
		}
		if i >= max && !slices.Contains(h.Sources, "semantic") {
			continue
		}
		out = append(out, h)
	}
	return out
}

// renderRecall formats up to maxHits hits from the project and the user
// scope. One slot is reserved for the best user-scope hit, marked [user];
// project hits fill the rest. It returns the block and the seen-file keys it
// printed. An empty block means nothing new to show.
func renderRecall(project string, hits, userHits []recallHit, seen map[string]bool, maxHits, maxChars int) (string, []string) {
	user := pickRecall(userHits, seen, userSeenPrefix, 1)
	proj := pickRecall(hits, seen, "", maxHits-len(user))
	if len(user)+len(proj) == 0 {
		return "", nil
	}
	var b strings.Builder
	var shown []string
	fmt.Fprintf(&b, "<memstate-recall project=%q>\n", project)
	b.WriteString("Memories related to this prompt. Call memstate_get(keypath) for full content.\n\n")
	write := func(h recallHit, marker, seenKey string) {
		fmt.Fprintf(&b, "### %s%s", h.Keypath, marker)
		if h.Category != "" {
			fmt.Fprintf(&b, " [%s]", h.Category)
		}
		b.WriteString("\n")
		b.WriteString(cutRunes(strings.TrimSpace(h.Content), maxChars))
		b.WriteString("\n\n")
		shown = append(shown, seenKey)
	}
	for _, h := range proj {
		write(h, "", h.Keypath)
	}
	for _, h := range user {
		write(h, " [user]", userSeenPrefix+h.Keypath)
	}
	b.WriteString("</memstate-recall>\n")
	return b.String(), shown
}

// cutRunes shortens s to max runes and marks the cut.
func cutRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "[…truncated]"
}

// recallSeenPath is the per-session file of keypaths already injected,
// kept next to the database so MEMSTATE_DB moves it too.
func recallSeenPath(sessionID string) string {
	id := sessionIDRE.ReplaceAllString(sessionID, "")
	if id == "" {
		id = "no_session"
	}
	return filepath.Join(filepath.Dir(defaultDBPath()), "recall", id)
}

func loadSeen(path string) map[string]bool {
	seen := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return seen
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			seen[line] = true
		}
	}
	return seen
}

func appendSeen(path string, keypaths []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(keypaths, "\n") + "\n")
	return err
}

// pruneSeen deletes seen files older than ttl so finished sessions do not
// accumulate forever.
func pruneSeen(dir string, ttl time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
