package main

import (
	"strings"
	"testing"
	"testing/quick"
)

func TestPreviewWords(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"one two", "one two"},
		{"a\nb\n\n  c", "a b c"},
		{"w1 w2 w3 w4", "w1 w2 w3 …"},
	}
	for _, c := range cases {
		if got := previewWords(c.in, 3); got != c.want {
			t.Errorf("previewWords(%q, 3) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The preview is a prefix of the content's words, never longer than n
// words, and marks a cut exactly when the content has more than n words.
func TestPreviewWordsProperty(t *testing.T) {
	prop := func(s string, n uint8) bool {
		limit := int(n%50) + 1
		words := strings.Fields(s)
		got := previewWords(s, limit)
		cut := len(words) > limit
		if cut != strings.HasSuffix(got, " …") {
			return false
		}
		got = strings.TrimSuffix(got, " …")
		gotWords := strings.Fields(got)
		if len(gotWords) > limit {
			return false
		}
		want := words
		if cut {
			want = words[:limit]
		}
		return strings.Join(gotWords, " ") == strings.Join(want, " ")
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
