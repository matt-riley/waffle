package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/skill/spec"
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

// TestDistillRefusesNonConformingOutput: distill refuses to write a skill
// failing the Agent Skills constraints (name, empty/oversized description)
// and creates no SKILL.md on the failing path (#396).
func TestDistillRefusesNonConformingOutput(t *testing.T) {
	cases := []struct {
		name        string
		manifest    string
		wantSub     string
		description string
	}{
		{name: "trailing hyphen", wantSub: "name", description: "d", manifest: `{"name":"bad-","description":"d","body":"step one is sufficiently detailed then step two"}`},
		{name: "consecutive hyphens", wantSub: "name", description: "d", manifest: `{"name":"a--b","description":"d","body":"step one is sufficiently detailed then step two"}`},
		{name: "over 64 chars", wantSub: "name", description: "d", manifest: `{"name":"` + strings.Repeat("a", 65) + `","description":"d","body":"step one is sufficiently detailed then step two"}`},
		{name: "empty description", wantSub: "description", manifest: `{"name":"ok-skill","description":"","body":"step one is sufficiently detailed then step two"}`},
		{name: "oversized description", wantSub: "description", manifest: `{"name":"ok-skill","description":"` + strings.Repeat("d", 1025) + `","body":"step one is sufficiently detailed then step two"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			tl := DistillTool{WS: ws}
			if _, err := tl.Run(context.Background(), json.RawMessage(tc.manifest)); err == nil {
				t.Fatal("distill succeeded, want refusal")
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want naming %q", err, tc.wantSub)
			}
			dir := filepath.Join(ws.SkillsDir(), tc.description)
			_ = dir
			if _, err := os.Stat(filepath.Join(ws.SkillsDir(), "ok-skill", "SKILL.md")); !os.IsNotExist(err) {
				t.Errorf("SKILL.md created on failing path")
			}
			if entries, err := os.ReadDir(ws.SkillsDir()); err == nil && len(entries) != 0 {
				t.Errorf("skills dir not empty after refusal: %v", entries)
			}
		})
	}
}

// TestDistillWritesSpecConformingFile: the distilled SKILL.md validates
// under the shared validator and carries status under the waffle metadata
// key — no non-standard top-level fields (#396).
func TestDistillWritesSpecConformingFile(t *testing.T) {
	ws := testWorkspace(t)
	tl := DistillTool{WS: ws}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{
		"name":"conform",
		"description":"Colon: and #hash and \"quotes\"",
		"body":"step one is sufficiently detailed then step two"
	}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws.SkillsDir(), "conform", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	fields, body, err := spec.ParseFrontmatter(string(raw))
	if err != nil {
		t.Fatalf("distilled file unparseable: %v\n%s", err, raw)
	}
	if fields[spec.WaffleStatusKey] != "inactive" {
		t.Errorf("status = %q, want inactive under metadata:\n%s", fields[spec.WaffleStatusKey], raw)
	}
	if err := spec.Validate(fields["name"], fields["description"], fields, body, "conform"); err != nil {
		t.Errorf("distilled file fails validator: %v", err)
	}
	for _, marker := range []string{"provenance:", "source_id:", "trust_class:", "session_id:", "channel:", "untrusted_context:"} {
		if strings.Contains(string(raw), marker) {
			t.Errorf("distilled file still carries dropped marker %q", marker)
		}
	}
}
