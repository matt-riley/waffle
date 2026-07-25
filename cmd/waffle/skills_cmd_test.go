package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
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

func TestSkillsListRecoversPendingUninstallBeforeDiscoveringSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	skillsDir := filepath.Join(home, "workspace", "main", "skills")
	skillDir := filepath.Join(skillsDir, "reviewer")
	backup := filepath.Join(skillsDir, ".waffle-uninstall-reviewer")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: reviewer\ndescription: review changes\nstatus: inactive\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := skill.SetSkillStatusRecord(ctx, st.DB, skill.StatusRecord{Name: "reviewer", Status: skill.StatusInactive, Source: "dashboard", SourceRef: "local:reviewer"}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(skillDir, backup); err != nil {
		t.Fatal(err)
	}
	journal := map[string]any{
		"version":   1,
		"name":      "reviewer",
		"skill_dir": skillDir,
		"backup":    backup,
		"parent":    skillsDir,
		"previous_status": map[string]any{
			"Name": "reviewer", "Status": skill.StatusInactive, "Source": "dashboard", "SourceRef": "local:reviewer",
		},
		"phase": "prepared",
	}
	journalBytes, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, ".waffle-uninstall-reviewer.json"), append(journalBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := skillsCmd(ctx, []string{"ls", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("skills ls --json recovery: %v", err)
	}
	var list []skillJSON
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "reviewer" || list[0].Active {
		t.Fatalf("recovered skills = %#v, want inactive reviewer", list)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatalf("recovery did not restore skill before list: %v", err)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery backup = %v, want absent", err)
	}
}

func TestOpenSkillsWorkspaceKeepsLifecycleGuardForFollowupOperation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	skillDir := filepath.Join(home, "workspace", "main", "skills", "reviewer")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: reviewer\nstatus: inactive\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, ws, err := openSkillsWorkspace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	guard := st.SkillLifecycleGuard()
	guardReleased := false
	t.Cleanup(func() {
		if !guardReleased {
			guard.Unlock()
		}
	})

	contender, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	acquired := make(chan error, 1)
	go func() {
		if err := contender.SkillLifecycleGuard().Lock(ctx); err != nil {
			acquired <- err
			return
		}
		contender.SkillLifecycleGuard().Unlock()
		acquired <- nil
	}()
	select {
	case err := <-acquired:
		guardReleased = true
		t.Fatalf("follow-up operation released lifecycle guard before returning: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := skill.ActivateSkill(ctx, st.DB, ws, "reviewer"); err != nil {
		t.Fatalf("follow-up activation: %v", err)
	}
	guard.Unlock()
	guardReleased = true
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("contender after follow-up operation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("contender did not acquire lifecycle guard after follow-up operation")
	}
}
