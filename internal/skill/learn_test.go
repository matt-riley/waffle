package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	patterns, _, _, _, err := MineFailurePatterns(ctx, sessions, LearnCursor{}, 20)
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

func TestSkillStatusPreservesInstallProvenance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		name          = "reviewer"
		sourceRef     = "git:https://example.invalid/org/reviewer@0123456789abcdef0123456789abcdef01234567"
		contentDigest = "sha256:0123456789abcdef"
	)
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name:          name,
		Status:        StatusInactive,
		Source:        "install",
		SourceRef:     sourceRef,
		ContentDigest: contentDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name:   name,
		Status: StatusActive,
		Source: "activate",
	}); err != nil {
		t.Fatal(err)
	}

	var status, source, gotSourceRef, gotContentDigest, activatedAt string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT status, source, source_ref, content_digest, activated_at
		FROM skill_status
		WHERE name = ?`, name).Scan(
		&status, &source, &gotSourceRef, &gotContentDigest, &activatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if status != StatusActive || source != "activate" || activatedAt == "" {
		t.Fatalf("activation state = (%q, %q, %q)", status, source, activatedAt)
	}
	if gotSourceRef != sourceRef || gotContentDigest != contentDigest {
		t.Fatalf("activation provenance = (%q, %q), want (%q, %q)",
			gotSourceRef, gotContentDigest, sourceRef, contentDigest)
	}

	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name:   name,
		Status: StatusInactive,
	}); err != nil {
		t.Fatal(err)
	}
	var inactiveSource, inactiveSourceRef, inactiveDigest, inactiveActivated string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT source, source_ref, content_digest, activated_at
		FROM skill_status
		WHERE name = ?`, name).Scan(
		&inactiveSource, &inactiveSourceRef, &inactiveDigest, &inactiveActivated,
	); err != nil {
		t.Fatal(err)
	}
	if inactiveSource != "activate" {
		t.Fatalf("blank source erased action source: %q", inactiveSource)
	}
	if inactiveSourceRef != sourceRef || inactiveDigest != contentDigest {
		t.Fatalf("inactive provenance = (%q, %q), want (%q, %q)",
			inactiveSourceRef, inactiveDigest, sourceRef, contentDigest)
	}
	if inactiveActivated != activatedAt {
		t.Fatalf("inactive activated_at = %q, want preserved %q", inactiveActivated, activatedAt)
	}
}

func TestActivateSkillRestoresInactiveFrontmatterWhenStatusPersistenceFails(t *testing.T) {
	root := t.TempDir()
	ws := memory.Workspace{Dir: filepath.Join(root, "workspace")}
	path := filepath.Join(ws.SkillsDir(), "atomic-activation", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("---\nname: atomic-activation\ndescription: Atomic activation.\nstatus: inactive\n---\n\n# Atomic\n")
	if err := os.WriteFile(path, before, 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(root, "state", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	err = ActivateSkill(t.Context(), st.DB, ws, "atomic-activation")

	if err == nil {
		t.Fatal("ActivateSkill succeeded with an unavailable status store")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("frontmatter was not restored after status failure:\n%s", after)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("restored skill mode = %v, want 0640", info.Mode().Perm())
	}
	active, discoverErr := DiscoverActive(ws.SkillsDir(), nil)
	if discoverErr != nil {
		t.Fatal(discoverErr)
	}
	if len(active) != 0 {
		t.Fatalf("failed activation remained active: %#v", active)
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

// fakeSkillStatusRows drives scanSkillStatusRows failure paths without a real DB.
type fakeSkillStatusRows struct {
	// pairs are (name, status) rows returned by Next/Scan in order.
	pairs [][2]string
	// failScanAt is the 0-based index at which Scan returns scanErr (if non-nil).
	failScanAt int
	scanErr    error
	// iterErr is returned from Err() after iteration ends.
	iterErr error
	i       int
	scanned int
}

func (f *fakeSkillStatusRows) Next() bool {
	if f.i >= len(f.pairs) {
		return false
	}
	f.i++
	return true
}

func (f *fakeSkillStatusRows) Scan(dest ...any) error {
	idx := f.i - 1
	if f.scanErr != nil && idx == f.failScanAt {
		return f.scanErr
	}
	if idx < 0 || idx >= len(f.pairs) {
		return errors.New("scan past end")
	}
	if len(dest) != 2 {
		return fmt.Errorf("scan: want 2 dests, got %d", len(dest))
	}
	namePtr, ok := dest[0].(*string)
	if !ok {
		return errors.New("scan: dest[0] not *string")
	}
	statusPtr, ok := dest[1].(*string)
	if !ok {
		return errors.New("scan: dest[1] not *string")
	}
	*namePtr = f.pairs[idx][0]
	*statusPtr = f.pairs[idx][1]
	f.scanned++
	return nil
}

func (f *fakeSkillStatusRows) Err() error { return f.iterErr }

func TestFilterActiveFailClosedOnSkillStatusErrors(t *testing.T) {
	// Skills that would become active if overrides were silently empty (#197).
	all := []Skill{
		{Name: "third-party", raw: "---\nname: third-party\nstatus: inactive\n---\n"},
		{Name: "legacy", raw: "---\nname: legacy\n---\n"},
	}

	t.Run("query failure", func(t *testing.T) {
		ctx := context.Background()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "filter.db"))
		if err != nil {
			t.Fatal(err)
		}
		// Close so subsequent Query fails (sql: database is closed).
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := FilterActive(all, st.DB)
		if err == nil {
			t.Fatal("FilterActive succeeded with unavailable skill_status store")
		}
		if got != nil {
			t.Fatalf("on query failure want nil skills, got %#v", got)
		}
		if !strings.Contains(err.Error(), "skill_status") {
			t.Fatalf("error = %v, want skill_status context", err)
		}
	})

	t.Run("scan failure", func(t *testing.T) {
		rows := &fakeSkillStatusRows{
			pairs:      [][2]string{{"third-party", StatusInactive}, {"legacy", StatusActive}},
			failScanAt: 0,
			scanErr:    errors.New("forced scan failure"),
		}
		got, err := scanSkillStatusRows(rows)
		if err == nil {
			t.Fatal("scanSkillStatusRows succeeded on Scan failure")
		}
		if got != nil {
			t.Fatalf("on scan failure want nil map, got %#v", got)
		}
		if !strings.Contains(err.Error(), "skill_status scan") {
			t.Fatalf("error = %v, want skill_status scan context", err)
		}
		// Fail closed through FilterActive when overrides cannot be built:
		// inject via closed DB is query path; scan is unit-tested above.
		// Assert Scan path never partially populates.
		if rows.scanned != 0 {
			t.Fatalf("scanned %d rows before failure, want 0 partial success", rows.scanned)
		}
	})

	t.Run("mid-iteration failure", func(t *testing.T) {
		rows := &fakeSkillStatusRows{
			pairs:   [][2]string{{"third-party", StatusInactive}},
			iterErr: errors.New("forced rows.Err"),
		}
		got, err := scanSkillStatusRows(rows)
		if err == nil {
			t.Fatal("scanSkillStatusRows succeeded on rows.Err")
		}
		if got != nil {
			t.Fatalf("on iterate failure want nil map, got %#v", got)
		}
		if !strings.Contains(err.Error(), "skill_status iterate") {
			t.Fatalf("error = %v, want skill_status iterate context", err)
		}
	})
}

func TestFilterActiveDBOverrideAndLegacyDefault(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "override.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Explicit inactive row wins even if frontmatter says active (deny-by-default
	// for third-party installs that write skill_status inactive).
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name:   "third-party",
		Status: StatusInactive,
		Source: "install",
	}); err != nil {
		t.Fatal(err)
	}

	all := []Skill{
		{Name: "third-party", raw: "---\nname: third-party\nstatus: active\n---\n"},
		// No skill_status row and no frontmatter status → active (#65 legacy).
		{Name: "legacy", raw: "---\nname: legacy\n---\n"},
	}
	got, err := FilterActive(all, st.DB)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if names["third-party"] {
		t.Fatal("skill with explicit inactive skill_status row must stay inactive")
	}
	if !names["legacy"] {
		t.Fatal("legacy skill with no row and no frontmatter status must stay active")
	}
}

// TestFilterActiveWithExtensionOverrides: the waffle extension status layer
// (#394) sits between frontmatter and the DB — the DB still wins, and a
// missing extension value falls back to frontmatter.
func TestFilterActiveWithExtensionOverrides(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "override.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name:   "db-inactive",
		Status: StatusInactive,
		Source: "install",
	}); err != nil {
		t.Fatal(err)
	}

	all := []Skill{
		// Extension says inactive; frontmatter has no status (default active).
		{Name: "ext-inactive", raw: "---\nname: ext-inactive\n---\n"},
		// Extension says active; frontmatter says inactive.
		{Name: "ext-active", raw: "---\nname: ext-active\nstatus: inactive\n---\n"},
		// DB row says inactive and wins over the extension's active.
		{Name: "db-inactive", raw: "---\nname: db-inactive\n---\n"},
		// No extension entry: frontmatter default applies.
		{Name: "legacy", raw: "---\nname: legacy\n---\n"},
	}
	extension := map[string]string{
		"ext-inactive": StatusInactive,
		"ext-active":   StatusActive,
		"db-inactive":  StatusActive, // must lose to the DB row
	}
	got, err := FilterActiveWithExtension(all, st.DB, extension)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if names["ext-inactive"] {
		t.Error("extension-inactive skill must be filtered out")
	}
	if !names["ext-active"] {
		t.Error("extension-active skill must stay")
	}
	if names["db-inactive"] {
		t.Error("DB override must beat the extension")
	}
	if !names["legacy"] {
		t.Error("legacy skill must stay active")
	}
}

func TestScanSkillStatusRowsHappyPath(t *testing.T) {
	rows := &fakeSkillStatusRows{
		pairs: [][2]string{
			{"a", StatusActive},
			{"b", StatusInactive},
		},
	}
	got, err := scanSkillStatusRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != StatusActive || got["b"] != StatusInactive {
		t.Fatalf("got %#v", got)
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
	updated, _ := setFrontmatterStatus(string(raw), StatusActive)
	_ = os.WriteFile(path, []byte(updated), 0o644)
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

func TestAcceptProposalGitCommit(t *testing.T) {
	// Accepted learn proposals create a git commit when GitDir/workspace is a repo (#65).
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "git-learn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=waffle-test",
			"GIT_AUTHOR_EMAIL=waffle-test@example.com",
			"GIT_COMMITTER_NAME=waffle-test",
			"GIT_COMMITTER_EMAIL=waffle-test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "waffle-test@example.com")
	runGit("config", "user.name", "waffle-test")
	// Initial commit so the repo is non-empty before the skill write.
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("learn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README")
	runGit("commit", "-m", "init")

	ws := memory.Workspace{Dir: repo}
	l := NewLearnerFromStore(st, session.New(st), ws)
	l.GitDir = repo
	before := map[string]int{"s1": 4, "s2": 4}
	after := map[string]int{"s1": 0, "s2": 0}
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		ID:          "prop-git",
		RunID:       "run-git",
		Surface:     SurfaceSkill,
		PatternSig:  "no such file",
		Name:        "recover-git",
		Description: "recover missing path",
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
	if !strings.Contains(out.Audit, "git commit:") {
		t.Fatalf("expected git commit in audit, got %q", out.Audit)
	}
	logCmd := exec.Command("git", "-C", repo, "log", "-1", "--pretty=%s")
	logOut, err := logCmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	subj := strings.TrimSpace(string(logOut))
	if !strings.Contains(subj, "learn: accept skill") || !strings.Contains(subj, "no such file") {
		t.Fatalf("HEAD subject = %q, want learn accept skill commit", subj)
	}
	// No-repo path: audit notes stored-only when GitDir has no .git.
	bare := t.TempDir()
	l2 := NewLearnerFromStore(st, session.New(st), memory.Workspace{Dir: bare})
	l2.Baseline = l.Baseline
	l2.Score = l.Score
	prop2 := prop
	prop2.ID = "prop-nogit"
	prop2.Name = "recover-nogit"
	out2, err := l2.PromoteProposal(ctx, prop2, pat)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != "accepted" {
		t.Fatalf("no-git status = %q audit=%q", out2.Status, out2.Audit)
	}
	if !strings.Contains(out2.Audit, "no git repo") {
		t.Fatalf("expected no-git audit note, got %q", out2.Audit)
	}
}

// TestAcceptProposalCommitStagesOnlyProposalPaths asserts an accepted learn
// proposal commits only the path it wrote, never unrelated tracked edits or
// untracked files already present in the workspace (#295).
func TestAcceptProposalCommitStagesOnlyProposalPaths(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "git-learn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=waffle-test",
			"GIT_AUTHOR_EMAIL=waffle-test@example.com",
			"GIT_COMMITTER_NAME=waffle-test",
			"GIT_COMMITTER_EMAIL=waffle-test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	runGit("init")
	runGit("config", "user.email", "waffle-test@example.com")
	runGit("config", "user.name", "waffle-test")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("learn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README")
	runGit("commit", "-m", "init")

	// Unrelated workspace state that must NOT enter the learning commit:
	// a tracked-but-modified file and an untracked file.
	if err := os.WriteFile(filepath.Join(repo, "tracked.go"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.go")
	runGit("commit", "-m", "add tracked")
	if err := os.WriteFile(filepath.Join(repo, "tracked.go"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "WIP.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := memory.Workspace{Dir: repo}
	l := NewLearnerFromStore(st, session.New(st), ws)
	l.GitDir = repo
	before := map[string]int{"s1": 4, "s2": 4}
	after := map[string]int{"s1": 0, "s2": 0}
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		ID:          "prop-git2",
		RunID:       "run-git2",
		Surface:     SurfaceSkill,
		PatternSig:  "no such file",
		Name:        "recover-git2",
		Description: "recover missing path",
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

	// The commit must contain exactly the accepted skill file.
	files := strings.Fields(runGit("show", "--name-only", "--pretty=format:", "HEAD"))
	if len(files) != 1 || !strings.Contains(files[0], "skills/recover-git2/SKILL.md") {
		t.Fatalf("HEAD files = %v, want only skills/recover-git2/SKILL.md", files)
	}
	// The unrelated changes stay outside the commit.
	status := runGit("status", "--porcelain")
	if !strings.Contains(status, " M tracked.go") || !strings.Contains(status, "?? WIP.go") {
		t.Fatalf("unrelated workspace changes leaked into the learning commit:\n%s", status)
	}
}

// TestAcceptProposalCommitIgnoresPreStagedChanges asserts an accepted learn
// proposal does not commit unrelated changes that were already staged in the
// index before the proposal ran (#295 review).
func TestAcceptProposalCommitIgnoresPreStagedChanges(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "git-learn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=waffle-test",
			"GIT_AUTHOR_EMAIL=waffle-test@example.com",
			"GIT_COMMITTER_NAME=waffle-test",
			"GIT_COMMITTER_EMAIL=waffle-test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	runGit("init")
	runGit("config", "user.email", "waffle-test@example.com")
	runGit("config", "user.name", "waffle-test")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("learn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README")
	runGit("commit", "-m", "init")

	// Unrelated change already staged in the index before the proposal.
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "staged.txt")

	ws := memory.Workspace{Dir: repo}
	l := NewLearnerFromStore(st, session.New(st), ws)
	l.GitDir = repo
	before := map[string]int{"s1": 4, "s2": 4}
	after := map[string]int{"s1": 0, "s2": 0}
	l.Baseline = func(_ context.Context, id, _ string) (int, error) { return before[id], nil }
	l.Score = func(_ context.Context, id, _ string) (int, error) { return after[id], nil }
	prop := Proposal{
		ID:          "prop-staged",
		RunID:       "run-staged",
		Surface:     SurfaceSkill,
		PatternSig:  "no such file",
		Name:        "recover-staged",
		Description: "recover missing path",
		Body:        "1. create the missing path",
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

	files := strings.Fields(runGit("show", "--name-only", "--pretty=format:", "HEAD"))
	if len(files) != 1 || !strings.Contains(files[0], "skills/recover-staged/SKILL.md") {
		t.Fatalf("HEAD files = %v, want only skills/recover-staged/SKILL.md", files)
	}
	// The pre-staged unrelated file is still staged but not committed.
	status := runGit("status", "--porcelain")
	if !strings.Contains(status, "A  staged.txt") {
		t.Fatalf("pre-staged change leaked into the learning commit:\n%s", status)
	}
}

// TestLearnerMemoryProposalAppendsUnderTheSharedLock is the review follow-up on
// #267: the learner used to open MEMORY.md itself, so an accepted proposal
// could be erased by a concurrent read-modify-write in another process and
// still be reported as applied.
func TestLearnerMemoryProposalAppendsUnderTheSharedLock(t *testing.T) {
	workspace := memory.Workspace{Dir: t.TempDir()}
	learner := &Learner{WS: workspace}
	proposal := &Proposal{Surface: SurfaceMemory, PatternSig: "sig-1", Body: "prefer the smaller diff"}

	if err := learner.applyAccepted(context.Background(), proposal); err != nil {
		t.Fatalf("applyAccepted: %v", err)
	}

	body, err := os.ReadFile(workspace.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	want := "- [learn:sig-1] prefer the smaller diff\n"
	if string(body) != want {
		t.Errorf("MEMORY.md = %q, want %q", body, want)
	}
	// The sidecar lock directory exists only if the write went through the
	// workspace's locking append rather than opening the file directly.
	if _, err := os.Stat(filepath.Join(workspace.Dir, ".memory-locks")); err != nil {
		t.Errorf("learner bypassed the MEMORY.md lock: %v", err)
	}
}
