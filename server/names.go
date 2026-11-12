package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The daemon is the trust boundary for the names in the store. The proxy
// and the Python scripts have stricter rules (snake_case, the creation
// gate), but every HTTP client and every import file reaches the store
// without them. Names appear later in the text that the recall hook puts
// into a session, so a name with a line break or markup is an injection.
//
// The rule is an alphabet, not the snake_case convention: stores hold
// keypaths with kebab dates and upper case from older clients, and heading
// extraction keeps letters of every script. All of those stay valid.

// ErrInvalidName marks a write whose project id, keypath, category, topic
// or source breaks the rules of checkWriteNames. Handlers map it to HTTP 400.
var ErrInvalidName = errors.New("invalid name")

// The Unicode line and paragraph separators break a line like "\n" does, but
// are not control characters. They are written as numbers because the
// characters are invisible in source.
const (
	lineSeparator      = rune(0x2028)
	paragraphSeparator = rune(0x2029)
)

const (
	maxProjectIDBytes = 128
	maxKeypathBytes   = 512
	maxLabelBytes     = 64 // a category, or one topic
	maxTopics         = 64
	maxSourceBytes    = 512
)

// isName reports whether s is one or more letters, digits, "_" or "-", in
// any script. Spaces, punctuation, control and format characters are not
// name characters.
func isName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '_' && r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// checkWriteNames refuses a write that would put an unsafe name into the
// store. A tombstone does not call it, so a name from an older daemon can
// still be deleted.
//
//   - project id: a name, at most maxProjectIDBytes.
//   - keypath: names separated by dots, at most maxKeypathBytes.
//   - category and each topic: a name, at most maxLabelBytes; at most
//     maxTopics topics.
//   - source: free text on one line, at most maxSourceBytes.
func checkWriteNames(projectID, keypath string, meta WriteMeta) error {
	if len(projectID) > maxProjectIDBytes || !isName(projectID) {
		return nameErr("project id", projectID,
			fmt.Sprintf("letters, digits, _ and -, at most %d bytes", maxProjectIDBytes))
	}
	if len(keypath) > maxKeypathBytes {
		return nameErr("keypath", keypath, fmt.Sprintf("at most %d bytes", maxKeypathBytes))
	}
	for _, segment := range strings.Split(keypath, ".") {
		if !isName(segment) {
			return nameErr("keypath", keypath, "segments of letters, digits, _ and -, separated by dots")
		}
	}
	if meta.Category != "" && (len(meta.Category) > maxLabelBytes || !isName(meta.Category)) {
		return nameErr("category", meta.Category,
			fmt.Sprintf("letters, digits, _ and -, at most %d bytes", maxLabelBytes))
	}
	if len(meta.Topics) > maxTopics {
		return fmt.Errorf("%w: %d topics: at most %d", ErrInvalidName, len(meta.Topics), maxTopics)
	}
	for _, topic := range meta.Topics {
		if len(topic) > maxLabelBytes || !isName(topic) {
			return nameErr("topic", topic,
				fmt.Sprintf("letters, digits, _ and -, at most %d bytes", maxLabelBytes))
		}
	}
	if len(meta.Source) > maxSourceBytes || !isOneLine(meta.Source) {
		return nameErr("source", meta.Source,
			fmt.Sprintf("one line of text, at most %d bytes", maxSourceBytes))
	}
	return nil
}

// safeName converts s to a name that isName accepts: each character that is
// not a name character becomes "_", and an empty string becomes "_". A
// valid name comes back unchanged. The length limit is not applied.
//
// Import uses it for the names that an older daemon accepted. Two names
// that differ only in such characters get the same converted name.
func safeName(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, s)
}

// safeKeypath converts a keypath to one that checkWriteNames accepts: each
// segment goes through safeName. The dots stay, so the number of segments
// does not change. A valid keypath comes back unchanged.
func safeKeypath(keypath string) string {
	segments := strings.Split(keypath, ".")
	for i, segment := range segments {
		segments[i] = safeName(segment)
	}
	return strings.Join(segments, ".")
}

// isOneLine reports whether s is valid UTF-8 with no control character and
// no line separator.
func isOneLine(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == lineSeparator || r == paragraphSeparator {
			return false
		}
	}
	return true
}

// nameErr builds the ErrInvalidName for one field. The value is quoted and
// cut, so the message is one short line whatever the value holds.
func nameErr(field, value, rule string) error {
	const show = 80
	if r := []rune(value); len(r) > show {
		value = string(r[:show]) + "..."
	}
	return fmt.Errorf("%w: %s %q: use %s", ErrInvalidName, field, value, rule)
}
