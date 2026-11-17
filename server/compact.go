package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

// `memstated compact-hook` is the PreCompact and PostCompact hook for Claude
// Code. It reads the hook event on stdin. Nothing can start a compact for
// the user, and a hook cannot make the model write, so the summary of the
// compact is the save: before the compact the hook prints instructions
// that make the model write the summary as sections, and after the compact
// the hook files each section in the session's project as one memory.
//
// PreCompact always exits 0: exit 2 there stops the compact. PostCompact
// exits 2 when a part of the summary is not stored, because Claude Code
// then shows the reason on stderr to the user and to nobody else.

const (
	// Claude Code cuts the output of a hook at 10,000 characters. The
	// PreCompact output stays below compactMaxOutput UTF-16 units: the
	// user's text is cut first, then the list of keypath names.
	compactMaxOutput      = 9500
	compactMaxCustomRunes = 1500
	compactMaxSessionID   = 64
	compactTopic          = "compact"
	compactSessionCat     = "status"
)

// compactAreas maps each area that the hook files to the category of its
// memories. A "###" section of the summary below one of these areas is one
// memory at <area>.<topic>.
var compactAreas = map[string]string{
	"decisions": "decision",
	"gotchas":   "gotcha",
	"todo":      "status",
	"questions": "note",
	"notes":     "note",
}

// compactInstructionsText is the fixed part of the PreCompact output. The
// "##" headings are the names that ExtractHeadings maps to compactAreas.
const compactInstructionsText = `Write the summary for an agent that continues this work and has no other context.
Write only what the code and the git history cannot answer.
A hook files this summary in memstate, the memory of the project. It stores each "###" section as one memory, so use the headings below in this exact form. Write as many "###" sections as the handoff needs. Leave out a "##" section that has nothing in it.

## Session
The task as the user stated it. What is done, with its evidence: the tests that ran, the commits made. What is not done, and the exact next step.

## Decisions
### <topic>
One section for each choice made in this session, with its reason.

## Gotchas
### <topic>
One section for each trap found: what failed, the cause, the fix.

## Todo
### <topic>
One section for each unfinished item.

## Questions
### <topic>
One section for each open question for the user, with the context that an answer needs.

## Notes
### <topic>
One section for each fact that you had to look up and will need again: how a subsystem works, the command that verified a step, a URL.

Rules:
- <topic> is a short name in lowercase snake_case, for example "### port_choice". The memory gets the keypath <area>.<topic>, for example decisions.port_choice.
- Each "###" section must be complete without the others, because a later session reads it alone.
- Write a "###" section only for a fact that is new or that changed in this session. For a fact that memstate holds and that did not change, write nothing: no section, and no line that says where the fact is stored. The hook stores each section that you write, also one that says "not repeated here".
- A "###" section holds a fact about the project or the work. Leave out what a hook, a reminder or the system told you about the session itself: the working directory, the names of projects, modes, lists of tools.
- To update a fact that memstate holds, use the name of its keypath. For a new fact, use a name that memstate does not hold.
- It is correct to write no "###" section when the session has no such fact.
- Refer to code by symbol name, not by file and line.
- Leave out tool output, file contents and reasoning.
- Leave out each secret and each credential.
- Leave out each tool call that the user or a permission check denied: its tool name, its command, its arguments and the fact of the denial.
`

// compactNow is the clock of the keypath date. Tests replace it.
var compactNow = time.Now

// startDaemon starts the shared daemon. Tests replace it, because the
// executable of a test is not a daemon.
var startDaemon = startDetachedDaemon

// Claude Code 2.1.325 sends the summary as the model wrote it: an
// <analysis> block of scratch work, then the summary in <summary> tags.
var (
	compactAnalysisRE = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)
	compactSummaryRE  = regexp.MustCompile(`(?s)<summary>(.*)</summary>`)
)

// compactEvent holds the fields of the PreCompact and PostCompact hook
// input that the hook reads. A JSON null leaves a field empty.
type compactEvent struct {
	SessionID          string `json:"session_id"`
	Cwd                string `json:"cwd"`
	HookEventName      string `json:"hook_event_name"`
	Trigger            string `json:"trigger"`
	CustomInstructions string `json:"custom_instructions"`
	CompactSummary     string `json:"compact_summary"`
	AgentID            string `json:"agent_id"`
}

func cmdCompactHook(args []string) int {
	return runCompactHook(os.Stdin, os.Stdout, os.Stderr)
}

// runCompactHook is cmdCompactHook with injectable streams for tests.
func runCompactHook(stdin io.Reader, stdout, stderr io.Writer) int {
	var ev compactEvent
	if err := json.NewDecoder(stdin).Decode(&ev); err != nil {
		return 0
	}
	// A subagent that compacts has its own task. Its summary is not the
	// state of the session, so the hook leaves it alone.
	if ev.AgentID != "" {
		return 0
	}
	switch ev.HookEventName {
	case "PreCompact":
		// The names are a help for the model, not a condition: without a
		// daemon or a project the instructions go out without them.
		names, _ := compactKeypathNames(ev.Cwd)
		fmt.Fprint(stdout, compactInstructions(ev.CustomInstructions, names))
	case "PostCompact":
		if err := storeCompactSummary(ev); err != nil {
			fmt.Fprintf(stderr, "memstate: compact summary not stored: %v\n", err)
			return 2
		}
	}
	return 0
}

// compactInstructions returns the PreCompact output: the fixed text, the
// keypaths that the project holds in the areas of compactAreas, and the
// text that the user gave to /compact. The documentation does not say if
// Claude Code keeps the user's text when a hook prints instructions, so
// the hook repeats it. A second copy does no harm.
func compactInstructions(custom string, names []string) string {
	tail := ""
	if custom = strings.TrimSpace(custom); custom != "" {
		tail = "\nThe user gave these instructions for this compact:\n" +
			cutRunes(custom, compactMaxCustomRunes) + "\n"
	}
	var b strings.Builder
	b.WriteString(compactInstructionsText)
	if len(names) > 0 {
		const head = "\nKeypaths that memstate holds for this project:\n"
		const more = "(the list is cut here)\n"
		room := compactMaxOutput - utf16Len(compactInstructionsText) - utf16Len(tail) -
			utf16Len(head) - utf16Len(more)
		b.WriteString(head)
		for i, name := range names {
			line := safeInline(name) + "\n"
			if room -= utf16Len(line); room < 0 {
				if i == 0 {
					b.Reset()
					b.WriteString(compactInstructionsText)
				} else {
					b.WriteString(more)
				}
				break
			}
			b.WriteString(line)
		}
	}
	b.WriteString(tail)
	return b.String()
}

// utf16Len counts s in UTF-16 units, the unit of the cut in Claude Code.
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// compactKeypathNames returns the keypaths of the session's project that
// lie in the areas of compactAreas, sorted. The model needs them to update
// a fact under its name and to give a new fact a new name.
func compactKeypathNames(cwd string) ([]string, error) {
	addr, err := ensureSharedDaemon()
	if err != nil {
		return nil, err
	}
	project, err := compactProject(addr, cwd)
	if err != nil {
		return nil, err
	}
	raw, err := (&daemonClient{addr: addr}).call("POST", "/keypaths", keypathsReq{ProjectID: project})
	if err != nil {
		return nil, err
	}
	var out struct {
		Memories []struct {
			Keypath string `json:"keypath"`
		} `json:"memories"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range out.Memories {
		if _, ok := compactArea(m.Keypath); ok {
			names = append(names, m.Keypath)
		}
	}
	slices.Sort(names)
	return names, nil
}

// compactArea returns the category for a keypath that the hook files as
// its own memory: a keypath below one of compactAreas. The area alone is
// not such a keypath, so a hook never replaces `decisions` or `todo`.
func compactArea(keypath string) (category string, ok bool) {
	area, topic, found := strings.Cut(keypath, ".")
	if !found || topic == "" {
		return "", false
	}
	category, ok = compactAreas[area]
	return category, ok
}

// compactSummaryText returns the part of compact_summary that memstate
// keeps: the text in the <summary> tags, or the whole text without its
// <analysis> block when there are no such tags.
func compactSummaryText(raw string) string {
	text := compactAnalysisRE.ReplaceAllString(raw, "")
	if m := compactSummaryRE.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	return strings.TrimSpace(text)
}

// splitCompactSummary divides the summary with the heading rule of
// memstate_remember (ExtractHeadings). A section below one of compactAreas
// is a memory with its own keypath. Each other section is a part of the
// session text, under its keypath as the heading; text before the first
// heading comes first.
func splitCompactSummary(summary string) (session string, memories []Section) {
	var b strings.Builder
	for _, s := range ExtractHeadings(summary, "") {
		if _, ok := compactArea(s.Keypath); ok {
			memories = append(memories, s)
			continue
		}
		if s.Keypath != "preamble" {
			b.WriteString("## " + s.Keypath + "\n")
		}
		b.WriteString(s.Content + "\n\n")
	}
	return strings.TrimSpace(b.String()), memories
}

// storeCompactSummary files the summary through the shared daemon, so the
// name rules, the versioning and the embedding apply as for any write.
// Each section of an area goes to <area>.<topic> and supersedes a memory
// of that name, as a write by the model does. The session text goes to
// task.compact.<date> with the list of the keypaths that were filed; a
// second compact on the same day supersedes it. The history keeps each
// version. A section that the daemon refuses does not stop the others.
func storeCompactSummary(ev compactEvent) error {
	session, memories := splitCompactSummary(compactSummaryText(ev.CompactSummary))
	if session == "" && len(memories) == 0 {
		return nil
	}
	addr, err := ensureSharedDaemon()
	if err != nil {
		return err
	}
	project, err := compactProject(addr, ev.Cwd)
	if err != nil {
		return err
	}
	d := &daemonClient{addr: addr}
	source := compactSource(ev)
	store := func(keypath, content, category string) error {
		_, err := d.call("POST", "/memories/store", storeReq{
			ProjectID: project,
			Keypath:   keypath,
			Content:   content,
			Source:    source,
			Category:  category,
			Topics:    []string{compactTopic},
		})
		return err
	}
	total := len(memories)
	var filed []string
	var failed []error
	for _, m := range memories {
		category, _ := compactArea(m.Keypath)
		if err := store(m.Keypath, m.Content, category); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", safeInline(m.Keypath), err))
			continue
		}
		filed = append(filed, m.Keypath)
	}
	if len(filed) > 0 {
		session = strings.TrimSpace(session + "\n\n## filed\n- " + strings.Join(filed, "\n- "))
	}
	if session != "" {
		total++
		if err := store(compactKeypath(compactNow()), session, compactSessionCat); err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d memories: %w", len(failed), total, errors.Join(failed...))
	}
	return nil
}

// compactKeypath is the keypath of the session text for the day of t. It
// is not task.summary.<date>: the model writes that keypath at the end of
// a task, and a hook must not replace what the model wrote.
func compactKeypath(t time.Time) string {
	return "task.compact." + t.Format("2006_01_02")
}

// compactSource is the provenance line of the write. The trigger and the
// session id come from the hook input, so only known values and safe
// characters go into it.
func compactSource(ev compactEvent) string {
	source := "claude-code compact"
	if ev.Trigger == "manual" || ev.Trigger == "auto" {
		source = "claude-code " + ev.Trigger + " compact"
	}
	id := sessionIDRE.ReplaceAllString(ev.SessionID, "")
	if len(id) > compactMaxSessionID {
		id = id[:compactMaxSessionID]
	}
	if id != "" {
		source += ", session " + id
	}
	return source
}

// sharedDaemonAddr returns the address where the shared daemon of this
// user runs or must run, with the rule of the MCP proxy (ensureDaemon):
// MEMSTATE_ADDR, then the daemon.addr file of a daemon that answers, then
// the default address. The default address belongs to the default
// database, so a custom MEMSTATE_DB without a published daemon has none.
func sharedDaemonAddr() (string, error) {
	if addr, ok := discoverAddr(); ok {
		return addr, nil
	}
	if os.Getenv("MEMSTATE_DB") != "" {
		return "", errors.New("no shared daemon serves the database of MEMSTATE_DB; " +
			"start one with `memstated --addr HOST:PORT`, or set MEMSTATE_ADDR")
	}
	return defaultAddr, nil
}

// ensureSharedDaemon returns the address of the shared daemon and starts
// the daemon first when none answers there. A daemon with an idle timeout
// stops during a long turn, and a compact comes at the end of such a turn.
// The new daemon is this binary, detached, and reads the environment and
// config.env as a daemon that the proxy starts. An address of another
// machine gets no daemon from here.
func ensureSharedDaemon() (string, error) {
	addr, err := sharedDaemonAddr()
	if err != nil {
		return "", err
	}
	h, site := locateDaemon(addr)
	switch {
	case h != nil:
		return addr, nil
	case site == daemonElsewhere:
		return "", fmt.Errorf("no daemon answers at %s, and that address is not on this machine", addr)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := startDaemon(exe, addr, nil); err != nil {
		return "", fmt.Errorf("could not start the shared daemon: %w", err)
	}
	return addr, nil
}

// compactProject returns the project that gets the summary, or the reason
// why there is none. The rule is the write rule of the MCP proxy
// (checkWriteTarget): a write does not create a project, except the cwd
// project of a git repository, and the home directory has no project. A
// project that a live proxy pinned for this directory comes first, as in
// `memstated recall`, and it must exist.
func compactProject(addr, cwd string) (string, error) {
	if pinned := pinnedProject(cwd); pinned != "" {
		exists, err := recallProjectExists(addr, pinned)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", fmt.Errorf("the pinned project %q does not exist", safeInline(pinned))
		}
		return pinned, nil
	}
	_, inRepo := repoRoot(cwd)
	if inRepo {
		return deriveProject(cwd), nil
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(cwd) == filepath.Clean(home) {
		return "", fmt.Errorf("the home directory has no project")
	}
	project := deriveProject(cwd)
	exists, err := recallProjectExists(addr, project)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%s is not a git repository and the project %q does not exist",
			safeInline(filepath.Base(cwd)), safeInline(project))
	}
	return project, nil
}
