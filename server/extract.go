package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Section is one extracted (keypath, content) pair from a markdown document.
type Section struct {
	Keypath string
	Content string
}

// ATX headings with optional trailing hashes: `## Title` or `## Title ##`.
var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

// ExtractHeadings scans markdown and emits one Section per h2+ heading.
// Deeper headings nest as dot segments under shallower ones. H1 is treated
// as a document title and contributes no keypath segment. Content of a
// section is every line from its heading to the next heading of the same
// or shallower level. Sections with empty content are dropped.
//
// Any non-empty prose appearing before the first h2+ heading is captured
// as a synthetic "preamble" section so it is not silently discarded.
// Common section names are normalized via aliasSlug so equivalent phrasings
// collapse to a canonical keypath (see reservedAliases).
//
// If root is non-empty, each extracted keypath is prefixed with it.
// Headings inside fenced code blocks (``` or ~~~) are ignored.
func ExtractHeadings(content, root string) []Section {
	root = strings.TrimSpace(root)
	lines := strings.Split(content, "\n")

	type frame struct {
		level int
		slug  string
	}
	var stack []frame
	var out []Section
	var buf strings.Builder
	var curKey string
	inFence := false

	prefix := func(seg string) string {
		if root == "" {
			return seg
		}
		if seg == "" {
			return root
		}
		return root + "." + seg
	}

	flush := func() {
		body := strings.TrimSpace(buf.String())
		buf.Reset()
		if body == "" {
			return
		}
		key := curKey
		if key == "" {
			key = prefix("preamble")
		}
		out = append(out, Section{Keypath: key, Content: body})
	}

	keyFromStack := func() string {
		parts := make([]string, 0, len(stack))
		for _, f := range stack {
			parts = append(parts, f.slug)
		}
		return prefix(strings.Join(parts, "."))
	}

	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			inFence = !inFence
			buf.WriteString(ln)
			buf.WriteByte('\n')
			continue
		}
		if inFence {
			buf.WriteString(ln)
			buf.WriteByte('\n')
			continue
		}
		m := headingRe.FindStringSubmatch(ln)
		if m == nil {
			buf.WriteString(ln)
			buf.WriteByte('\n')
			continue
		}
		level := len(m[1])
		title := strings.TrimSpace(m[2])

		flush()
		curKey = ""

		if level == 1 {
			stack = stack[:0]
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		sl := aliasSlug(Slug(title))
		if sl == "" {
			continue
		}
		stack = append(stack, frame{level: level, slug: sl})
		curKey = keyFromStack()
	}
	flush()
	return out
}

// reservedAliases maps common section-name slugs to canonical forms so
// that equivalent phrasings converge on the same keypath.
var reservedAliases = map[string]string{
	"todo":           "todo",
	"todos":          "todo",
	"to_do":          "todo",
	"to_dos":         "todo",
	"decision":       "decisions",
	"decisions":      "decisions",
	"question":       "questions",
	"questions":      "questions",
	"open_question":  "questions",
	"open_questions": "questions",
	"file":           "files",
	"files":          "files",
	"files_to_touch": "files",
	"files_touched":  "files",
	"note":           "notes",
	"notes":          "notes",
	"gotcha":         "gotchas",
	"gotchas":        "gotchas",
}

func aliasSlug(s string) string {
	if canonical, ok := reservedAliases[s]; ok {
		return canonical
	}
	return s
}

// Slug converts a heading title to a keypath segment: lowercase, runs of
// non-alphanumeric characters collapsed to single underscore, trimmed.
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if b.Len() > 0 && !lastUnderscore {
			b.WriteRune('_')
			lastUnderscore = true
		}
	}
	return strings.TrimRight(b.String(), "_")
}
