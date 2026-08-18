package policy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzSplitCommand pins the invariants the action policy engine leans on.
// Bash allow/deny rules are matched against these tokens, so an empty token,
// a token carrying invalid UTF-8, or disagreement with the shell's own word
// splitting on unquoted input all change which rule a command matches.
func FuzzSplitCommand(f *testing.F) {
	for _, seed := range []string{
		"",
		"   ",
		"git status",
		`echo "hello world" 'x y' z`,
		`rm\ -rf /`,
		"git\tcommit\n-m x",
		"echo 'unterminated",
		`trailing escape \`,
		"go test ./... -run TestX",
		"\xff\xfe not utf8",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, cmd string) {
		tokens := SplitCommand(cmd)
		for i, token := range tokens {
			if token == "" {
				t.Fatalf("SplitCommand(%q) produced empty token at %d: %q", cmd, i, tokens)
			}
			if !utf8.ValidString(token) {
				t.Fatalf("SplitCommand(%q) produced invalid UTF-8 at %d: %q", cmd, i, token)
			}
		}

		// Without quotes or escapes there is no shell syntax to interpret, so
		// the splitter must agree with strings.Fields. Invalid UTF-8 is
		// excluded: SplitCommand decodes runes and so normalises bad bytes to
		// U+FFFD, while strings.Fields preserves them byte for byte.
		if strings.ContainsAny(cmd, "'\"\\") || !utf8.ValidString(cmd) {
			return
		}
		fields := strings.Fields(cmd)
		if len(fields) != len(tokens) {
			t.Fatalf("SplitCommand(%q) = %q, want strings.Fields %q", cmd, tokens, fields)
		}
		for i := range fields {
			if fields[i] != tokens[i] {
				t.Fatalf("SplitCommand(%q) token %d = %q, want %q", cmd, i, tokens[i], fields[i])
			}
		}
	})
}

// FuzzMatchBashPrefix guards the direction that matters for a deny rule: a
// command whose leading tokens are exactly the rule's prefix must be reported
// as a match. A false negative here is rule evasion, not just a missed hit.
func FuzzMatchBashPrefix(f *testing.F) {
	for _, seed := range [][2]string{
		{"git status", "git"},
		{`git   status`, "git status"},
		{`git "status"`, "git status"},
		{"gitx status", "git"},
		{"git status", "  "},
		{"git status", "''"},
		{"\xff", "\xff"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, cmd, prefix string) {
		want := SplitCommand(prefix)
		if len(want) == 0 {
			// A prefix with no tokens anchors nothing; MatchBashPrefix
			// deliberately refuses to match on it.
			return
		}
		got := SplitCommand(cmd)
		if len(got) < len(want) {
			return
		}
		for i := range want {
			if got[i] != want[i] {
				return
			}
		}
		if !MatchBashPrefix(cmd, prefix) {
			t.Fatalf("MatchBashPrefix(%q, %q) = false, but tokens %q lead with %q", cmd, prefix, got, want)
		}
	})
}
