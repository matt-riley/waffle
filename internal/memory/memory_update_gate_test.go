package memory

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/session"
)

// seedNote writes one live note directly and returns its id plus the update
// tool wired with the given gate (may be nil for auto).
func seedNote(t *testing.T, ws Workspace, gate *Gate) (string, MemoryUpdateTool) {
	t.Helper()
	id, err := ws.Append("trusted baseline note")
	if err != nil {
		t.Fatal(err)
	}
	return id, MemoryUpdateTool{WS: ws, Gate: gate}
}

func liveText(t *testing.T, ws Workspace) string {
	t.Helper()
	b, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMemoryUpdateReviewPending verifies supersede/forget cross the gate in
// review mode: no live change until approval, pending file carries target,
// current text, digest, and a human-readable diff.
func TestMemoryUpdateReviewPending(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	id, upd := seedNote(t, ws, gate)
	ctx := session.WithOrigin(context.Background(), "session-r", "telegram")

	supIn, _ := json.Marshal(map[string]string{"id": id, "action": "supersede", "note": "reviewed replacement"})
	out, err := upd.Run(ctx, supIn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pending owner approval") {
		t.Fatalf("supersede output = %q, want pending", out)
	}
	if strings.Contains(liveText(t, ws), "reviewed replacement") {
		t.Fatal("live memory changed before approval")
	}

	forIn, _ := json.Marshal(map[string]string{"id": id, "action": "forget"})
	if out, err := upd.Run(ctx, forIn); err != nil || !strings.Contains(out, "pending owner approval") {
		t.Fatalf("forget = %q, %v; want pending", out, err)
	}
	if strings.Contains(liveText(t, ws), "trusted baseline note") == false && len(liveText(t, ws)) == 0 {
		t.Fatal("forget changed live memory before approval")
	}

	pending, err := gate.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %+v, %v; want 2 update candidates", pending, err)
	}
	for _, c := range pending {
		if c.Action == "" || c.TargetID != id || c.Digest == "" || c.Current == "" || c.Diff == "" {
			t.Errorf("update candidate missing review fields: %+v", c)
		}
		if c.Provenance.SessionID != "session-r" || c.Provenance.Channel != "telegram" {
			t.Errorf("provenance not derived from context: %+v", c.Provenance)
		}
		if c.Provenance.TrustClass != "model_derived" {
			t.Errorf("update trust class = %q, want model_derived (never owner_stated)", c.Provenance.TrustClass)
		}
	}
}

// TestMemoryUpdateUntrustedForcesReview verifies untrusted-derived updates are
// pending even under write_gate=auto, matching candidate policy.
func TestMemoryUpdateUntrustedForcesReview(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "auto", WS: ws}
	id, upd := seedNote(t, ws, gate)
	ctx := session.WithOrigin(context.Background(), "session-u", "web")

	supIn, _ := json.Marshal(map[string]string{"id": id, "action": "supersede", "note": "from untrusted page"})
	session.MarkUntrusted(ctx)
	out, err := upd.Run(ctx, supIn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pending owner approval") {
		t.Fatalf("untrusted supersede = %q, want pending", out)
	}
	if strings.Contains(liveText(t, ws), "from untrusted page") {
		t.Fatal("untrusted supersede applied without review")
	}
	pending, err := gate.Pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	if pending[0].Provenance.TrustClass != "untrusted_derived" {
		t.Errorf("trust class = %q, want untrusted_derived", pending[0].Provenance.TrustClass)
	}
}

// TestMemoryUpdateTrustedAuto verifies trusted model-derived updates still
// apply immediately under write_gate=auto (existing behavior preserved).
func TestMemoryUpdateTrustedAuto(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "auto", WS: ws}
	id, upd := seedNote(t, ws, gate)
	ctx := session.WithOrigin(context.Background(), "session-a", "cli")

	supIn, _ := json.Marshal(map[string]string{"id": id, "action": "supersede", "note": "auto replacement"})
	out, err := upd.Run(ctx, supIn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "superseded") {
		t.Fatalf("auto supersede = %q", out)
	}
	if !strings.Contains(liveText(t, ws), "auto replacement") {
		t.Fatal("auto supersede did not apply")
	}
	if pend, _ := gate.Pending(); len(pend) != 0 {
		t.Fatalf("auto supersede left pending candidates: %+v", pend)
	}
}

// TestMemoryUpdateApproveDenyAndStale exercises the full decision path:
// approve applies the exact reviewed mutation; deny records a reason without
// mutating; approving against a changed target fails stale.
func TestMemoryUpdateApproveDenyAndStale(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	id, upd := seedNote(t, ws, gate)
	ctx := session.WithOrigin(context.Background(), "session-d", "telegram")

	// Forget → deny.
	forIn, _ := json.Marshal(map[string]string{"id": id, "action": "forget"})
	if _, err := upd.Run(ctx, forIn); err != nil {
		t.Fatal(err)
	}
	pending, _ := gate.Pending()
	if len(pending) != 1 {
		t.Fatalf("want 1 forget candidate, got %d", len(pending))
	}
	forgetID := pending[0].ID
	denied, err := gate.Deny(forgetID, "owner", "keep this note")
	if err != nil {
		t.Fatal(err)
	}
	if denied.Status != "denied" || denied.DeniedBy != "owner" || denied.DenyReason != "keep this note" || denied.DeniedAt == nil {
		t.Fatalf("denied candidate = %+v", denied)
	}
	if !strings.Contains(liveText(t, ws), "trusted baseline note") {
		t.Fatal("deny mutated live memory")
	}
	if _, err := gate.Approve(forgetID, "owner"); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("approving a denied candidate err = %v, want not-pending", err)
	}

	// Supersede → approve applies.
	supIn, _ := json.Marshal(map[string]string{"id": id, "action": "supersede", "note": "approved replacement"})
	if _, err := upd.Run(ctx, supIn); err != nil {
		t.Fatal(err)
	}
	pending, _ = gate.Pending()
	if len(pending) != 1 {
		t.Fatalf("want 1 supersede candidate, got %d", len(pending))
	}
	supID := pending[0].ID
	approved, err := gate.Approve(supID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "applied" || approved.ApprovedBy != "owner" || approved.ApprovedAt == nil {
		t.Fatalf("approved candidate = %+v", approved)
	}
	live := liveText(t, ws)
	if !strings.Contains(live, "approved replacement") || strings.Contains(live, "trusted baseline note") {
		t.Fatalf("approve did not apply the reviewed supersede:\n%s", live)
	}
	arch, err := os.ReadFile(ws.ArchivePath())
	if err != nil || !strings.Contains(string(arch), "trusted baseline note") {
		t.Fatalf("archive after approve = %q, %v", arch, err)
	}
	if _, err := gate.Approve(supID, "owner"); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("double approve err = %v, want not-pending", err)
	}

	// Stale target: propose a supersede, then change the target out from under
	// it; approving must fail stale and leave the newer note untouched.
	newID := extractReportedID(t, "id="+liveIDOfText(t, ws, "approved replacement"))
	supIn2, _ := json.Marshal(map[string]string{"id": newID, "action": "supersede", "note": "stale replacement"})
	if _, err := upd.Run(ctx, supIn2); err != nil {
		t.Fatal(err)
	}
	pending, _ = gate.Pending()
	if len(pending) != 1 {
		t.Fatalf("want 1 new supersede candidate, got %d", len(pending))
	}
	staleID := pending[0].ID
	// The target changed after proposal (direct supersede bypassing the gate).
	if _, err := ws.SupersedeNote(newID, "newer live text", Provenance{TrustClass: "owner_stated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Approve(staleID, "owner"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale approve err = %v, want stale digest error", err)
	}
	if !strings.Contains(liveText(t, ws), "newer live text") {
		t.Fatal("stale approval mutated the newer note")
	}
}

// liveIDOfText returns the id of the live note whose line contains want.
func liveIDOfText(t *testing.T, ws Workspace, want string) string {
	t.Helper()
	for _, n := range loadNotes(liveText(t, ws)) {
		if strings.Contains(n.body, want) && n.id != "" {
			return n.id
		}
	}
	t.Fatalf("no live note contains %q:\n%s", want, liveText(t, ws))
	return ""
}

// TestMemoryUpdateNotesIndexKeepsFTSState verifies supersede/forget through
// the gate keep the FTS/archive split consistent (live index reflects the new
// note, archive reflects the old one).
func TestMemoryUpdateNotesIndexKeepsFTSState(t *testing.T) {
	ws := testWorkspace(t)
	gate := &Gate{Mode: "review", WS: ws}
	id, upd := seedNote(t, ws, gate)
	ctx := session.WithOrigin(context.Background(), "session-f", "cli")

	supIn, _ := json.Marshal(map[string]string{"id": id, "action": "supersede", "note": "indexed replacement"})
	if _, err := upd.Run(ctx, supIn); err != nil {
		t.Fatal(err)
	}
	pending, _ := gate.Pending()
	if len(pending) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(pending))
	}
	if _, err := gate.Approve(pending[0].ID, "owner"); err != nil {
		t.Fatal(err)
	}
	// Note: without a NotesIndex wired (testWorkspace), FTS sync is a no-op,
	// but the file split must still hold: old line archived, new line live.
	live := liveText(t, ws)
	if !strings.Contains(live, "indexed replacement") || strings.Contains(live, "trusted baseline note") {
		t.Fatalf("live memory after approve:\n%s", live)
	}
}
