package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistillWritesLoadableSkill(t *testing.T) {
	ws := testWorkspace(t)
	tl := DistillTool{WS: ws}

	out, err := tl.Run(context.Background(), json.RawMessage(`{
		"name": "release-cli",
		"description": "cut a new CLI release",
		"body": "1. bump version\n2. tag the release\n3. push tags to origin"
	}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "release-cli") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(out, "inactive") {
		t.Errorf("expected inactive notice, out = %q", out)
	}

	// Check the written SKILL.md directly (do not import skill — skill imports
	// memory for audit and that creates a test import cycle).
	path := filepath.Join(ws.SkillsDir(), "release-cli", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("distilled skill not written:", err)
	}
	body := string(raw)
	if !strings.Contains(body, "cut a new CLI release") {
		t.Errorf("description missing: %q", body)
	}
	if !strings.Contains(body, "bump version") {
		t.Errorf("body missing: %q", body)
	}
	if !strings.Contains(body, "status: inactive") {
		t.Errorf("expected inactive status in frontmatter: %q", body)
	}

	// Re-distilling updates in place while inactive.
	out, err = tl.Run(context.Background(), json.RawMessage(`{"name":"release-cli","description":"d","body":"1. new steps for release\n2. publish artifacts"}`))
	if err != nil || !strings.Contains(out, "updated") {
		t.Errorf("re-distill = %q, %v", out, err)
	}
}

func TestDistillRefusesActiveOverwrite(t *testing.T) {
	ws := testWorkspace(t)
	path := filepath.Join(ws.SkillsDir(), "live", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: live\nstatus: active\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := DistillTool{WS: ws}
	_, err := tl.Run(context.Background(), json.RawMessage(`{"name":"live","description":"d","body":"step one of a real procedure that is long enough"}`))
	if err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("want active overwrite refusal, got %v", err)
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
	if _, err := os.Stat(ws.SkillsDir()); !os.IsNotExist(err) {
		t.Skip("skills dir already present")
	}
	tl := DistillTool{WS: ws}
	if _, err := tl.Run(context.Background(),
		json.RawMessage(`{"name":"x","description":"d","body":"step one of a real procedure that is long enough"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.SkillsDir(), "x", "SKILL.md")); err != nil {
		t.Errorf("skill file not created: %v", err)
	}
}

func TestDistillQuotesAndNormalizesDescription(t *testing.T) {
	ws := testWorkspace(t)
	tl := DistillTool{WS: ws}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{
		"name":"quoted-skill",
		"description":"release: prod\ncarefully",
		"body":"step one carefully then step two of the release"
	}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws.SkillsDir(), "quoted-skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Newlines in description are normalized to spaces in frontmatter.
	if !strings.Contains(string(raw), "release: prod carefully") {
		t.Fatalf("description = %q", raw)
	}
}
