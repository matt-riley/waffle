package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// TestNoBaselineNeverAutoAccepts is the core #414 regression: today's case —
// one evidence session plus a body longer than 20 characters — must not be
// accepted without measured before/after results.
func TestNoBaselineNeverAutoAccepts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "nb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, session.New(st), ws)
	// No Baseline and no Score wired: exactly the production path today.
	prop := Proposal{
		ID:          "prop-nobaseline",
		RunID:       "run-nb",
		Surface:     SurfaceSkill,
		PatternSig:  "permission denied",
		Name:        "recover-perm",
		Description: "auto-mined recovery: permission denied",
		Body:        "1. fix the permissions carefully with chmod\n2. re-run the command and confirm",
		Status:      "proposed",
	}
	pat := FailurePattern{Class: "permission denied", SessionIDs: []string{"s1"}}
	out, err := l.PromoteProposal(ctx, prop, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status == "accepted" {
		t.Fatalf("nil baseline auto-accepted the proposal: %+v", out)
	}
	if out.Status != "proposed" {
		t.Fatalf("status = %q, want proposed (pending owner review)", out.Status)
	}
	if !strings.Contains(out.Audit, "pending owner review") {
		t.Fatalf("audit = %q, want pending-owner-review disposition", out.Audit)
	}
	// No live write occurred.
	skillDir := filepath.Join(ws.SkillsDir(), "recover-perm", "SKILL.md")
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("skill written without validation: %v", err)
	}
	// The proposal is persisted with the pending status for operator review.
	var status, audit string
	if err := st.DB.QueryRowContext(ctx, `SELECT status, audit FROM learn_proposals WHERE id = ?`, prop.ID).Scan(&status, &audit); err != nil {
		t.Fatal(err)
	}
	if status != "proposed" || !strings.Contains(audit, "no baseline") {
		t.Fatalf("persisted proposal = (%q, %q)", status, audit)
	}
}

// TestNoHeldOutEvidenceStaysPending verifies automatic promotion requires an
// independent held-out case even when a real baseline exists (#414).
func TestNoHeldOutEvidenceStaysPending(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ho.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, session.New(st), ws)
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return 5, nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return 1, nil }
	prop := Proposal{
		ID: "prop-noheldout", RunID: "run-ho", Surface: SurfaceSkill,
		PatternSig: "no such file", Name: "recover-missing",
		Body: "1. create the missing path\n2. re-run the failing command carefully",
	}
	// Single evidence session → held-in only, no held-out split (#65).
	pat := FailurePattern{Class: "no such file", SessionIDs: []string{"s1"}}
	out, err := l.PromoteProposal(ctx, prop, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status == "accepted" {
		t.Fatalf("auto-accepted with no held-out evidence: %+v", out)
	}
	if out.Status != "proposed" || !strings.Contains(out.Audit, "no independent held-out") {
		t.Fatalf("status=%q audit=%q, want pending owner approval", out.Status, out.Audit)
	}
}

// TestEvaluatorErrorFailsClosed verifies a scorer error rejects the proposal
// and is persisted, never treated as a zero (green) result (#414).
func TestEvaluatorErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ee.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, session.New(st), ws)
	boom := errors.New("held-out index unavailable")
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return 2, nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return 0, boom }
	prop := Proposal{
		ID: "prop-ee", RunID: "run-ee", Surface: SurfaceSkill,
		PatternSig: "permission denied", Name: "recover-perm",
		Body: "1. fix the permissions carefully with chmod\n2. re-run the command",
	}
	pat := FailurePattern{Class: "permission denied", SessionIDs: []string{"s1", "s2", "s3", "s4"}}
	out, err := l.PromoteProposal(ctx, prop, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "rejected" || !strings.Contains(out.Audit, "evaluator error") {
		t.Fatalf("status=%q audit=%q, want rejected with evaluator error", out.Status, out.Audit)
	}
	var status, audit string
	if err := st.DB.QueryRowContext(ctx, `SELECT status, audit FROM learn_proposals WHERE id = ?`, prop.ID).Scan(&status, &audit); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || !strings.Contains(audit, boom.Error()) {
		t.Fatalf("persisted = (%q, %q)", status, audit)
	}
	// No live skill written despite a syntactically valid body.
	if _, err := os.Stat(filepath.Join(ws.SkillsDir(), "recover-perm", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("skill written after evaluator error: %v", err)
	}
}

// TestPromotePersistsExactCounts verifies both exact held-in/held-out counts
// land in the persisted audit when a measured baseline exists (#414).
func TestPromotePersistsExactCounts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cnt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, session.New(st), ws)
	before := map[string]int{"s1": 4, "s2": 4}
	after := map[string]int{"s1": 0, "s2": 0}
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		ID: "prop-counts", RunID: "run-cnt", Surface: SurfaceSkill,
		PatternSig:  "no such file",
		Name:        "recover-missing",
		Description: "recover missing path",
		Body:        "1. create the missing path\n2. re-run the failing command carefully",
	}
	pat := FailurePattern{Class: "no such file", SessionIDs: []string{"s1", "s2"}}
	out, err := l.PromoteProposal(ctx, prop, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "accepted" {
		t.Fatalf("status=%q audit=%q", out.Status, out.Audit)
	}
	var audit string
	if err := st.DB.QueryRowContext(ctx, `SELECT audit FROM learn_proposals WHERE id = ?`, prop.ID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audit, "held-in 4→0") || !strings.Contains(audit, "held-out 4→0") {
		t.Fatalf("audit missing exact counts: %q", audit)
	}
}

// TestProductionLearnerFromStoreReturnsPending is the acceptance boundary for
// the production constructor: NewLearnerFromStore wires no baseline evaluator,
// so its proposals explicitly resolve as unevaluated/pending, never accepted.
func TestProductionLearnerFromStoreReturnsPending(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "prod.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedFailureClass(t, sessions, "a", "error: permission denied writing protected config path", 3)
	l := NewLearnerFromStore(st, sessions, memory.Workspace{Dir: t.TempDir()})
	res, err := l.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 0 {
		t.Fatalf("production run accepted %d proposals without a baseline", res.Accepted)
	}
	for _, p := range res.Proposals {
		if p.Status == "accepted" {
			t.Fatalf("production proposal auto-accepted: %+v", p)
		}
	}
	if res.Pending == 0 && res.Rejected == 0 {
		t.Fatalf("no proposals resolved: %+v", res)
	}
}
