package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runnerIsIntentionallyUnlisted documents the one command the reference covers
// that `waffle help` does not: runner is the in-container entrypoint the
// sandbox executes, not something an operator invokes.
const runnerIsIntentionallyUnlisted = "runner"

func cliReferencePath() string {
	return filepath.Join("..", "..", "website", "src", "content", "docs", "docs", "reference", "cli.md")
}

// commandsFromUsage reads the command list out of the real usage text rather
// than re-listing it here, so the test cannot drift from the binary the way a
// hand-maintained copy would.
func commandsFromUsage(t *testing.T) []string {
	t.Helper()

	var output bytes.Buffer
	usage(&output)

	var commands []string
	inCommands := false
	// Command lines are indented exactly two spaces; continuation lines for a
	// long description are indented further and must not be mistaken for one.
	entry := regexp.MustCompile(`^ {2}(\S+)\s`)
	for _, line := range strings.Split(output.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "Commands:"):
			inCommands = true
		case inCommands && strings.TrimSpace(line) == "":
			inCommands = false
		case inCommands:
			if m := entry.FindStringSubmatch(line); m != nil {
				commands = append(commands, m[1])
			}
		}
	}

	if len(commands) == 0 {
		t.Fatalf("parsed no commands from usage text:\n%s", output.String())
	}
	return commands
}

func headingsFromReference(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(cliReferencePath())
	if err != nil {
		t.Fatalf("read CLI reference: %v", err)
	}

	var headings []string
	heading := regexp.MustCompile(`^## (\S+)\s*$`)
	for _, line := range strings.Split(string(body), "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			headings = append(headings, m[1])
		}
	}

	if len(headings) == 0 {
		t.Fatal("parsed no headings from the CLI reference")
	}
	return headings
}

// TestCLIReferenceCoversEveryCommand is the drift guard for the published
// command reference: a new subcommand that ships without documentation fails
// here, and so does a reference entry for a command that does not exist.
func TestCLIReferenceCoversEveryCommand(t *testing.T) {
	commands := commandsFromUsage(t)
	headings := headingsFromReference(t)

	documented := make(map[string]bool, len(headings))
	for _, h := range headings {
		documented[h] = true
	}

	for _, command := range commands {
		if !documented[command] {
			t.Errorf("waffle %s has no entry in the CLI reference (%s)", command, cliReferencePath())
		}
	}

	listed := make(map[string]bool, len(commands))
	for _, command := range commands {
		listed[command] = true
	}
	listed[runnerIsIntentionallyUnlisted] = true

	for _, heading := range headings {
		if !listed[heading] {
			t.Errorf("CLI reference documents %q, which is not a command in `waffle help`", heading)
		}
	}

	// Allowing the exception is not the same as requiring it. Without this,
	// deleting the runner entry would leave the guard silently satisfied.
	if !documented[runnerIsIntentionallyUnlisted] {
		t.Errorf(
			"CLI reference has no %q entry; it is absent from `waffle help` on purpose, so nothing else would catch its removal",
			runnerIsIntentionallyUnlisted,
		)
	}
}
