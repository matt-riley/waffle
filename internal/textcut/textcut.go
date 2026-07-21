// Package textcut provides UTF-8-safe string truncation helpers.
//
// Byte-offset slices (s[:n]) can split multi-byte runes and produce invalid
// UTF-8. Callers that cap strings by byte budget should use Cut / CutSuffix
// so results remain valid UTF-8 and never exceed the requested length.
package textcut

import "unicode/utf8"

// Cut returns the longest prefix of s with len <= n that ends on a UTF-8
// rune boundary. Never splits a multi-byte rune. If n <= 0, returns "".
func Cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// s[n] is the first excluded byte; if it is a continuation byte we are
	// mid-rune and must retreat to the start of that rune.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// CutSuffix returns the longest suffix of s with len <= n that begins on a
// UTF-8 rune boundary. Never splits a multi-byte rune. If n <= 0, returns "".
func CutSuffix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	// Advance to the next rune start so the suffix does not begin mid-rune.
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
