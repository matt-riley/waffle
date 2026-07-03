package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverParsesFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "standup", `---
name: standup
description: Write my daily standup update
---

# Standup

Collect yesterday's commits and summarize them.
`)
	writeSkill(t, root, "bare", "Just instructions, no frontmatter.\n")
	// A directory without SKILL.md is skipped.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	skills, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(skills))
	}

	standup, ok := Find(skills, "standup")
	if !ok {
		t.Fatal("standup not found")
	}
	if standup.Description != "Write my daily standup update" {
		t.Errorf("description = %q", standup.Description)
	}
	body, err := standup.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.Contains(body, "---") || !strings.HasPrefix(body, "# Standup") {
		t.Errorf("body = %q", body)
	}

	bare, ok := Find(skills, "bare")
	if !ok {
		t.Fatal("bare not found")
	}
	if bare.Description != "" {
		t.Errorf("bare description = %q", bare.Description)
	}
	if body, _ := bare.Body(); !strings.Contains(body, "Just instructions") {
		t.Errorf("bare body = %q", body)
	}
}

func TestDiscoverMissingDir(t *testing.T) {
	skills, err := Discover(filepath.Join(t.TempDir(), "nope"))
	if err != nil || skills != nil {
		t.Errorf("missing dir = %v, %v; want nil, nil", skills, err)
	}
}

func TestIndex(t *testing.T) {
	if Index(nil) != "" {
		t.Error("empty index not empty")
	}
	got := Index([]Skill{{Name: "standup", Description: "daily update", Path: "/x/SKILL.md"}})
	if !strings.Contains(got, "standup: daily update (/x/SKILL.md)") {
		t.Errorf("index = %q", got)
	}
}
