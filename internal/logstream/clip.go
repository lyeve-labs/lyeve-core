package logstream

import (
	"strings"
	"unicode/utf8"
)

// The rings are bounded by entry count only, so without a size cap one caller
// that logs a large value grows every ring by that value times its length.
const (
	// MaxValueBytes caps each attribute value an entry keeps.
	MaxValueBytes = 1 << 10
	// MaxMessageBytes caps the message an entry keeps.
	MaxMessageBytes = 8 << 10
)

// Clip returns a copy of s cut to at most n bytes, ending on a whole rune.
//
// The copy matters as much as the cut. net/http parses the method and the path
// as substrings of the request line, so an entry holding either one as passed
// keeps the whole line, query string included, for as long as the entry lives.
func Clip(s string, n int) string {
	if len(s) > n {
		i := n
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	return strings.Clone(s)
}
