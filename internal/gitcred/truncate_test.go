package gitcred

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateSplitsOnRuneBoundary(t *testing.T) {
	// "é" is two bytes (U+00E9). Cutting at byte 3 of a 4-byte string
	// "aé b" would land mid-rune with a raw slice.
	s := "aé b"
	got := truncate(s, 3)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate(%q, 3) = %q: invalid UTF-8", s, got)
	}
	if len(got) > 3+len("…") {
		t.Fatalf("truncate(%q, 3) = %q: exceeds n + ellipsis", s, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncate(%q, 3) = %q: missing ellipsis", s, got)
	}
}

func TestTruncateShortStringUntouched(t *testing.T) {
	got := truncate("short", 10)
	if got != "short" {
		t.Fatalf("truncate short string = %q, want %q", got, "short")
	}
}
