package main

import "strings"

// previewWordCount is the length of the preview that search results and
// write responses carry instead of the content. The caller reads the full
// memory with a get on its keypath, or a prior version with history.
const previewWordCount = 40

// previewWords returns the first n whitespace-separated words of s joined
// by single spaces. A cut is marked with an ellipsis.
func previewWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + " …"
}
