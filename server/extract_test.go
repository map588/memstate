package main

import (
	"reflect"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Auth", "auth"},
		{"  Auth Provider  ", "auth_provider"},
		{"API v2 / Endpoints!", "api_v2_endpoints"},
		{"`memstate_remember`", "memstate_remember"},
		{"", ""},
		{"!!!", ""},
		{"--hyphenated--title--", "hyphenated_title"},
	}
	for _, c := range cases {
		got := Slug(c.in)
		if got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractHeadings_FlatSections(t *testing.T) {
	md := `## Auth

Using SuperTokens.

## Database

Postgres 15.
`
	got := ExtractHeadings(md, "")
	want := []Section{
		{Keypath: "auth", Content: "Using SuperTokens."},
		{Keypath: "database", Content: "Postgres 15."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_PreambleCaptured(t *testing.T) {
	md := `Intro prose here.

## Auth

body
`
	got := ExtractHeadings(md, "root")
	want := []Section{
		{Keypath: "root.preamble", Content: "Intro prose here."},
		{Keypath: "root.auth", Content: "body"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_H1ContentFlushed(t *testing.T) {
	md := `# Title

Some lead paragraph.

## Auth

body
`
	got := ExtractHeadings(md, "")
	// "Some lead paragraph." comes between h1 and h2 → captured as preamble.
	want := []Section{
		{Keypath: "preamble", Content: "Some lead paragraph."},
		{Keypath: "auth", Content: "body"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_ReservedAliases(t *testing.T) {
	md := "## TODOs\n\na\n\n## Decisions\n\nb\n\n## Open Questions\n\nc\n\n## Files to touch\n\nd\n"
	got := ExtractHeadings(md, "")
	want := []Section{
		{Keypath: "todo", Content: "a"},
		{Keypath: "decisions", Content: "b"},
		{Keypath: "questions", Content: "c"},
		{Keypath: "files", Content: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_Nested(t *testing.T) {
	md := `## Auth

Top-level auth notes.

### Provider

SuperTokens.

### Session

TTL 15m.

## Database

PG.
`
	got := ExtractHeadings(md, "")
	want := []Section{
		{Keypath: "auth", Content: "Top-level auth notes."},
		{Keypath: "auth.provider", Content: "SuperTokens."},
		{Keypath: "auth.session", Content: "TTL 15m."},
		{Keypath: "database", Content: "PG."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_WithRoot(t *testing.T) {
	md := `## Auth

x
`
	got := ExtractHeadings(md, "project.my_app")
	want := []Section{
		{Keypath: "project.my_app.auth", Content: "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_IgnoresFencedCodeHashes(t *testing.T) {
	md := "## Real Heading\n\nBody.\n\n```\n## Not A Heading\nprint(1)\n```\n\n## Next\n\nMore.\n"
	got := ExtractHeadings(md, "")
	want := []Section{
		{Keypath: "real_heading", Content: "Body.\n\n```\n## Not A Heading\nprint(1)\n```"},
		{Keypath: "next", Content: "More."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_SkipsEmptySections(t *testing.T) {
	md := `## First

## Second

Has body.

## Third
`
	got := ExtractHeadings(md, "")
	want := []Section{
		{Keypath: "second", Content: "Has body."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_NoHeadings(t *testing.T) {
	// With preamble capture, prose-only content becomes a single "preamble"
	// section rather than being silently dropped.
	got := ExtractHeadings("Just prose.\nNothing structural.", "")
	want := []Section{
		{Keypath: "preamble", Content: "Just prose.\nNothing structural."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestExtractHeadings_DeeperNestingSkipLevel(t *testing.T) {
	// h2 followed by h4 (skipping h3) still nests one level deeper.
	md := `## A

a body

#### B

b body
`
	got := ExtractHeadings(md, "")
	want := []Section{
		{Keypath: "a", Content: "a body"},
		{Keypath: "a.b", Content: "b body"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
