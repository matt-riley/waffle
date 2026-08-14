// Learning loop mine→propose→validate (#65).
package skill

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill/spec"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/textcut"
)

// Allowed proposal surfaces (#65). Anything else is rejected.
const (
	SurfaceSkill      = "skill"
	SurfaceMemory     = "memory"
	SurfaceConfigStub = "config_stub"
)

// AllowedSurfaces is the closed set of edit targets for learn proposals.
var AllowedSurfaces = map[string]bool{
	SurfaceSkill:      true,
	SurfaceMemory:     true,
	SurfaceConfigStub: true,
}

// FailurePattern is a recurring failure class with evidence session IDs.
type FailurePattern struct {
	// Class is a stable fingerprint (normalized error signature).
	Class string
	// Count of occurrences in the mined window.
	Count int
	// SessionIDs are evidence sessions where the pattern appeared.
	SessionIDs []string
	// Samples are short example lines.
	Samples []string
	// Attribution is optional utility-model labeling (cached).
	Attribution string
}

// Proposal is one constrained edit proposed by the learning loop.
type Proposal struct {
	ID          string
	RunID       string
	Surface     string
	PatternSig  string
	Status      string // proposed | accepted | rejected
	Name        string
	Description string
	Body        string
	Rationale   string // mechanism-specific one-liner from the proposer (#410)
	Audit       string
	CreatedAt   time.Time
	// FromCache is true when the proposal came from the proposal cache (#410).
	FromCache bool
}

// RunResult is the outcome of one learn pass.
type RunResult struct {
	ID              string
	StartedAt       time.Time
	FinishedAt      time.Time
	SinceAt         string
	Patterns        []FailurePattern
	Proposals       []Proposal
	Accepted        int
	Rejected        int
	Pending         int
	ProviderCalls   int
	Digest          string
	FromCacheOnly   bool // true when attribution used only cache
	Cursor          LearnCursor
	ScannedSessions int
	Pages           int
}

// ValidationResult is the fail-closed outcome of held-in/held-out validation
// (#414). Evaluated is true only when a real before/after measurement ran (or
// a scorer error aborted it); deterministic precheck rejections (body too
// short, empty held-in) set Rejected without claiming a measurement, and a
// nil baseline leaves both false so the proposal stays pending. Err is
// non-nil when a scorer/evaluator error aborted the measurement. Only a
// fully evaluated, error-free result that strictly improves held-in without
// regressing held-out may auto-promote.
type ValidationResult struct {
	HeldInBefore  int
	HeldInAfter   int
	HeldOutBefore int
	HeldOutAfter  int
	EvidenceIDs   []string
	Evaluated     bool
	// Rejected marks deterministic precheck rejections that never measured
	// anything: the proposal must be rejected, not held pending (#414 review).
	Rejected bool
	Err      error
}

// Promotable reports whether this result satisfies the conservative
// promotion rule: held-in improves strictly and held-out does not regress,
// with a real measurement behind both counts. Note that PromoteProposal
// additionally requires at least one independent held-out case before an
// automatic accept, so Promotable alone is not the complete promotion gate
// (#414 review).
func (v ValidationResult) Promotable() bool {
	return v.Evaluated && v.Err == nil &&
		v.HeldInBefore > v.HeldInAfter &&
		v.HeldOutAfter <= v.HeldOutBefore
}

// ValidateFunc is the pluggable held-in/held-out validator used by
// PromoteProposal (#65). It returns a ValidationResult plus an audit string.
type ValidateFunc func(p Proposal, heldIn, heldOut []string) (ValidationResult, string)

// ScoreFunc returns a failure count for one session under a pattern signature
// (lower is better). Used by DefaultValidate to compare baseline vs current.
type ScoreFunc func(ctx context.Context, sessionID, patternSig string) (int, error)

// Learner runs mine→propose→validate.
type Learner struct {
	DB       *sql.DB
	Sessions *session.Store
	WS       memory.Workspace
	// Provider optional; when set with Model, used for attribution.
	Provider llm.Provider
	Model    string // utility model; empty skips provider attribution
	// MinCount is the recurrence threshold (default 2).
	MinCount int
	// GitDir is the workspace (or repo) root used for accepted commits; empty
	// disables git and stores audit only.
	GitDir string
	// Now is optional clock for tests.
	Now func() time.Time
	// Validate is optional; when set, overrides DefaultValidate.
	Validate ValidateFunc
	// Score counts current pattern failures per session (default: CountPatternErrors).
	Score ScoreFunc
	// Baseline counts pre-edit pattern failures per session. When set with Score,
	// DefaultValidate compares baseline→score for held-in improve / held-out regress.
	// When nil, validation is not measured: proposals stay pending for owner
	// review and can never be auto-accepted (#414).
	Baseline ScoreFunc
	// ValidateCtx is the context used by default scoring (tests may set it).
	ValidateCtx context.Context
	// StaleRunAfter bounds how old an in-progress run may be before a new run
	// treats it as a crashed/interrupted run and reclaims the loop (#412).
	// Zero means the default (30 minutes).
	StaleRunAfter time.Duration
	// mu serializes in-process Run calls so concurrent /learn triggers cannot
	// interleave (#412).
	mu sync.Mutex
	// lastExistingSkills and lastPriorAttempts are per-Run proposer inputs,
	// kept on the Learner so buildProposals can reuse them without re-reading
	// the workspace and database per pattern (#410).
	lastExistingSkills []SkillSummary
	lastPriorAttempts  []AttemptSummary
}

func (l *Learner) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now().UTC()
}

// LearnCursor is the durable mining high-water mark (#412). Only a
// successfully finished learn run may advance it, so a failed or interrupted
// run never skips evidence. SessionID is the tie-breaker for sessions sharing
// the same updated_at (keyset pagination).
type LearnCursor struct {
	// UpdatedAt is the RFC3339Nano updated_at of the last mined session; empty
	// means start from the beginning.
	UpdatedAt string
	SessionID string
}

// LastRunSince returns the committed cursor's updated_at (the old
// high-water mark display), or "" when no finished run exists.
func LastRunSince(ctx context.Context, db *sql.DB) (string, error) {
	c, err := LoadCommittedCursor(ctx, db)
	return c.UpdatedAt, err
}

// LoadCommittedCursor returns the cursor committed by the most recent
// successfully finished learn run (#412). Failed and in-progress runs never
// advance it.
func LoadCommittedCursor(ctx context.Context, db *sql.DB) (LearnCursor, error) {
	if db == nil {
		return LearnCursor{}, nil
	}
	var c LearnCursor
	err := db.QueryRowContext(ctx, `
		SELECT cursor_updated_at, cursor_session_id
		FROM learn_runs
		WHERE status = 'finished'
		ORDER BY started_at DESC LIMIT 1`).Scan(&c.UpdatedAt, &c.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return LearnCursor{}, nil
	}
	return c, err
}

// MineFailurePatterns pages through every session updated after the cursor in
// (updated_at, id) keyset order and aggregates recurring tool-error classes.
// The fixed page size is a page size, not a total-window cap: a busy learn
// window is drained completely (#412). It returns the patterns, the next
// cursor (position after the last mined session), the count of scanned
// sessions, and the number of pages consumed.
func MineFailurePatterns(ctx context.Context, sessions *session.Store, cursor LearnCursor, pageSize int) ([]FailurePattern, LearnCursor, int, int, error) {
	if sessions == nil {
		return nil, cursor, 0, 0, fmt.Errorf("no session store")
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	type acc struct {
		p    FailurePattern
		seen map[string]bool
	}
	counts := map[string]*acc{}
	scanned := 0
	pages := 0
	for {
		list, err := sessions.ListUpdatedAfter(ctx, cursor.UpdatedAt, cursor.SessionID, pageSize)
		if err != nil {
			return nil, cursor, scanned, pages, err
		}
		if len(list) == 0 {
			break
		}
		// A page is counted only when it carried work: a drained cursor must
		// not report a phantom page (#412 review).
		pages++
		for _, sess := range list {
			turns, err := sessions.Turns(ctx, sess.ID)
			if err != nil {
				return nil, cursor, scanned, pages, err
			}
			for _, m := range turns {
				for _, b := range m.Blocks {
					if b.Type != llm.BlockToolResult || b.ToolResult == nil {
						continue
					}
					if !b.ToolResult.IsError && !strings.Contains(strings.ToLower(b.ToolResult.Content), "error:") {
						continue
					}
					sig, sample := fingerprintError(b.ToolResult.Content)
					if sig == "" {
						continue
					}
					a := counts[sig]
					if a == nil {
						a = &acc{p: FailurePattern{Class: sig}, seen: map[string]bool{}}
						counts[sig] = a
					}
					a.p.Count++
					if !a.seen[sess.ID] {
						a.seen[sess.ID] = true
						a.p.SessionIDs = append(a.p.SessionIDs, sess.ID)
					}
					if len(a.p.Samples) < 3 {
						a.p.Samples = append(a.p.Samples, sample)
					}
				}
			}
			scanned++
			cursor = LearnCursor{UpdatedAt: sess.UpdatedAt.Format(time.RFC3339Nano), SessionID: sess.ID}
		}
		if len(list) < pageSize {
			break
		}
	}
	out := make([]FailurePattern, 0, len(counts))
	for _, a := range counts {
		if a.p.Count >= 2 {
			out = append(out, a.p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Class < out[j].Class
	})
	return out, cursor, scanned, pages, nil
}

// contentHash for attribution cache: class + samples + session ids.
func patternHash(p FailurePattern) string {
	h := sha256.New()
	_, _ = h.Write([]byte(p.Class))
	for _, s := range p.Samples {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(s))
	}
	for _, id := range p.SessionIDs {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AttributePatterns labels patterns with the utility model when configured.
// Cached by content hash so re-runs on unchanged data make 0 provider calls.
func (l *Learner) AttributePatterns(ctx context.Context, patterns []FailurePattern) (out []FailurePattern, calls int, err error) {
	out = make([]FailurePattern, len(patterns))
	copy(out, patterns)
	if l.DB == nil {
		return out, 0, nil
	}
	for i := range out {
		hash := patternHash(out[i])
		var cached string
		qerr := l.DB.QueryRowContext(ctx, `SELECT attribution FROM learn_attr_cache WHERE content_hash = ?`, hash).Scan(&cached)
		if qerr == nil && cached != "" {
			out[i].Attribution = cached
			continue
		}
		if !errors.Is(qerr, sql.ErrNoRows) && qerr != nil {
			return out, calls, qerr
		}
		// No cache hit: call provider when configured, else heuristic.
		label := "recurring tool failure: " + out[i].Class
		if l.Provider != nil && l.Model != "" {
			label, err = l.attributeOnce(ctx, out[i])
			if err != nil {
				return out, calls, err
			}
			calls++
		}
		out[i].Attribution = label
		_, err = l.DB.ExecContext(ctx, `
			INSERT OR REPLACE INTO learn_attr_cache (content_hash, attribution, created_at)
			VALUES (?, ?, ?)`, hash, label, l.now().Format(time.RFC3339Nano))
		if err != nil {
			return out, calls, err
		}
	}
	return out, calls, nil
}

func (l *Learner) attributeOnce(ctx context.Context, p FailurePattern) (string, error) {
	prompt := fmt.Sprintf(
		"In one short label (≤12 words), name the root-cause class of this recurring agent tool failure.\nSignature: %s\nSamples:\n- %s\nReply with only the label.",
		p.Class, strings.Join(p.Samples, "\n- "))
	resp, err := l.Provider.Complete(ctx, llm.Request{
		Model:     l.Model,
		Messages:  []llm.Message{llm.UserText(prompt)},
		MaxTokens: 64,
	}, nil)
	if err != nil {
		return "", err
	}
	label := strings.TrimSpace(resp.Message.Text())
	if label == "" {
		label = "recurring tool failure: " + p.Class
	}
	if len(label) > 120 {
		label = textcut.Cut(label, 120)
	}
	return label, nil
}

// ValidateSurface rejects non-enumerated proposal surfaces (#65).
func ValidateSurface(surface string) error {
	if !AllowedSurfaces[surface] {
		return fmt.Errorf("proposal surface %q rejected: allowed surfaces are skill, memory, config_stub", surface)
	}
	return nil
}

// SplitHeld splits evidence session IDs into held-in (first half, min 1) and
// held-out (remainder). Odd counts put the extra in held-in.
func SplitHeld(sessionIDs []string) (heldIn, heldOut []string) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	if len(sessionIDs) == 1 {
		return sessionIDs, nil
	}
	n := (len(sessionIDs) + 1) / 2
	return append([]string(nil), sessionIDs[:n]...), append([]string(nil), sessionIDs[n:]...)
}

// DefaultPromote is the conservative promotion rule: accept only when held-in
// improves and held-out does not regress.
func DefaultPromote(improve, regress bool) bool {
	return improve && !regress
}

// CountPatternErrors counts tool-error turns in a session whose fingerprint or
// content matches patternSig. Lower is better (fewer residual failures).
func CountPatternErrors(ctx context.Context, sessions *session.Store, sessionID, patternSig string) (int, error) {
	if sessions == nil || sessionID == "" {
		return 0, nil
	}
	turns, err := sessions.Turns(ctx, sessionID)
	if err != nil {
		// Missing/unknown sessions score as zero (no residual errors observed).
		if errors.Is(err, session.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	sig := strings.ToLower(strings.TrimSpace(patternSig))
	n := 0
	for _, m := range turns {
		for _, b := range m.Blocks {
			if b.Type != llm.BlockToolResult || b.ToolResult == nil {
				continue
			}
			if !b.ToolResult.IsError && !strings.Contains(strings.ToLower(b.ToolResult.Content), "error:") {
				continue
			}
			content := b.ToolResult.Content
			fp, _ := fingerprintError(content)
			if sig != "" {
				if fp == sig || strings.Contains(strings.ToLower(content), sig) ||
					(fp != "" && strings.Contains(fp, sig)) ||
					(fp != "" && strings.Contains(sig, fp)) {
					n++
				}
				continue
			}
			n++
		}
	}
	return n, nil
}

// sumScores totals ScoreFunc results over session IDs. Scorer errors fail
// closed (#414): a missing measurement must not silently convert into a zero
// and a green result.
func sumScores(ctx context.Context, score ScoreFunc, sessionIDs []string, patternSig string) (int, error) {
	if score == nil || len(sessionIDs) == 0 {
		return 0, nil
	}
	total := 0
	for _, id := range sessionIDs {
		n, err := score(ctx, id, patternSig)
		if err != nil {
			return 0, fmt.Errorf("score session %s: %w", id, err)
		}
		total += n
	}
	return total, nil
}

// DefaultValidate scores a proposal against held-in/held-out session splits (#65).
//
// With a baseline, compares failure counts (lower is better): held-in must
// improve strictly (before must be > after) and held-out must not regress.
// Scorer errors abort the measurement and fail the proposal closed.
//
// Without a baseline there is no real before/after pair, so the result is
// marked unevaluated (Evaluated=false) and can never auto-promote (#414): the
// caller keeps the proposal pending for owner review instead of labeling it
// accepted on the strength of an audit string that was never measured.
func DefaultValidate(ctx context.Context, score, baseline ScoreFunc, p Proposal, heldIn, heldOut []string) (ValidationResult, string) {
	evidence := append(append([]string(nil), heldIn...), heldOut...)
	if strings.TrimSpace(p.Body) == "" || len(strings.TrimSpace(p.Body)) < 20 {
		return ValidationResult{Rejected: true, EvidenceIDs: evidence}, "body too short; rejected as non-substantive"
	}
	if len(heldIn) == 0 {
		return ValidationResult{Rejected: true, EvidenceIDs: evidence}, "held-in has no evidence"
	}
	if baseline == nil {
		return ValidationResult{EvidenceIDs: evidence},
			"not measured: no baseline evaluator; proposal kept pending for owner review"
	}
	if score == nil {
		// A nil after-scorer must never masquerade as a real measurement: an
		// all-zero "after" could look like a strict improvement (#414 review).
		return ValidationResult{Evaluated: true, Err: errors.New("no after-scorer configured"), EvidenceIDs: evidence},
			"no after-scorer configured; failed closed"
	}
	inBefore, err := sumScores(ctx, baseline, heldIn, p.PatternSig)
	if err != nil {
		return ValidationResult{Evaluated: true, Err: err, EvidenceIDs: evidence}, "held-in baseline scoring failed: " + err.Error()
	}
	inAfter, err := sumScores(ctx, score, heldIn, p.PatternSig)
	if err != nil {
		return ValidationResult{Evaluated: true, Err: err, EvidenceIDs: evidence}, "held-in scoring failed: " + err.Error()
	}
	outBefore, err := sumScores(ctx, baseline, heldOut, p.PatternSig)
	if err != nil {
		return ValidationResult{Evaluated: true, Err: err, EvidenceIDs: evidence}, "held-out baseline scoring failed: " + err.Error()
	}
	outAfter, err := sumScores(ctx, score, heldOut, p.PatternSig)
	if err != nil {
		return ValidationResult{Evaluated: true, Err: err, EvidenceIDs: evidence}, "held-out scoring failed: " + err.Error()
	}
	v := ValidationResult{
		HeldInBefore:  inBefore,
		HeldInAfter:   inAfter,
		HeldOutBefore: outBefore,
		HeldOutAfter:  outAfter,
		EvidenceIDs:   evidence,
		Evaluated:     true,
	}
	audit := fmt.Sprintf("held-in %d→%d; held-out %d→%d", inBefore, inAfter, outBefore, outAfter)
	switch {
	case v.Promotable():
		audit = "held-in improved; held-out did not regress; " + audit
	case outAfter > outBefore:
		audit = "held-out regress; " + audit
	case inAfter >= inBefore:
		audit = "held-in did not improve; " + audit
	}
	return v, audit
}

// defaultValidate is the Learner-bound ValidateFunc using Score/Baseline/Sessions.
func (l *Learner) defaultValidate(p Proposal, heldIn, heldOut []string) (ValidationResult, string) {
	ctx := context.Background()
	if l != nil && l.ValidateCtx != nil {
		ctx = l.ValidateCtx
	}
	var score ScoreFunc
	var baseline ScoreFunc
	if l != nil {
		score = l.Score
		baseline = l.Baseline
		if score == nil && l.Sessions != nil {
			sessions := l.Sessions
			score = func(ctx context.Context, sessionID, patternSig string) (int, error) {
				return CountPatternErrors(ctx, sessions, sessionID, patternSig)
			}
		}
	}
	return DefaultValidate(ctx, score, baseline, p, heldIn, heldOut)
}

// Propose builds constrained proposals for patterns (skill surface by default).
// Replaced by the mechanism-specific Learner.Propose (#410).
func Propose(runID string, patterns []FailurePattern, minCount int) ([]Proposal, error) {
	props, _, err := (&Learner{}).Propose(context.Background(), runID, patterns, minCount)
	return props, err
}

// PromoteProposal applies, rejects, or holds a proposal under the fail-closed
// held-in/out rule (#414).
//
//   - Scorer/evaluator errors fail the proposal closed: rejected, persisted,
//     never treated as zero.
//   - A real before/after measurement must strictly improve held-in and not
//     regress held-out, with at least one independent held-out case, before an
//     automatic accept can write anything.
//   - No baseline (unevaluated) or no held-out evidence keeps the proposal
//     pending for owner review; it is never labeled accepted.
func (l *Learner) PromoteProposal(ctx context.Context, p Proposal, pattern FailurePattern) (Proposal, error) {
	if err := ValidateSurface(p.Surface); err != nil {
		p.Status = "rejected"
		p.Audit = err.Error()
		if serr := l.storeProposal(ctx, p); serr != nil {
			return p, serr
		}
		return p, nil
	}
	heldIn, heldOut := SplitHeld(pattern.SessionIDs)
	// Prefer pattern class as the signature for scoring when proposal lacks one.
	if p.PatternSig == "" {
		p.PatternSig = pattern.Class
	}
	validate := l.Validate
	if validate == nil {
		validate = l.defaultValidate
	}
	result, audit := validate(p, heldIn, heldOut)
	p.Audit = audit
	if result.Err != nil {
		// Evaluator/scorer errors fail closed and are persisted (#414).
		p.Status = "rejected"
		p.Audit = "rejected: evaluator error; " + audit
		if err := l.storeProposal(ctx, p); err != nil {
			return p, err
		}
		return p, nil
	}
	if result.Rejected {
		// Deterministic precheck rejections (body too short, no held-in
		// evidence) are rejected, never held pending (#414 review).
		p.Status = "rejected"
		p.Audit = "rejected: " + audit
		if err := l.storeProposal(ctx, p); err != nil {
			return p, err
		}
		return p, nil
	}
	if !result.Promotable() {
		if result.Evaluated && result.HeldOutAfter > result.HeldOutBefore {
			p.Status = "rejected"
			p.Audit = "rejected: held-out regress; " + audit
		} else if result.Evaluated && result.HeldInAfter >= result.HeldInBefore {
			p.Status = "rejected"
			p.Audit = "rejected: held-in did not improve; " + audit
		} else {
			// Not measured (no baseline) or no independent held-out case: keep
			// pending for owner review rather than labeling it accepted (#414).
			p.Status = "proposed"
			p.Audit = "pending owner review; " + audit
		}
		if err := l.storeProposal(ctx, p); err != nil {
			return p, err
		}
		return p, nil
	}
	// Held-out evidence is required for automatic promotion; without it the
	// proposal stays pending under the documented owner-approval fallback.
	if len(heldOut) == 0 {
		p.Status = "proposed"
		p.Audit = "pending owner review; no independent held-out case; " + audit
		if err := l.storeProposal(ctx, p); err != nil {
			return p, err
		}
		return p, nil
	}
	// Accept: apply surface edit.
	if err := l.applyAccepted(ctx, &p); err != nil {
		p.Status = "rejected"
		p.Audit = "apply failed: " + err.Error()
		_ = l.storeProposal(ctx, p)
		return p, err
	}
	p.Status = "accepted"
	p.Audit = audit
	// Git commit when possible.
	if msg, err := l.commitAccepted(p); err != nil {
		p.Audit += "; git: " + err.Error() + " (stored audit only)"
	} else if msg != "" {
		p.Audit += "; git commit: " + msg
	} else {
		p.Audit += "; no git repo — audit stored"
	}
	if err := l.storeProposal(ctx, p); err != nil {
		return p, err
	}
	return p, nil
}

func (l *Learner) applyAccepted(ctx context.Context, p *Proposal) error {
	switch p.Surface {
	case SurfaceSkill:
		if l.WS.Dir == "" {
			return errors.New("no workspace")
		}
		// Write as inactive skill until validated/activated.
		c := memory.Candidate{
			Kind:        "skill",
			Name:        p.Name,
			Description: p.Description,
			Body:        p.Body,
			Provenance: memory.Provenance{
				SourceKind: "reflection",
				SourceID:   p.ID,
				TrustClass: "model_derived",
			},
		}
		if err := writeSkillInactive(l.WS, c); err != nil {
			return err
		}
		return SetSkillStatusRecord(ctx, l.DB, StatusRecord{
			Name:   p.Name,
			Status: StatusInactive,
			Source: "learn",
		})
	case SurfaceMemory:
		// Append a config-stub style note candidate under pending via gate body.
		// Through the workspace so it takes the shared MEMORY.md lock (#267):
		// opening the file directly here let a concurrent read-modify-write in
		// another process erase a note already reported as applied.
		return l.WS.AppendRawLine(fmt.Sprintf("- [learn:%s] %s", p.PatternSig, oneLine(p.Body)))
	case SurfaceConfigStub:
		// Write a non-live stub under workspace for operator review.
		dir := filepath.Join(l.WS.Dir, "config-stubs")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		name := p.Name
		if name == "" {
			name = "stub"
		}
		return os.WriteFile(filepath.Join(dir, name+".toml.stub"), []byte(p.Body+"\n"), 0o600)
	default:
		return ValidateSurface(p.Surface)
	}
}

func oneLine(s string) string {
	s = memory.OneLine(s)
	if len(s) > 200 {
		s = textcut.Cut(s, 200) + "…"
	}
	return s
}

func (l *Learner) commitAccepted(p Proposal) (string, error) {
	dir := l.GitDir
	if dir == "" {
		dir = l.WS.Dir
	}
	if dir == "" {
		return "", nil
	}
	// Detect git repo.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", nil
	}
	// Stage only the paths the accepted proposal wrote (skill file, memory
	// file, or config stub), never the whole workspace: `git add -A` swept
	// every unrelated tracked edit, deletion, and untracked file into the
	// learning commit (#295).
	paths, err := l.proposalPaths(dir, p)
	if err != nil {
		return "", err
	}
	addArgs := append([]string{"-C", dir, "--literal-pathspecs", "add", "--"}, paths...)
	if out, err := exec.Command("git", addArgs...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	msg := fmt.Sprintf("learn: accept %s proposal for pattern %q", p.Surface, p.PatternSig)
	// Commit only the proposal paths: a plain `git commit` would also sweep
	// in unrelated changes that were already staged in the index (#295).
	// `git commit -- <paths>` commits just those paths' contents.
	commitArgs := append([]string{"-C", dir, "commit", "-m", msg, "--"}, paths...)
	out, err := exec.Command("git", commitArgs...).CombinedOutput()
	if err != nil {
		// Nothing to commit is fine.
		if strings.Contains(string(out), "nothing to commit") {
			return msg, nil
		}
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return msg, nil
}

// proposalPaths returns the exact paths (relative to dir) an accepted
// proposal writes, so the learning commit stays limited to the accepted
// edit (#295). The paths are relative to the git working tree dir because
// `git add` resolves pathspecs from its -C directory.
func (l *Learner) proposalPaths(dir string, p Proposal) ([]string, error) {
	var abs string
	switch p.Surface {
	case SurfaceSkill:
		if l.WS.Dir == "" {
			return nil, errors.New("no workspace")
		}
		abs = filepath.Join(l.WS.SkillsDir(), p.Name, "SKILL.md")
	case SurfaceMemory:
		if l.WS.Dir == "" {
			return nil, errors.New("no workspace")
		}
		abs = l.WS.MemoryPath()
	case SurfaceConfigStub:
		if l.WS.Dir == "" {
			return nil, errors.New("no workspace")
		}
		name := p.Name
		if name == "" {
			name = "stub"
		}
		abs = filepath.Join(l.WS.Dir, "config-stubs", name+".toml.stub")
	default:
		return nil, ValidateSurface(p.Surface)
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return nil, fmt.Errorf("proposal path %q outside git dir %q: %w", abs, dir, err)
	}
	return []string{rel}, nil
}

func (l *Learner) storeProposal(ctx context.Context, p Proposal) error {
	if l.DB == nil {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{
		"name":        p.Name,
		"description": p.Description,
		"body":        p.Body,
		"surface":     p.Surface,
	})
	resolved := ""
	if p.Status == "accepted" || p.Status == "rejected" {
		resolved = l.now().Format(time.RFC3339Nano)
	}
	_, err := l.DB.ExecContext(ctx, `
		INSERT INTO learn_proposals (id, run_id, surface, pattern_sig, status, payload, audit, created_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			audit = excluded.audit,
			resolved_at = excluded.resolved_at`,
		p.ID, p.RunID, p.Surface, p.PatternSig, p.Status, string(payload), p.Audit,
		p.CreatedAt.Format(time.RFC3339Nano), resolved)
	return err
}

// Run executes a full mine→propose→validate pass (#412). The durable cursor
// only advances on a fully successful run; any failure marks the run failed
// with an error summary and leaves the cursor unchanged so the failed page is
// retried on the next run. Concurrent triggers are serialized (in-process by
// the learner mutex; cross-process by refusing to start while a fresh run is
// in progress; stale in-progress rows from a crash are reclaimed).
func (l *Learner) Run(ctx context.Context) (*RunResult, error) {
	if l.Sessions == nil {
		return nil, fmt.Errorf("no session store")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	minCount := l.MinCount
	if minCount <= 0 {
		minCount = 2
	}
	started := l.now()
	runID, err := id.New("learn-")
	if err != nil {
		return nil, err
	}
	if l.DB != nil {
		staleAfter := l.StaleRunAfter
		if staleAfter <= 0 {
			staleAfter = 30 * time.Minute
		}
		// Reclaim crashed/interrupted runs so a restart can never wedge the
		// loop. Their cursor was never committed, so the failed page is retried.
		if _, err := l.DB.ExecContext(ctx, `
			UPDATE learn_runs SET status = 'failed', error = 'interrupted (crash/restart); cursor not advanced'
			WHERE status = 'running' AND started_at < ?`,
			started.Add(-staleAfter).Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		// Claim the loop atomically: the conditional insert and its
		// existence check are one statement, so two processes cannot both
		// observe zero running rows and start (#412 review). RowsAffected is
		// 0 when another process won the claim.
		res, err := l.DB.ExecContext(ctx, `
			INSERT INTO learn_runs (id, started_at, since_at, status)
			SELECT ?, ?, '', 'running'
			WHERE NOT EXISTS (SELECT 1 FROM learn_runs WHERE status = 'running')`,
			runID, started.Format(time.RFC3339Nano))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, errors.New("a learn run is already in progress; refusing to start a concurrent run")
		}
	}

	cursor, err := LoadCommittedCursor(ctx, l.DB)
	if err != nil {
		l.failRun(ctx, runID, err)
		return nil, err
	}
	patterns, nextCursor, scanned, pages, err := MineFailurePatterns(ctx, l.Sessions, cursor, 50)
	if err != nil {
		l.failRun(ctx, runID, err)
		return nil, err
	}
	patterns, calls, err := l.AttributePatterns(ctx, patterns)
	if err != nil {
		l.failRun(ctx, runID, err)
		return nil, err
	}

	props, propCalls, err := l.Propose(ctx, runID, patterns, minCount)
	if err != nil {
		l.failRun(ctx, runID, err)
		return nil, err
	}
	calls += propCalls

	// Index patterns by class for promotion evidence.
	byClass := map[string]FailurePattern{}
	for _, p := range patterns {
		byClass[p.Class] = p
	}

	accepted, rejected, pending := 0, 0, 0
	resolved := make([]Proposal, 0, len(props))
	for _, prop := range props {
		pat := byClass[prop.PatternSig]
		out, err := l.PromoteProposal(ctx, prop, pat)
		if err != nil {
			l.failRun(ctx, runID, err)
			return nil, err
		}
		switch out.Status {
		case "accepted":
			accepted++
		case "rejected":
			rejected++
		default:
			pending++
		}
		resolved = append(resolved, out)
	}

	digest := formatDigest(patterns, resolved, calls, scanned, pages, nextCursor)
	finished := l.now()
	if l.DB != nil {
		// Only a fully successful run commits the durable cursor (#412).
		_, err = l.DB.ExecContext(ctx, `
			UPDATE learn_runs SET finished_at = ?, status = 'finished', error = '',
				pattern_count = ?, proposal_count = ?,
				accepted_count = ?, rejected_count = ?, pending_count = ?, provider_calls = ?, digest = ?,
				scanned_sessions = ?, pages = ?, cursor_updated_at = ?, cursor_session_id = ?
			WHERE id = ?`,
			finished.Format(time.RFC3339Nano), len(patterns), len(props),
			accepted, rejected, pending, calls, digest, scanned, pages,
			nextCursor.UpdatedAt, nextCursor.SessionID, runID)
		if err != nil {
			l.failRun(ctx, runID, err)
			return nil, err
		}
	}
	return &RunResult{
		ID:              runID,
		StartedAt:       started,
		FinishedAt:      finished,
		SinceAt:         cursor.UpdatedAt,
		Patterns:        patterns,
		Proposals:       resolved,
		Accepted:        accepted,
		Rejected:        rejected,
		Pending:         pending,
		ProviderCalls:   calls,
		Digest:          digest,
		FromCacheOnly:   calls == 0 && len(patterns) > 0,
		Cursor:          nextCursor,
		ScannedSessions: scanned,
		Pages:           pages,
	}, nil
}

// failRun marks the in-progress run failed with an explicit error summary.
// It never advances the cursor, so the next run retries from the last
// committed position (#412).
func (l *Learner) failRun(ctx context.Context, runID string, runErr error) {
	if l.DB == nil {
		return
	}
	if _, err := l.DB.ExecContext(ctx, `
		UPDATE learn_runs SET status = 'failed', error = ?, finished_at = ? WHERE id = ?`,
		runErr.Error(), l.now().Format(time.RFC3339Nano), runID); err != nil {
		slog.Warn("learn: failed to record failed run", "run_id", runID, "err", err)
	}
}

func formatDigest(patterns []FailurePattern, props []Proposal, calls, scanned, pages int, cursor LearnCursor) string {
	var b strings.Builder
	accepted, rejected, pending := 0, 0, 0
	for _, p := range props {
		switch p.Status {
		case "accepted":
			accepted++
		case "rejected":
			rejected++
		default:
			pending++
		}
	}
	fmt.Fprintf(&b, "patterns=%d proposals=%d accepted=%d rejected=%d pending=%d provider_calls=%d scanned_sessions=%d pages=%d\n",
		len(patterns), len(props), accepted, rejected, pending, calls, scanned, pages)
	fmt.Fprintf(&b, "cursor=%s/%s\n", cursor.UpdatedAt, cursor.SessionID)
	for _, p := range patterns {
		fmt.Fprintf(&b, "  (%d×) %s sessions=%s", p.Count, p.Class, strings.Join(p.SessionIDs, ","))
		if p.Attribution != "" {
			fmt.Fprintf(&b, " attr=%q", p.Attribution)
		}
		b.WriteByte('\n')
	}
	for _, p := range props {
		fmt.Fprintf(&b, "  proposal %s %s status=%s", p.Surface, p.Name, p.Status)
		if p.Rationale != "" {
			fmt.Fprintf(&b, " rationale=%q", p.Rationale)
		}
		if p.FromCache {
			b.WriteString(" [cache]")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// writeSkillInactive writes SKILL.md with status: inactive recorded under
// the waffle metadata key (#396). Non-conforming output is refused, not
// written; the provenance markers are dropped (write-only).
func writeSkillInactive(w memory.Workspace, c memory.Candidate) error {
	description := oneLine(c.Description)
	if err := spec.Validate(c.Name, description, nil, c.Body, c.Name); err != nil {
		return fmt.Errorf("refuse to write non-conforming skill: %w", err)
	}
	dir := filepath.Join(w.SkillsDir(), c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Refuse to overwrite an active skill without validation (#65).
	path := filepath.Join(dir, "SKILL.md")
	if raw, err := os.ReadFile(path); err == nil {
		if isActiveFrontmatter(string(raw)) {
			return fmt.Errorf("cannot overwrite active skill %q without validation; deactivate first or use waffle skills activate after review", c.Name)
		}
	}
	content := spec.MarshalSKILL(map[string]string{
		"name":               c.Name,
		"description":        description,
		spec.WaffleStatusKey: "inactive",
	}, c.Body)
	return os.WriteFile(path, content, 0o644)
}

// NewLearnerFromStore is a convenience constructor.
func NewLearnerFromStore(st *store.Store, sessions *session.Store, ws memory.Workspace) *Learner {
	l := &Learner{Sessions: sessions, WS: ws}
	if st != nil {
		l.DB = st.DB
	}
	return l
}
