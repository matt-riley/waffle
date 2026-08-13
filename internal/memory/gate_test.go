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

func TestRememberRecordsUntrustedFetchProvenance(t *testing.T) {
	ws := testWorkspace(t)
	tool := RememberTool{WS: ws, Provenance: Provenance{
		SessionID: "session-fetch", Channel: "telegram", UntrustedContext: true,
		SourceKind: "model_inference", SourceID: "fetch:tool-1", TrustClass: "untrusted_derived",
	}}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"note":"fact derived from fetched page"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pending owner approval") {
		t.Fatalf("untrusted write = %q, want pending", out)
	}
	pending, err := (&Gate{WS: ws}).Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	p := pending[0].Provenance
	if p.SessionID != "session-fetch" || p.Channel != "telegram" || !p.UntrustedContext || p.SourceID != "fetch:tool-1" {
		t.Fatalf("provenance = %+v", p)
	}
}

func TestLiveMemoryAndSkillRecordCompleteProvenance(t *testing.T) {
	ws := testWorkspace(t)
	provenance := Provenance{SessionID: "session-1", Channel: "telegram", SourceKind: "model_inference", SourceID: "turn-7", TrustClass: "model_derived"}
	if _, err := (RememberTool{WS: ws, Provenance: provenance}).Run(context.Background(), json.RawMessage(`{"note":"trusted observation"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := (DistillTool{WS: ws, Provenance: provenance}).Run(context.Background(), json.RawMessage(`{"name":"provenance-skill","description":"records provenance","body":"step one is sufficiently detailed then step two"}`)); err != nil {
		t.Fatal(err)
	}
	for path, fields := range map[string][]string{
		ws.MemoryPath(): {"session=session-1", "channel=telegram", "untrusted=false"},
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range fields {
			if !strings.Contains(string(body), field) {
				t.Errorf("%s missing %q: %s", path, field, body)
			}
		}
	}
	// The distilled SKILL.md no longer carries the write-only provenance
	// markers (#396): authoritative provenance lives in MEMORY.md and the
	// install journal. What waffle writes is spec-conforming — name,
	// description, and activation state under the waffle metadata key.
	skillBody, err := os.ReadFile(filepath.Join(ws.SkillsDir(), "provenance-skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"session_id:", "channel:", "untrusted_context:", "provenance:"} {
		if strings.Contains(string(skillBody), marker) {
			t.Errorf("SKILL.md still carries dropped provenance marker %q:\n%s", marker, skillBody)
		}
	}
	fields, _, err := spec.ParseFrontmatter(string(skillBody))
	if err != nil {
		t.Fatalf("distilled SKILL.md not parseable: %v\n%s", err, skillBody)
	}
	if fields["name"] != "provenance-skill" || fields["description"] == "" {
		t.Errorf("distilled SKILL.md fields = %v", fields)
	}
	if fields[spec.WaffleStatusKey] != "inactive" {
		t.Errorf("distilled SKILL.md status = %q, want inactive under metadata", fields[spec.WaffleStatusKey])
	}
}

func TestReviewGateRoundTripMemoryAndSkill(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	remember := RememberTool{WS: ws, Gate: gate}
	if _, err := remember.Run(context.Background(), json.RawMessage(`{"note":"review this memory"}`)); err != nil {
		t.Fatal(err)
	}
	distill := DistillTool{WS: ws, Gate: gate}
	if _, err := distill.Run(context.Background(), json.RawMessage(`{"name":"review-me","description":"review flow","body":"step one is sufficiently detailed then step two"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.MemoryPath()); !os.IsNotExist(err) {
		t.Fatalf("live memory changed before approval: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.SkillsDir(), "review-me", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("live skill changed before approval: %v", err)
	}
	pending, err := gate.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	for _, candidate := range pending {
		if _, err := gate.Approve(candidate.ID, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	memoryBody, err := os.ReadFile(ws.MemoryPath())
	if err != nil || !strings.Contains(string(memoryBody), "review this memory") {
		t.Fatalf("approved memory = %q, %v", memoryBody, err)
	}
	skillBody, err := os.ReadFile(filepath.Join(ws.SkillsDir(), "review-me", "SKILL.md"))
	if err != nil || !strings.Contains(string(skillBody), "step one") {
		t.Fatalf("approved skill = %q, %v", skillBody, err)
	}
}

func TestNotifyGateDeliversDiffForMemoryAndSkill(t *testing.T) {
	ws := testWorkspace(t)
	var notices []Candidate
	gate := &Gate{Mode: "notify", WS: ws, Notify: func(c Candidate) { notices = append(notices, c) }}
	if _, err := (RememberTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"note":"notify memory"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := (DistillTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"name":"notify-skill","description":"notify flow","body":"step one is sufficiently detailed then step two"}`)); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 {
		t.Fatalf("notices = %+v, want memory and skill", notices)
	}
	for _, notice := range notices {
		if !strings.HasPrefix(notice.Diff, "+ ") || !strings.Contains(notice.Diff, notice.Body) {
			t.Errorf("notice diff = %q for %+v", notice.Diff, notice)
		}
	}
}
