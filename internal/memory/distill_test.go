package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/skill"
)

func TestDistillWritesLoadableSkill(t *testing.T) {
	ws := testWorkspace(t)
	tl := DistillTool{WS: ws}

	out, err := tl.Run(context.Background(), json.RawMessage(`{
		"name": "release-cli",
		"description": "cut a new CLI release",
		"body": "1. bump version\n2. tag\n3. push"
	}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "release-cli") {
		t.Errorf("out = %q", out)
	}

	// The written skill must be discoverable and parse back.
	skills, err := skill.Discover(ws.SkillsDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	s, ok := skill.Find(skills, "release-cli")
	if !ok {
		t.Fatal("distilled skill not discovered")
	}
	if s.Description != "cut a new CLI release" {
		t.Errorf("description = %q", s.Description)
	}
	body, _ := s.Body()
	if !strings.Contains(body, "bump version") {
		t.Errorf("body = %q", body)
	}

	// Re-distilling updates in place.
	out, err = tl.Run(context.Background(), json.RawMessage(`{"name":"release-cli","description":"d","body":"new steps"}`))
	if err != nil || !strings.Contains(out, "updated") {
		t.Errorf("re-distill = %q, %v", out, err)
	}
}

func TestDistillRejectsBadNames(t *testing.T) {
	tl := DistillTool{WS: testWorkspace(t)}
	for _, name := range []string{"Bad Name", "UPPER", "with/slash", ""} {
		input, _ := json.Marshal(map[string]string{"name": name, "description": "d", "body": "b"})
		if _, err := tl.Run(context.Background(), input); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestDistillCreatesSkillsDir(t *testing.T) {
	ws := testWorkspace(t)
	// SkillsDir doesn't exist yet.
	if _, err := os.Stat(ws.SkillsDir()); !os.IsNotExist(err) {
		t.Skip("skills dir already present")
	}
	tl := DistillTool{WS: ws}
	if _, err := tl.Run(context.Background(),
		json.RawMessage(`{"name":"x","description":"d","body":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.SkillsDir(), "x", "SKILL.md")); err != nil {
		t.Errorf("skill file not created: %v", err)
	}
}
