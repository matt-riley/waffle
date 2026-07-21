package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillsListJSONOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()

	// skills ls discovers under $WAFFLE_HOME/workspace/main/skills.
	activeDir := filepath.Join(home, "workspace", "main", "skills", "ship-it")
	if err := os.MkdirAll(activeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "SKILL.md"), []byte(`---
name: ship-it
description: release checklist
status: active
---

body
`), 0o600); err != nil {
		t.Fatal(err)
	}
	inactiveDir := filepath.Join(home, "workspace", "main", "skills", "draft")
	if err := os.MkdirAll(inactiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inactiveDir, "SKILL.md"), []byte(`---
name: draft
description: work in progress
status: inactive
---

body
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := skillsCmd(ctx, []string{"ls", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("skills ls --json: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %s", stdout.String())
	}
	var list []skillJSON
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byName := map[string]skillJSON{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("skills = %+v, want two", list)
	}
	if !byName["ship-it"].Active || byName["ship-it"].Description != "release checklist" {
		t.Fatalf("ship-it = %+v", byName["ship-it"])
	}
	if byName["draft"].Active || byName["draft"].Description != "work in progress" {
		t.Fatalf("draft = %+v", byName["draft"])
	}
}

func TestSkillsListJSONEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := skillsCmd(ctx, []string{"ls", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var list []skillJSON
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("list = %+v, want empty non-nil array", list)
	}
}
