package main

import (
	"strings"
	"testing"
)

func TestRenderMarkdownFenceMarkerMatching(t *testing.T) {
	p := palette{on: true}
	content := "```\ncode\n~~~ not a toggle\nmore code\n```\nprose after"
	out := renderMarkdown(content, "", p)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	green := "\x1b[32m"
	if !strings.Contains(lines[2], green) || !strings.Contains(lines[3], green) {
		t.Errorf("~~~ inside a ``` fence should stay fenced content:\n%q", out)
	}
	if strings.Contains(lines[5], green) {
		t.Errorf("prose after the closing ``` should not be fenced:\n%q", out)
	}
}

func TestRenderMarkdownInlineCodeInsideBold(t *testing.T) {
	p := palette{on: true}
	out := renderMarkdown("**run `make test` first**", "", p)
	if strings.Contains(out, "\x1b[0m") {
		t.Errorf("inline spans must use targeted resets, not \\x1b[0m, so nesting survives:\n%q", out)
	}
	// Bold opens before the code span and closes after it.
	if !strings.Contains(out, "\x1b[1m") || !strings.Contains(out, "\x1b[22m") ||
		!strings.Contains(out, "\x1b[36m") || !strings.Contains(out, "\x1b[39m") {
		t.Errorf("expected bold [1m/[22m and code [36m/[39m spans:\n%q", out)
	}
}

func TestRenderMarkdownNoColor(t *testing.T) {
	content := "# h\n**b** `c`\n- item"
	out := renderMarkdown(content, "  ", palette{})
	if strings.Contains(out, "\x1b[") {
		t.Errorf("disabled palette must emit no escapes:\n%q", out)
	}
	if out != "  # h\n  **b** `c`\n  - item\n" {
		t.Errorf("content must pass through verbatim with indent:\n%q", out)
	}
}

func TestFlagAfterPositional(t *testing.T) {
	cases := []struct {
		raw, positionals []string
		want             bool
	}{
		{[]string{"proj", "--keys"}, []string{"proj", "--keys"}, true},
		{[]string{"proj", "notes.x"}, []string{"proj", "notes.x"}, false},
		{[]string{"--", "-foo"}, []string{"-foo"}, false},
		{[]string{"--limit", "3", "--", "--idle-timeout"}, []string{"--idle-timeout"}, false},
	}
	for _, c := range cases {
		if got := flagAfterPositional(c.raw, c.positionals); got != c.want {
			t.Errorf("flagAfterPositional(%v, %v) = %v, want %v", c.raw, c.positionals, got, c.want)
		}
	}
}
