package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCandidateServiceFullWorkflow(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	svc := &CandidateService{Gate: gate}

	// Write → pending.
	out, err := (RememberTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"note":"review queue candidate"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "waffle candidates show candidate-") {
		t.Fatalf("pending message %q lacks review command", out)
	}

	// List exposes summary fields.
	summaries, corrupt, err := svc.List(context.Background(), "pending")
	if err != nil || len(corrupt) != 0 || len(summaries) != 1 {
		t.Fatalf("List = %+v corrupt=%v err=%v", summaries, corrupt, err)
	}
	s := summaries[0]
	if s.Kind != "memory" || s.Status != "pending" || !strings.Contains(s.Preview, "review queue candidate") || s.Provenance.TrustClass == "" {
		t.Fatalf("summary = %+v", s)
	}

	// Inspect returns the payload digest; approving with the wrong digest fails.
	insp, err := svc.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if insp.FileDigest == "" || insp.Candidate.Body != "review queue candidate" {
		t.Fatalf("inspection = %+v", insp)
	}
	if _, err := svc.Approve(context.Background(), s.ID, "owner", "wrong-digest"); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("approve with wrong digest err = %v, want digest mismatch", err)
	}
	if _, err := os.Stat(ws.MemoryPath()); !os.IsNotExist(err) {
		t.Fatal("failed approval mutated live memory")
	}

	// Approve with the inspected digest applies exactly the reviewed payload.
	applied, err := svc.Approve(context.Background(), s.ID, "owner", insp.FileDigest)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.ApprovedBy != "owner" || applied.ApprovedAt == nil {
		t.Fatalf("applied = %+v", applied)
	}
	live, err := os.ReadFile(ws.MemoryPath())
	if err != nil || !strings.Contains(string(live), "review queue candidate") {
		t.Fatalf("approved memory = %q, %v", live, err)
	}

	// Decisions are idempotent: a second decision fails (either the rewritten
	// file's digest no longer matches, or the status is no longer pending).
	if _, err := svc.Approve(context.Background(), s.ID, "owner", insp.FileDigest); err == nil ||
		(!strings.Contains(err.Error(), "not pending") && !strings.Contains(err.Error(), "digest mismatch")) {
		t.Fatalf("second approve err = %v, want not pending or digest mismatch", err)
	}
	// List with status filter sees applied.
	appliedList, _, err := svc.List(context.Background(), "applied")
	if err != nil || len(appliedList) != 1 {
		t.Fatalf("List(applied) = %+v err=%v", appliedList, err)
	}
}

func TestCandidateServiceDenyRecordsReasonWithoutMutation(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	svc := &CandidateService{Gate: gate}
	if _, err := (RememberTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"note":"deny me"}`)); err != nil {
		t.Fatal(err)
	}
	summaries, _, err := svc.List(context.Background(), "pending")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List = %+v, %v", summaries, err)
	}
	id := summaries[0].ID
	insp, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := svc.Deny(context.Background(), id, "owner", "not useful", insp.FileDigest)
	if err != nil {
		t.Fatal(err)
	}
	if denied.Status != "denied" || denied.DenyReason != "not useful" || denied.DeniedBy != "owner" || denied.DeniedAt == nil {
		t.Fatalf("denied = %+v", denied)
	}
	if _, err := os.Stat(ws.MemoryPath()); !os.IsNotExist(err) {
		t.Fatal("deny mutated live memory")
	}
	// Durable audit: the pending file now carries the denial.
	b, err := os.ReadFile(gate.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var reread Candidate
	if err := json.Unmarshal(b, &reread); err != nil {
		t.Fatal(err)
	}
	if reread.Status != "denied" || reread.DenyReason != "not useful" {
		t.Fatalf("audit file = %+v", reread)
	}
}

func TestCandidateServiceListSkipsCorruptFiles(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	svc := &CandidateService{Gate: gate}
	if _, err := (RememberTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"note":"good candidate"}`)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ws.Dir, "pending")
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	summaries, corrupt, err := svc.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0].Preview, "good candidate") {
		t.Fatalf("valid candidate hidden by corrupt file: %+v", summaries)
	}
	if len(corrupt) != 1 || !strings.Contains(corrupt[0], "broken.json") {
		t.Fatalf("corrupt report = %v, want broken.json named", corrupt)
	}
}

func TestCandidateServiceConcurrentDecisionsApplyOnce(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	svc := &CandidateService{Gate: gate}
	if _, err := (RememberTool{WS: ws, Gate: gate}).Run(context.Background(), json.RawMessage(`{"note":"one winner"}`)); err != nil {
		t.Fatal(err)
	}
	summaries, _, err := svc.List(context.Background(), "pending")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List = %+v, %v", summaries, err)
	}
	insp, err := svc.Get(context.Background(), summaries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Approve(context.Background(), insp.Candidate.ID, "owner", insp.FileDigest); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	applied := 0
	for err := range errs {
		if err != nil && !strings.Contains(err.Error(), "not pending") && !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("unexpected decision error: %v", err)
		}
		applied++
	}
	if applied != 1 {
		t.Fatalf("concurrent decisions: want exactly one failure, got %d", applied)
	}
	live, err := os.ReadFile(ws.MemoryPath())
	if err != nil || !strings.Contains(string(live), "one winner") {
		t.Fatalf("winner not applied: %q, %v", live, err)
	}
}

func TestCandidateServiceMemoryUpdateDecision(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	svc := &CandidateService{Gate: gate}
	id, err := ws.Append("original text")
	if err != nil {
		t.Fatal(err)
	}
	upd := MemoryUpdateTool{WS: ws, Gate: gate}
	supIn, _ := json.Marshal(map[string]string{"id": id, "action": "supersede", "note": "owner-approved replacement"})
	if _, err := upd.Run(context.Background(), supIn); err != nil {
		t.Fatal(err)
	}
	summaries, _, err := svc.List(context.Background(), "pending")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List = %+v, %v", summaries, err)
	}
	if summaries[0].TargetID != id || summaries[0].Action != "supersede" {
		t.Fatalf("update summary = %+v", summaries[0])
	}
	insp, err := svc.Get(context.Background(), summaries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := svc.Approve(context.Background(), summaries[0].ID, "owner", insp.FileDigest)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.TargetID != id || applied.Digest == "" {
		t.Fatalf("applied update = %+v", applied)
	}
	live, _ := os.ReadFile(ws.MemoryPath())
	if !strings.Contains(string(live), "owner-approved replacement") || strings.Contains(string(live), "original text") {
		t.Fatalf("approved update not applied exactly:\n%s", live)
	}
}
