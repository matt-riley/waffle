package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func seedFailureClass(t *testing.T, sessions *session.Store, title, errMsg string, n int) string {
	t.Helper()
	ctx := context.Background()
	sess, err := sessions.Create(ctx, title)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := sessions.AppendTurn(ctx, sess.ID, llm.Message{
			Role: llm.RoleUser,
			Blocks: []llm.Block{{
				Type: llm.BlockToolResult,
				ToolResult: &llm.ToolResult{
					ToolUseID: "t1",
					Content:   errMsg,
					IsError:   true,
				},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return sess.ID
}

func TestMineThreeFailureClasses(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "learn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)

	// Three distinct failure classes, each recurring (distinct fingerprints).
	seedFailureClass(t, sessions, "a", "error: no such file or directory while opening module cache", 3)
	seedFailureClass(t, sessions, "b", "error: permission denied writing protected config path", 2)
	seedFailureClass(t, sessions, "c", "error: command not found for foobar-cli binary", 4)

	patterns, err := MineFailurePatterns(ctx, sessions, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) < 3 {
		t.Fatalf("want ≥3 failure classes, got %d: %+v", len(patterns), patterns)
	}
	for _, p := range patterns {
		if p.Count < 2 {
			t.Errorf("pattern %q count=%d", p.Class, p.Count)
		}
		if len(p.SessionIDs) == 0 {
			t.Errorf("pattern %q missing evidence session IDs", p.Class)
		}
	}
}

func TestValidateSurfaceRejectsUnknown(t *testing.T) {
	if err := ValidateSurface("skill"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSurface("memory"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSurface("config_stub"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSurface("system_prompt"); err == nil {
		t.Fatal("expected reject for system_prompt")
	}
	if err := ValidateSurface("binary"); err == nil {
		t.Fatal("expected reject for binary")
	}
}

func TestPromotionHeldOutRegress(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "promo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, session.New(st), ws)
	// Rigged eval pair: held-in improves (5→1), held-out regresses (1→4) → reject.
	// SplitHeld on 4 IDs → held-in={s1,s2}, held-out={s3,s4}.
	before := map[string]int{"s1": 5, "s2": 5, "s3": 1, "s4": 1}
	after := map[string]int{"s1": 1, "s2": 1, "s3": 4, "s4": 4}
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		ID:         "prop-test",
		RunID:      "run-1",
		Surface:    SurfaceSkill,
		PatternSig: "permission denied",
		Name:       "recover-perm",
		Body:       "1. fix permissions carefully with chmod\n2. re-run the command",
		Status:     "proposed",
	}
	pat := FailurePattern{Class: "permission denied", SessionIDs: []string{"s1", "s2", "s3", "s4"}}
	out, err := l.PromoteProposal(ctx, prop, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", out.Status)
	}
	if !strings.Contains(out.Audit, "held-out regress") {
		t.Fatalf("audit = %q", out.Audit)
	}
}

func TestPromotionAcceptHeldInImprove(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "promo2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, session.New(st), ws)
	// Rigged: both held-in and held-out improve → accept.
	before := map[string]int{"s1": 4, "s2": 4}
	after := map[string]int{"s1": 1, "s2": 0}
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		ID:          "prop-ok",
		RunID:       "run-2",
		Surface:     SurfaceSkill,
		PatternSig:  "no such file",
		Name:        "recover-missing",
		Description: "recover missing file",
		Body:        "1. create the missing path\n2. re-run the failing command carefully",
		Status:      "proposed",
	}
	pat := FailurePattern{Class: "no such file", SessionIDs: []string{"s1", "s2"}}
	out, err := l.PromoteProposal(ctx, prop, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "accepted" {
		t.Fatalf("status = %q audit=%q", out.Status, out.Audit)
	}
	path := filepath.Join(ws.SkillsDir(), "recover-missing", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "status: inactive") {
		t.Fatalf("skill not inactive: %s", raw)
	}
}

func TestDefaultValidateHeldOutStableAccept(t *testing.T) {
	// held-in improves, held-out stable (equal) → accept under conservative rule.
	before := map[string]int{"in": 3, "out": 2}
	after := map[string]int{"in": 1, "out": 2}
	score := func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	baseline := func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	prop := Proposal{
		PatternSig: "permission denied",
		Body:       "1. fix permissions carefully with chmod\n2. re-run the command",
	}
	improve, regress, audit := DefaultValidate(context.Background(), score, baseline, prop, []string{"in"}, []string{"out"})
	if !improve || regress {
		t.Fatalf("improve=%v regress=%v audit=%q", improve, regress, audit)
	}
	if !DefaultPromote(improve, regress) {
		t.Fatal("expected promote")
	}
}

func TestDefaultValidateScoresSessionTurns(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "score.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	// held-in: many matching errors; held-out: fewer.
	inID := seedFailureClass(t, sessions, "in", "error: permission denied writing protected config path", 4)
	outID := seedFailureClass(t, sessions, "out", "error: permission denied writing protected config path", 1)
	// After "edit": held-in cleaned, held-out same → use Baseline=Count now, Score=rigged lower for in.
	baseline := func(ctx context.Context, id, sig string) (int, error) {
		return CountPatternErrors(ctx, sessions, id, sig)
	}
	// Simulate post-edit: held-in zero errors, held-out unchanged.
	after := map[string]int{inID: 0, outID: 1}
	score := func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		PatternSig: "permission denied",
		Body:       "1. fix permissions carefully with chmod\n2. re-run the command",
	}
	improve, regress, audit := DefaultValidate(ctx, score, baseline, prop, []string{inID}, []string{outID})
	if !improve || regress {
		t.Fatalf("improve=%v regress=%v audit=%q", improve, regress, audit)
	}
	// Rigged regress on held-out.
	after[outID] = 9
	improve, regress, audit = DefaultValidate(ctx, score, baseline, prop, []string{inID}, []string{outID})
	if !improve || !regress {
		t.Fatalf("want improve+regress, got improve=%v regress=%v audit=%q", improve, regress, audit)
	}
	if DefaultPromote(improve, regress) {
		t.Fatal("must not promote when held-out regresses")
	}
}

func TestAttributionCacheZeroProviderCalls(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "attr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	l := NewLearnerFromStore(st, session.New(st), memory.Workspace{Dir: t.TempDir()})
	// No provider configured: first pass writes heuristic into cache.
	pats := []FailurePattern{{
		Class:      "no such file or directory",
		Count:      3,
		SessionIDs: []string{"s1"},
		Samples:    []string{"error: no such file"},
	}}
	out1, calls1, err := l.AttributePatterns(ctx, pats)
	if err != nil {
		t.Fatal(err)
	}
	if calls1 != 0 {
		t.Fatalf("heuristic path should not call provider, calls=%d", calls1)
	}
	if out1[0].Attribution == "" {
		t.Fatal("expected attribution label")
	}
	// Second pass: same data hits cache, still 0 calls even if provider were set.
	l.Model = "utility-model"
	l.Provider = &countingProvider{}
	_, calls2, err := l.AttributePatterns(ctx, pats)
	if err != nil {
		t.Fatal(err)
	}
	if calls2 != 0 {
		t.Fatalf("re-run on unchanged data must be 0 provider calls, got %d", calls2)
	}
	if l.Provider.(*countingProvider).n != 0 {
		t.Fatalf("provider was called %d times", l.Provider.(*countingProvider).n)
	}
}

type countingProvider struct{ n int }

func (p *countingProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.n++
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "label"}}}}, nil
}

func TestDiscoverActiveFiltersInactive(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "active-one", `---
name: active-one
description: live
status: active
---

body
`)
	writeSkill(t, root, "inactive-one", `---
name: inactive-one
description: pending
status: inactive
---

body
`)
	// Pre-#65 skills without status are active.
	writeSkill(t, root, "legacy", `---
name: legacy
description: old
---

body
`)
	skills, err := DiscoverActive(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}
	if !names["active-one"] || !names["legacy"] {
		t.Fatalf("expected active+legacy, got %v", names)
	}
	if names["inactive-one"] {
		t.Fatal("inactive skill listed")
	}
}

func TestCannotOverwriteActiveSkill(t *testing.T) {
	ws := memory.Workspace{Dir: t.TempDir()}
	c := memory.Candidate{Name: "live", Description: "d", Body: "step one of a long enough procedure"}
	if err := writeSkillInactive(ws, c); err != nil {
		t.Fatal(err)
	}
	// Activate via frontmatter.
	path := filepath.Join(ws.SkillsDir(), "live", "SKILL.md")
	raw, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(setFrontmatterStatus(string(raw), StatusActive)), 0o644)
	if err := writeSkillInactive(ws, c); err == nil {
		t.Fatal("expected refuse overwrite of active skill")
	}
}

func TestFullLearnRun(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedFailureClass(t, sessions, "a", "error: exit status 1\nno such file or directory: /tmp/x", 3)
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, sessions, ws)
	res, err := l.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Patterns) < 1 {
		t.Fatal("expected patterns")
	}
	if res.Digest == "" {
		t.Fatal("expected digest")
	}
	// Second run on unchanged sessions after since watermark may mine nothing
	// (sessions not updated after first run finished). That's OK.
}
