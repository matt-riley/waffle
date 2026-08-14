package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// scriptedProposer returns a fixed payload and records the requests it saw.
type scriptedProposer struct {
	payload string
	err     error
	reqs    []llm.Request
}

func (p *scriptedProposer) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.reqs = append(p.reqs, req)
	if p.err != nil {
		return nil, p.err
	}
	return &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: p.payload}}},
	}, nil
}

func patternFor(class string, count int) FailurePattern {
	return FailurePattern{
		Class:       class,
		Count:       count,
		SessionIDs:  []string{"s1", "s2"},
		Samples:     []string{"error: " + class + " while running tool"},
		Attribution: "root cause: " + class,
	}
}

func TestProposeFallbackMechanismSpecific(t *testing.T) {
	l := &Learner{}
	patterns := []FailurePattern{
		patternFor("permission denied writing protected config path", 3),
		patternFor("command not found for foobar-cli", 2),
	}
	props, calls, err := l.Propose(context.Background(), "run-fb", patterns, 2)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("fallback made %d provider calls", calls)
	}
	if len(props) != 2 {
		t.Fatalf("props = %+v, want 2 mechanism candidates", props)
	}
	for _, p := range props {
		if p.Rationale == "" {
			t.Errorf("proposal %s lacks rationale", p.Name)
		}
		if isGenericRestatement(p.Body) {
			t.Errorf("proposal %s is a generic restatement:\n%s", p.Name, p.Body)
		}
		if !strings.Contains(p.Body, "## Mechanism") {
			t.Errorf("proposal %s lacks mechanism section:\n%s", p.Name, p.Body)
		}
		// Concrete mechanism markers must appear.
		if !strings.Contains(p.Body, "ls -la") && !strings.Contains(p.Body, "which") {
			t.Errorf("proposal %s lacks a concrete mechanism step:\n%s", p.Name, p.Body)
		}
	}
}

// TestProposeFallbackSkipsUnknownClass proves the fallback never manufactures
// boilerplate: a class with no mechanism rule yields no candidate at all.
func TestProposeFallbackSkipsUnknownClass(t *testing.T) {
	l := &Learner{}
	props, _, err := l.Propose(context.Background(), "run-unk", []FailurePattern{patternFor("quantum flux anomaly in zeta subsystem", 9)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 0 {
		t.Fatalf("unknown class produced boilerplate: %+v", props)
	}
}

func TestDecodeCandidatesStrict(t *testing.T) {
	if _, err := decodeCandidates("not json at all"); err == nil {
		t.Fatal("expected structured-decode failure")
	}
	if _, err := decodeCandidates(`{"candidates":[]}`); err == nil {
		t.Fatal("expected failure for empty candidates")
	}
	if _, err := decodeCandidates(`{"candidates":[{"surface":"skill","name":"ok-name","rationale":"r","body":"x"}]}`); err != nil {
		t.Fatal(err)
	}
	// A bare array is tolerated at the envelope level.
	edits, err := decodeCandidates(`[{"surface":"skill","name":"ok-name","rationale":"r","body":"x"}]`)
	if err != nil || len(edits) != 1 {
		t.Fatalf("bare array decode = %+v, %v", edits, err)
	}
}

func TestValidateCandidatesClosedSurfaceAndBudget(t *testing.T) {
	req := ProposalRequest{AllowedSurfaces: []string{SurfaceSkill, SurfaceMemory}, MaxCandidates: 2}
	edits := []CandidateEdit{
		{Surface: "system_prompt", Name: "bad", Rationale: "r", Body: "should be dropped: unknown surface with a long body"},          // unknown surface
		{Surface: SurfaceSkill, Name: "ok-one", Rationale: "r", Body: "one concrete mechanism: run `ls -la` then `chmod` and verify"}, // ok
		{Surface: SurfaceSkill, Name: "ok-two", Rationale: "r", Body: "two concrete mechanism: run `which` then install and verify"},  // ok
		{Surface: SurfaceSkill, Name: "ok-three", Rationale: "r", Body: "three concrete mechanism: run `curl` then retry and verify"}, // over budget
		{Surface: SurfaceSkill, Name: "empty", Rationale: "r", Body: "   "},                                                           // empty body
	}
	got := validateCandidates(edits, req)
	if len(got) != 2 {
		t.Fatalf("validated = %+v, want exactly 2 (budget cap, closed surface)", got)
	}
	if got[0].Name != "ok-one" || got[1].Name != "ok-two" {
		t.Fatalf("validated order = %+v", got)
	}
}

// TestValidateCandidatesRejectsGenericRestatement proves a boilerplate body
// cannot be auto-promoted into a candidate (#410).
func TestValidateCandidatesRejectsGenericRestatement(t *testing.T) {
	req := ProposalRequest{AllowedSurfaces: []string{SurfaceSkill}, MaxCandidates: 3}
	generic := CandidateEdit{
		Surface: SurfaceSkill, Name: "recover-generic",
		Rationale: "generic",
		Body:      "1. Reproduce with the same tool input.\n2. Fix the root cause.\n3. Re-run and confirm the error is gone.",
	}
	got := validateCandidates([]CandidateEdit{generic}, req)
	if len(got) != 0 {
		t.Fatalf("generic restatement passed validation: %+v", got)
	}
}

// TestProposeModelStructuredAndCached verifies the model path strictly decodes
// candidates, persists them to cache, and a second run on identical inputs
// makes zero provider calls (cache hit) while a changed evidence set misses.
func TestProposeModelStructuredAndCached(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedFailureClass(t, sessions, "a", "error: permission denied writing protected config path", 3)

	p := &scriptedProposer{payload: `{"candidates":[
		{"surface":"skill","name":"recover-perm","rationale":"fix file mode","body":"1. run ` + "`ls -la`" + ` on the path\n2. chmod the needed bits\n3. verify with ls"},
		{"surface":"memory","name":"","rationale":"note the deploy policy","body":"deploys require the deploy key; keep it in the secret store"}]}`}
	l := NewLearnerFromStore(st, sessions, memory.Workspace{Dir: t.TempDir()})
	l.Provider = p
	l.Model = "utility-m"

	patterns, _, _, _, err := MineFailurePatterns(ctx, sessions, LearnCursor{}, 20)
	if err != nil {
		t.Fatal(err)
	}
	props, calls, err := l.Propose(context.Background(), "run-1", patterns, 2)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if len(props) != 2 {
		t.Fatalf("props = %+v, want 2 (skill + memory)", props)
	}
	for _, prop := range props {
		if prop.Rationale == "" {
			t.Errorf("proposal %s missing rationale", prop.ID)
		}
	}

	// Cache hit: same inputs, zero provider calls.
	p.reqs = nil
	props2, calls2, err := l.Propose(context.Background(), "run-2", patterns, 2)
	if err != nil {
		t.Fatal(err)
	}
	if calls2 != 0 {
		t.Fatalf("cached run made %d calls", calls2)
	}
	if len(props2) != len(props) {
		t.Fatalf("cached props differ: %d vs %d", len(props2), len(props))
	}
	if len(p.reqs) != 0 {
		t.Fatalf("provider called on cache hit: %d requests", len(p.reqs))
	}

	// Changed evidence (new session) → different cache key → model called.
	seedFailureClass(t, sessions, "b", "error: permission denied writing protected config path", 2)
	patterns2, _, _, _, err := MineFailurePatterns(ctx, sessions, LearnCursor{}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, calls3, err := l.Propose(context.Background(), "run-3", patterns2, 2); err != nil {
		t.Fatal(err)
	} else if calls3 == 0 {
		t.Fatal("changed evidence served from stale cache")
	}
}

// TestProposePriorAttemptAvoidance proves a rejected attempt's content hash is
// not re-proposed in a later round (#410).
func TestProposePriorAttemptAvoidance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "pa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedFailureClass(t, sessions, "a", "error: command not found for foobar-cli binary", 3)

	// Store a rejected proposal with the same body the fallback would emit.
	l := NewLearnerFromStore(st, sessions, memory.Workspace{Dir: t.TempDir()})
	props, _, err := l.Propose(context.Background(), "run-first", []FailurePattern{patternFor("command not found for foobar-cli", 3)}, 2)
	if err != nil || len(props) != 1 {
		t.Fatalf("first propose = %+v, %v", props, err)
	}
	if err := l.storeProposal(ctx, Proposal{
		ID: "prop-prior", RunID: "run-first", Surface: SurfaceSkill,
		PatternSig: "command not found for foobar-cli", Status: "rejected",
		Body: props[0].Body, Audit: "rejected in review",
	}); err != nil {
		t.Fatal(err)
	}
	// The next round must not re-propose the same content hash.
	props2, _, err := l.Propose(context.Background(), "run-second", []FailurePattern{patternFor("command not found for foobar-cli", 3)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range props2 {
		if p.Body == props[0].Body {
			t.Fatalf("re-proposed rejected content hash: %s", p.ID)
		}
	}
}

// TestProposePrefersExistingInactiveSkill verifies the proposer prefers
// updating a matching inactive skill's name over minting a redundant
// recover-* skill, and never targets an active skill (#410).
func TestProposePrefersExistingInactiveSkill(t *testing.T) {
	ws := memory.Workspace{Dir: t.TempDir()}
	// Write an inactive skill that matches the permission pattern.
	skillDir := filepath.Join(ws.SkillsDir(), "handle-permissions")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: handle-permissions\ndescription: Fix permission denied failures.\nmetadata:\n  waffle/status: inactive\n---\n# Handle permissions\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Learner{WS: ws}
	p := &scriptedProposer{payload: `{"candidates":[{"surface":"skill","name":"recover-permissions","rationale":"fix mode","body":"1. run ` + "`ls -la`" + `\n2. chmod the needed bits\n3. verify"}]}`}
	l.Provider = p
	l.Model = "utility-m"
	props, _, err := l.Propose(context.Background(), "run-pref", []FailurePattern{patternFor("permission denied writing protected config path", 3)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("props = %+v", props)
	}
	if props[0].Name != "handle-permissions" {
		t.Fatalf("name = %q, want reuse of matching inactive skill name", props[0].Name)
	}

	// An active matching skill must never be the target.
	active := memory.Workspace{Dir: t.TempDir()}
	adir := filepath.Join(active.SkillsDir(), "active-perm")
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adir, "SKILL.md"), []byte("---\nname: active-perm\ndescription: Fix permission denied failures.\nmetadata:\n  waffle/status: active\n---\n# Active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l2 := &Learner{WS: active}
	l2.Provider = p
	l2.Model = "utility-m"
	props2, _, err := l2.Propose(context.Background(), "run-act", []FailurePattern{patternFor("permission denied writing protected config path", 3)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(props2) != 1 {
		t.Fatalf("props2 = %+v", props2)
	}
	if props2[0].Name == "active-perm" {
		t.Fatalf("active skill targeted for update: %+v", props2[0])
	}
}

// TestProposeModelFailureFallsBack verifies a model error degrades to the
// deterministic fallback instead of dropping the pattern (#410).
func TestProposeModelFailureFallsBack(t *testing.T) {
	l := &Learner{}
	l.Provider = &scriptedProposer{err: errBoom}
	l.Model = "utility-m"
	props, calls, err := l.Propose(context.Background(), "run-err", []FailurePattern{patternFor("permission denied writing protected config path", 3)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 (model failed, no call counted)", calls)
	}
	if len(props) != 1 || props[0].Name != "recover-permissions" {
		t.Fatalf("fallback after model failure = %+v", props)
	}
}

// TestDigestPrintsRationaleWithoutSamples verifies the digest includes
// candidate rationales and dispositions but never full sensitive samples.
func TestDigestPrintsRationaleWithoutSamples(t *testing.T) {
	props := []Proposal{
		{Surface: SurfaceSkill, Name: "recover-perm", Status: "proposed", Rationale: "fix file mode", Body: "secret sample: hunter2 token=abc123"},
	}
	digest := formatDigest(nil, props, 1, 5, 2, LearnCursor{UpdatedAt: "2026-01-01T00:00:00Z"})
	if !strings.Contains(digest, `rationale="fix file mode"`) {
		t.Fatalf("digest missing rationale:\n%s", digest)
	}
	if !strings.Contains(digest, "status=proposed") {
		t.Fatalf("digest missing disposition:\n%s", digest)
	}
	if strings.Contains(digest, "hunter2") || strings.Contains(digest, "abc123") {
		t.Fatalf("digest leaked a sensitive sample:\n%s", digest)
	}
}

var errBoom = errors.New("proposer exploded")

// TestDecodeCandidatesRejectsUnknownFields is the #424 review regression:
// strict decoding must reject extra fields, not silently ignore them.
func TestDecodeCandidatesRejectsUnknownFields(t *testing.T) {
	if _, err := decodeCandidates(`{"candidates":[{"surface":"skill","name":"ok-name","rationale":"r","body":"a mechanism body long enough","extra":"field"}]}`); err == nil {
		t.Fatal("unknown field accepted by strict decode")
	}
	if _, err := decodeCandidates(`{"candidates":[{"surface":"skill","name":"ok-name","rationale":"r","body":"a mechanism body long enough"}],"extra":true}`); err == nil {
		t.Fatal("unknown envelope field accepted")
	}
	// Trailing content after the JSON value is also rejected.
	if _, err := decodeCandidates(`{"candidates":[{"surface":"skill","name":"ok-name","rationale":"r","body":"a mechanism body long enough"}]} trailing`); err == nil {
		t.Fatal("trailing content accepted")
	}
	// Clean payload still decodes.
	edits, err := decodeCandidates(`{"candidates":[{"surface":"skill","name":"ok-name","rationale":"r","body":"a mechanism body long enough"}]}`)
	if err != nil || len(edits) != 1 {
		t.Fatalf("clean decode = %+v, %v", edits, err)
	}
}

// TestValidateCandidatesRejectsUnsafeConfigStubName is the #424 review
// regression: a model-provided name must never escape the config-stubs dir.
func TestValidateCandidatesRejectsUnsafeConfigStubName(t *testing.T) {
	req := ProposalRequest{AllowedSurfaces: []string{SurfaceSkill, SurfaceConfigStub}, MaxCandidates: 3}
	edits := []CandidateEdit{
		{Surface: SurfaceConfigStub, Name: "../../etc/cron", Rationale: "r", Body: "a mechanism body long enough for a config stub candidate"},
		{Surface: SurfaceConfigStub, Name: "..", Rationale: "r", Body: "another mechanism body long enough for a config stub candidate"},
		{Surface: SurfaceConfigStub, Name: "safe-stub", Rationale: "r", Body: "a final mechanism body long enough for a config stub candidate"},
	}
	got := validateCandidates(edits, req)
	if len(got) != 1 || got[0].Name != "safe-stub" {
		t.Fatalf("validated = %+v, want only the safe-stub candidate", got)
	}
}

// TestSkillsMatchDoesNotMatchUnrelatedSkill is the #424 review regression:
// matching must use pattern words against the skill, never the skill's own
// text (which made every skill match).
func TestSkillsMatchDoesNotMatchUnrelatedSkill(t *testing.T) {
	p := patternFor("permission denied writing protected config path", 3)
	c := CandidateEdit{Surface: SurfaceSkill, Name: "recover-permissions", Rationale: "fix file mode", Body: "x"}
	unrelated := SkillSummary{Name: "docker-networking", Status: "inactive", Description: "Troubleshoot container network connectivity."}
	related := SkillSummary{Name: "handle-permissions", Status: "inactive", Description: "Fix permission denied failures."}
	if skillsMatch(unrelated, p, c) {
		t.Fatal("unrelated skill matched the pattern")
	}
	if !skillsMatch(related, p, c) {
		t.Fatal("related skill did not match the pattern")
	}
}

// TestProposeFallsBackWhenValidationDropsAllCandidates is the #424 review
// regression: a model payload whose candidates all fail validation must not
// silently yield zero proposals — the deterministic table takes over.
func TestProposeFallsBackWhenValidationDropsAllCandidates(t *testing.T) {
	p := &scriptedProposer{payload: `{"candidates":[{"surface":"system_prompt","name":"nope","rationale":"r","body":"unknown surface rejected"}]}`}
	l := &Learner{Provider: p, Model: "utility-m"}
	props, _, err := l.Propose(context.Background(), "run-drop", []FailurePattern{patternFor("permission denied writing protected config path", 3)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].Name != "recover-permissions" {
		t.Fatalf("fallback after all-dropped validation = %+v", props)
	}
}
