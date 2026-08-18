package redact

import (
	"strings"
	"testing"
)

// FuzzResidual asserts scrubbing completes in a single pass. Projections are
// rendered once, so any pattern still matching the returned value is a secret
// format that reached the operator's screen.
func FuzzResidual(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain text",
		"key AGE-SECRET-KEY-abc123 tail",
		"sk-proj-xyz",
		"sk-AGE-SECRET-KEY-abc123",
		"/var/lib/waffle/workspace/main",
		"/var/lib/wafflesk-abc",
		"WAFFLE_AGE_IDENTITY=AGE-SECRET-KEY-abc",
		"[redacted]sk-",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		clean := Residual(value)
		for _, pattern := range residualPatterns {
			if pattern.MatchString(clean) {
				t.Fatalf("Residual(%q) = %q still matches %v", value, clean, pattern)
			}
		}
		if strings.Contains(clean, "WAFFLE_AGE_IDENTITY") {
			t.Fatalf("Residual(%q) = %q still names the identity env var", value, clean)
		}
		if again := Residual(clean); again != clean {
			t.Fatalf("Residual is not idempotent: %q -> %q -> %q", value, clean, again)
		}
	})
}

// FuzzExact asserts the exact-value scrubber removes every occurrence of a
// secret. The property is stated over secret-shaped values that the
// "[REDACTED]" marker does not itself contain, because two ways of finding
// the needle in the output say nothing about the secret: the marker can
// splice with neighbouring text to re-form it ("D]x" survives in "D]xx"), and
// the marker contains short needles outright ("E"). Age identities, sk-* keys
// and broker tokens all fall inside the stated domain.
func FuzzExact(f *testing.F) {
	for _, seed := range [][2]string{
		{"key sk-abc tail", "sk-abc"},
		{"aaa", "aa"},
		{"nothing to do", ""},
		{"AGE-SECRET-KEY-abc", "AGE-SECRET-KEY-abc"},
		{"wk_tokenwk_token", "wk_token"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, value, private string) {
		if private == "" {
			if got := Exact(value, private); got != value {
				t.Fatalf("Exact(%q, \"\") = %q, want the value unchanged", value, got)
			}
			return
		}
		if !secretShaped(private) || strings.Contains(marker, private) {
			return
		}
		if got := Exact(value, private); strings.Contains(got, private) {
			t.Fatalf("Exact(%q, %q) = %q, which still contains the secret", value, private, got)
		}
	})
}

// marker is the replacement Exact writes over every secret.
const marker = "[REDACTED]"

// secretShaped reports whether s is drawn from the alphabet Waffle's own
// credentials use. Every non-empty prefix of the marker starts with "[" and
// every non-empty suffix ends with "]", so no value in this alphabet can be
// formed by splicing the marker onto the text around it.
func secretShaped(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
