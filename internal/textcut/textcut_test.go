package textcut

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutASCII(t *testing.T) {
	if got := Cut("hello", 5); got != "hello" {
		t.Fatalf("Cut short = %q", got)
	}
	if got := Cut("hello world", 5); got != "hello" {
		t.Fatalf("Cut = %q, want hello", got)
	}
	if got := Cut("hello", 0); got != "" {
		t.Fatalf("Cut n=0 = %q", got)
	}
	if got := Cut("hello", -1); got != "" {
		t.Fatalf("Cut n<0 = %q", got)
	}
}

func TestCutMidRune(t *testing.T) {
	// 🌍 is U+1F30D, 4 UTF-8 bytes: F0 9F 8C 8D
	s := "hi 🌍🌍🌍 world"
	// Index of first emoji is 3; force a cut mid-first-emoji (byte 4 or 5).
	for _, n := range []int{4, 5, 6} {
		got := Cut(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("Cut(%q, %d) = %q invalid UTF-8", s, n, got)
		}
		if len(got) > n {
			t.Fatalf("Cut(%q, %d) len=%d > limit", s, n, len(got))
		}
		if got != "hi " {
			t.Fatalf("Cut(%q, %d) = %q, want %q", s, n, got, "hi ")
		}
	}
}

func TestCutCafeEmoji(t *testing.T) {
	// é is 2 bytes (C3 A9); limit mid-é must drop it.
	s := "café"
	got := Cut(s, 4) // "caf" + first byte of é
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if got != "caf" {
		t.Fatalf("Cut = %q, want caf", got)
	}
	if len(got) > 4 {
		t.Fatalf("len=%d > 4", len(got))
	}
}

func TestCutSuffixMidRune(t *testing.T) {
	s := "hi 🌍🌍🌍 world"
	// Tail budget that lands mid-emoji from the end.
	// " world" is 6 bytes; before that is emoji.
	for _, n := range []int{7, 8, 9} {
		got := CutSuffix(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("CutSuffix(%q, %d) = %q invalid UTF-8", s, n, got)
		}
		if len(got) > n {
			t.Fatalf("CutSuffix(%q, %d) len=%d > limit", s, n, len(got))
		}
		if !strings.HasSuffix(s, got) {
			t.Fatalf("CutSuffix result %q is not a suffix of %q", got, s)
		}
	}
}

func TestCutSuffixShortAndEmpty(t *testing.T) {
	if got := CutSuffix("hello", 10); got != "hello" {
		t.Fatalf("CutSuffix short = %q", got)
	}
	if got := CutSuffix("hello", 0); got != "" {
		t.Fatalf("CutSuffix n=0 = %q", got)
	}
	// Single multi-byte rune longer than budget → empty suffix after snap.
	if got := CutSuffix("🌍", 2); got != "" {
		t.Fatalf("CutSuffix small budget = %q, want empty", got)
	}
}

func TestCutNeverExceedsLimit(t *testing.T) {
	inputs := []string{
		"plain ascii text",
		"café crème",
		"hi 🌍🌍🌍 world",
		strings.Repeat("日本語", 20),
		"a" + strings.Repeat("🎉", 50) + "z",
	}
	for _, s := range inputs {
		for n := 0; n <= len(s)+2; n++ {
			got := Cut(s, n)
			if !utf8.ValidString(got) {
				t.Fatalf("Cut(%q, %d) invalid: %q", s, n, got)
			}
			if len(got) > n && n >= 0 {
				t.Fatalf("Cut(%q, %d) len=%d", s, n, len(got))
			}
			suf := CutSuffix(s, n)
			if !utf8.ValidString(suf) {
				t.Fatalf("CutSuffix(%q, %d) invalid: %q", s, n, suf)
			}
			if len(suf) > n && n >= 0 {
				t.Fatalf("CutSuffix(%q, %d) len=%d", s, n, len(suf))
			}
		}
	}
}
