package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/memory"
)

// candidatesFixture creates an isolated WAFFLE_HOME workspace and submits one
// pending memory candidate plus one pending skill candidate through the
// production gate path, returning the service used by the CLI.
func candidatesFixture(t *testing.T) *memory.CandidateService {
	t.Helper()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	gate := &memory.Gate{Mode: "review", WS: ws}
	if _, err := (memory.RememberTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"note":"pending memory note"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := (memory.DistillTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"name":"pending-skill","description":"a candidate skill","body":"step one is sufficiently detailed then step two"}`)); err != nil {
		t.Fatal(err)
	}
	return &memory.CandidateService{Gate: gate}
}

func TestCandidatesCLIListShowApproveWorkflow(t *testing.T) {
	svc := candidatesFixture(t)

	// list shows both candidates with kind, status, preview, and review hint.
	var out bytes.Buffer
	if err := candidatesCmd(context.Background(), []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pending memory note", "pending-skill", "skill", "memory"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "CORRUPT") {
		t.Errorf("list reported corruption for a healthy queue:\n%s", out.String())
	}

	// list --json returns a parseable array with provenance.
	out.Reset()
	if err := candidatesCmd(context.Background(), []string{"list", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var summaries []memory.CandidateSummary
	if err := json.Unmarshal(out.Bytes(), &summaries); err != nil {
		t.Fatalf("list --json: %v\n%s", err, out.String())
	}
	if len(summaries) != 2 {
		t.Fatalf("list --json: want 2 candidates, got %d", len(summaries))
	}

	// show <id> prints the full review view.
	out.Reset()
	if err := candidatesCmd(context.Background(), []string{"show", summaries[0].ID}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind:", "status:", "provenance:", "approve:     waffle candidates approve " + summaries[0].ID} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show output missing %q:\n%s", want, out.String())
		}
	}

	// approve applies exactly the reviewed payload.
	out.Reset()
	if err := candidatesCmd(context.Background(), []string{"approve", summaries[0].ID}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "approved memory") {
		t.Fatalf("approve output = %q", out.String())
	}
	live, err := os.ReadFile(svc.Gate.WS.MemoryPath())
	if err != nil || !strings.Contains(string(live), "pending memory note") {
		t.Fatalf("approved memory not live: %q, %v", live, err)
	}

	// The skill candidate is written inactive; activation stays explicit.
	skillDir := filepath.Join(svc.Gate.WS.SkillsDir(), "pending-skill", "SKILL.md")
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("skill approved before its turn: %v", err)
	}
	var skillOut bytes.Buffer
	if err := candidatesCmd(context.Background(), []string{"list", "--json"}, &skillOut); err != nil {
		t.Fatal(err)
	}
	var after []memory.CandidateSummary
	if err := json.Unmarshal(skillOut.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	var skillID string
	for _, s := range after {
		if s.Kind == "skill" {
			skillID = s.ID
		}
	}
	if skillID == "" {
		t.Fatal("skill candidate vanished from the queue")
	}
	if err := candidatesCmd(context.Background(), []string{"approve", skillID}, &skillOut); err != nil {
		t.Fatal(err)
	}
	skillBody, err := os.ReadFile(skillDir)
	if err != nil || !strings.Contains(string(skillBody), "step one") {
		t.Fatalf("approved skill = %q, %v", skillBody, err)
	}
	if !strings.Contains(skillOut.String(), "skills activate pending-skill") {
		t.Errorf("approve output should mention explicit activation:\n%s", skillOut.String())
	}
}

func TestCandidatesCLIDenyRecordsReason(t *testing.T) {
	svc := candidatesFixture(t)
	out, _, err := svc.List(context.Background(), "pending")
	if err != nil || len(out) != 2 {
		t.Fatalf("List = %+v, %v", out, err)
	}
	id := out[0].ID
	var buf bytes.Buffer
	if err := candidatesCmd(context.Background(), []string{"deny", id, "--reason=duplicate of existing note"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "duplicate of existing note") {
		t.Fatalf("deny output = %q", buf.String())
	}
	if _, err := os.Stat(svc.Gate.WS.MemoryPath()); !os.IsNotExist(err) {
		t.Fatal("deny mutated live memory")
	}
	// The candidate is still visible with its denial audit.
	buf.Reset()
	if err := candidatesCmd(context.Background(), []string{"show", id}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "denied:") || !strings.Contains(buf.String(), "duplicate of existing note") {
		t.Errorf("show after deny missing denial audit:\n%s", buf.String())
	}
}

func TestCandidatesCLIUsageErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := candidatesCmd(context.Background(), []string{"approve"}, &buf); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("approve without id err = %v", err)
	}
	if err := candidatesCmd(context.Background(), []string{"deny", "candidate-x"}, &buf); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("deny without reason err = %v", err)
	}
	if err := candidatesCmd(context.Background(), []string{"bogus"}, &buf); err == nil || !strings.Contains(err.Error(), "unknown candidates subcommand") {
		t.Fatalf("unknown subcommand err = %v", err)
	}
}

func TestCandidatesCLISkipsCorruptFiles(t *testing.T) {
	svc := candidatesFixture(t)
	dir := filepath.Join(svc.Gate.WS.Dir, "pending")
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := candidatesCmd(context.Background(), []string{"list"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "CORRUPT broken.json") {
		t.Errorf("list should report the corrupt file individually:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "pending memory note") {
		t.Errorf("valid candidates hidden by corrupt file:\n%s", buf.String())
	}
}
