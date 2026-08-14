// Mechanism-specific proposal generation (#410): the learner no longer emits
// one hard-coded recovery skill per fingerprint. Propose builds a bounded,
// structured candidate set from the normalized signature, representative
// samples, evidence IDs, attribution, relevant existing skill summaries, and
// prior accepted/rejected attempts, using the utility model when configured
// and a deterministic mechanism rule table when it is not. Candidates are
// strictly decoded, validated against the closed surface allowlist, deduped
// by content hash (including against prior attempts), and cache-keyed so
// unchanged evidence makes zero provider calls.
package skill

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/skill/spec"
	"github.com/matt-riley/waffle/internal/textcut"
)

// proposalSchemaVersion is part of the proposal cache key. Bump it whenever
// the prompt, schema, or validation rules change so stale candidates are not
// served from cache.
const proposalSchemaVersion = "learn-propose-v1"

// defaultMaxCandidates bounds how many materially distinct candidates one
// pattern may generate, and is also the token/call budget cap (#410).
const defaultMaxCandidates = 3

// SkillSummary is one existing skill relevant to a pattern, so the model can
// prefer updating a matching inactive skill over minting a redundant one.
type SkillSummary struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // active | inactive
	Description string `json:"description"`
	ContentHash string `json:"content_hash"`
}

// AttemptSummary is one previously stored proposal for the same pattern, so
// later rounds can avoid re-proposing the same content (#410).
type AttemptSummary struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ContentHash string `json:"content_hash"`
}

// ProposalRequest is the bounded context handed to the proposer.
type ProposalRequest struct {
	Pattern         FailurePattern
	AllowedSurfaces []string
	ExistingSkills  []SkillSummary
	PriorAttempts   []AttemptSummary
	MaxCandidates   int
}

// CandidateEdit is one structured candidate returned by the proposer.
type CandidateEdit struct {
	Surface   string `json:"surface"`
	Name      string `json:"name"`
	Rationale string `json:"rationale"`
	Body      string `json:"body"`
}

func (c CandidateEdit) contentHash() string {
	h := sha256.New()
	_, _ = h.Write([]byte(c.Body))
	return hex.EncodeToString(h.Sum(nil))
}

// Propose builds constrained, mechanism-specific proposals for the patterns
// (skill surface by default). It uses the utility model when configured,
// otherwise a deterministic mechanism rule table; generic restatements are
// never generated. Prior attempts and cache hits are honored so unchanged
// evidence costs zero provider calls. The second return is the number of
// provider calls made during proposal generation.
func (l *Learner) Propose(runID string, patterns []FailurePattern, minCount int) ([]Proposal, int, error) {
	if minCount <= 0 {
		minCount = 2
	}
	existing, err := l.existingSkillSummaries()
	if err != nil {
		return nil, 0, err
	}
	var out []Proposal
	calls := 0
	for _, p := range patterns {
		if p.Count < minCount {
			continue
		}
		req := ProposalRequest{
			Pattern:         p,
			AllowedSurfaces: []string{SurfaceSkill, SurfaceMemory, SurfaceConfigStub},
			ExistingSkills:  relevantSkills(existing, p),
			MaxCandidates:   defaultMaxCandidates,
		}
		if l.DB != nil {
			req.PriorAttempts, err = l.priorAttempts(context.Background(), p.Class)
			if err != nil {
				return nil, calls, err
			}
		}
		edits, fromCache, c, err := l.proposeEdits(req)
		if err != nil {
			return nil, calls, err
		}
		calls += c
		props, err := l.buildProposals(runID, p, edits)
		if err != nil {
			return nil, calls, err
		}
		for i := range props {
			props[i].FromCache = fromCache
		}
		out = append(out, props...)
	}
	return out, calls, nil
}

// proposeEdits returns candidates for one pattern from cache, the model, or
// the deterministic fallback (in that order). fromCache reports a cache hit;
// calls reports model calls made.
func (l *Learner) proposeEdits(req ProposalRequest) (edits []CandidateEdit, fromCache bool, calls int, err error) {
	if l.DB != nil {
		key := l.proposalCacheKey(req)
		cached, ok, cerr := l.proposalCacheGet(context.Background(), key)
		if cerr != nil {
			return nil, false, 0, cerr
		}
		if ok {
			edits, derr := decodeCandidates(cached)
			if derr == nil && len(edits) > 0 {
				return edits, true, 0, nil
			}
		}
	}
	if l.Provider != nil && l.Model != "" {
		edits, err = l.proposeOnce(req)
		if err != nil {
			// Model failure falls back to the deterministic table rather than
			// dropping the pattern (#410).
			edits = fallbackPropose(req)
			calls = 0
		} else {
			calls = 1
		}
	} else {
		edits = fallbackPropose(req)
	}
	if l.DB != nil && len(edits) > 0 {
		payload, _ := json.Marshal(edits)
		key := l.proposalCacheKey(req)
		if _, err := l.DB.ExecContext(context.Background(), `
			INSERT OR REPLACE INTO learn_proposal_cache (cache_key, payload, created_at)
			VALUES (?, ?, ?)`, key, string(payload), l.now().Format("2006-01-02T15:04:05.999999999Z07:00")); err != nil {
			return nil, false, calls, err
		}
	}
	return edits, false, calls, nil
}

// proposalCacheKey binds the cache entry to every input that shapes the
// candidates: evidence hash, model, prompt/schema version, the existing
// surface digest, and the prior-attempt digest (#410).
func (l *Learner) proposalCacheKey(req ProposalRequest) string {
	h := sha256.New()
	_, _ = h.Write([]byte(patternHash(req.Pattern)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(l.Model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(proposalSchemaVersion))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(existingSkillsDigest(req.ExistingSkills)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(attemptsDigest(req.PriorAttempts)))
	return hex.EncodeToString(h.Sum(nil))
}

func (l *Learner) proposalCacheGet(ctx context.Context, key string) (string, bool, error) {
	if l.DB == nil {
		return "", false, nil
	}
	var payload string
	err := l.DB.QueryRowContext(ctx, `SELECT payload FROM learn_proposal_cache WHERE cache_key = ?`, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return payload, true, nil
}

func existingSkillsDigest(skills []SkillSummary) string {
	h := sha256.New()
	for _, s := range skills {
		_, _ = fmt.Fprintf(h, "%s|%s|%s|%s\n", s.Name, s.Status, s.Description, s.ContentHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func attemptsDigest(attempts []AttemptSummary) string {
	h := sha256.New()
	for _, a := range attempts {
		_, _ = fmt.Fprintf(h, "%s|%s|%s\n", a.ID, a.Status, a.ContentHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// proposeOnce asks the utility model for a strict JSON candidate set. The
// response must decode as a single candidates array; unknown surfaces, empty
// bodies, and out-of-budget output are rejected by validateCandidates.
func (l *Learner) proposeOnce(req ProposalRequest) ([]CandidateEdit, error) {
	if l.Provider == nil || l.Model == "" {
		return nil, errors.New("proposer is not configured")
	}
	prompt := proposalPrompt(req)
	resp, err := l.Provider.Complete(context.Background(), llm.Request{
		Model:     l.Model,
		MaxTokens: 1600,
		Messages:  []llm.Message{llm.UserText(prompt)},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("propose candidates: %w", err)
	}
	edits, err := decodeCandidates(resp.Message.Text())
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return nil, errors.New("proposer returned no candidates")
	}
	return validateCandidates(edits, req), nil
}

// decodeCandidates strictly decodes the model's JSON candidates array. The
// output must be a JSON object with a "candidates" array; anything else is a
// structured-decode failure.
func decodeCandidates(raw string) ([]CandidateEdit, error) {
	raw = strings.TrimSpace(raw)
	var envelope struct {
		Candidates []CandidateEdit `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		// Accept a bare array too (lenient envelope, strict fields).
		var bare []CandidateEdit
		if berr := json.Unmarshal([]byte(raw), &bare); berr != nil {
			return nil, fmt.Errorf("proposer output is not structured JSON: %w", err)
		}
		envelope.Candidates = bare
	}
	if len(envelope.Candidates) == 0 {
		return nil, errors.New("proposer output has no candidates")
	}
	return envelope.Candidates, nil
}

// validateCandidates applies the closed-surface allowlist and shape rules:
// unknown surfaces, empty bodies, and generic restatements are dropped; names
// must satisfy the spec; output is capped at MaxCandidates.
func validateCandidates(edits []CandidateEdit, req ProposalRequest) []CandidateEdit {
	allowed := map[string]bool{}
	for _, s := range req.AllowedSurfaces {
		allowed[s] = true
	}
	max := req.MaxCandidates
	if max <= 0 {
		max = defaultMaxCandidates
	}
	var out []CandidateEdit
	for _, c := range edits {
		if len(out) >= max {
			break
		}
		if !allowed[c.Surface] || c.Surface == "" {
			continue
		}
		if strings.TrimSpace(c.Body) == "" || len(strings.TrimSpace(c.Body)) < 20 {
			continue
		}
		if isGenericRestatement(c.Body) {
			continue
		}
		if c.Surface == SurfaceSkill {
			if !spec.ValidName(strings.TrimSpace(c.Name)) {
				continue
			}
			c.Name = strings.TrimSpace(c.Name)
		}
		out = append(out, c)
	}
	return out
}

// isGenericRestatement detects the boilerplate recovery shape ("reproduce,
// fix root cause, re-run") with no mechanism-specific content (#410).
func isGenericRestatement(body string) bool {
	b := strings.ToLower(body)
	steps := []string{
		"reproduce with the same tool input",
		"fix the root cause",
		"re-run and confirm",
		"reproduce the issue",
	}
	hits := 0
	for _, s := range steps {
		if strings.Contains(b, s) {
			hits++
		}
	}
	// Two or more boilerplate steps with no concrete mechanism markers
	// (commands, paths, tools) is a generic restatement.
	if hits < 2 {
		return false
	}
	mechanism := []string{"ls -la", "chmod", "chown", "install", "brew", "apt", "pip", "npm", "mkdir", "touch", "test -e", "which", "export ", "curl", "git ", "docker", "restart", "systemctl", "ulimit", "env "}
	for _, m := range mechanism {
		if strings.Contains(b, m) {
			return false
		}
	}
	return true
}

// buildProposals turns validated candidate edits into persisted-shape
// Proposals: dedupe by content hash (including prior attempts), prefer
// updating a matching inactive skill over a redundant recover-* name, and
// never target an active skill.
func (l *Learner) buildProposals(runID string, p FailurePattern, edits []CandidateEdit) ([]Proposal, error) {
	prior := map[string]bool{}
	for _, a := range l.lastPriorAttempts {
		if a.ContentHash != "" {
			prior[a.ContentHash] = true
		}
	}
	seen := map[string]bool{}
	var out []Proposal
	for _, c := range edits {
		hash := c.contentHash()
		if prior[hash] || seen[hash] {
			continue
		}
		seen[hash] = true
		attr := p.Attribution
		if attr == "" {
			attr = p.Class
		}
		name := c.Name
		if c.Surface == SurfaceSkill {
			name = l.preferredSkillName(name, p, c)
		}
		idstr, err := id.New("prop-")
		if err != nil {
			return out, err
		}
		prop := Proposal{
			ID:          idstr,
			RunID:       runID,
			Surface:     c.Surface,
			PatternSig:  p.Class,
			Status:      "proposed",
			Name:        name,
			Description: "auto-mined recovery: " + attr,
			Body:        c.Body,
			Rationale:   c.Rationale,
			CreatedAt:   l.now().UTC(),
		}
		if err := ValidateSurface(prop.Surface); err != nil {
			return out, err
		}
		out = append(out, prop)
	}
	return out, nil
}

// preferredSkillName prefers reusing a matching inactive skill's name over
// minting a new recover-* name; active skills are never targeted (#410).
func (l *Learner) preferredSkillName(proposed string, p FailurePattern, c CandidateEdit) string {
	for _, s := range l.lastExistingSkills {
		if s.Status == "active" {
			continue
		}
		if skillsMatch(s, p, c) {
			return s.Name
		}
	}
	return proposed
}

// skillsMatch reports whether an existing skill is about the same mechanism
// as this candidate (shared significant words with the description or the
// pattern class).
func skillsMatch(s SkillSummary, p FailurePattern, c CandidateEdit) bool {
	hay := strings.ToLower(s.Description + " " + s.Name + " " + p.Class + " " + p.Attribution)
	words := significantWords(hay)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if strings.Contains(strings.ToLower(s.Description), w) || strings.Contains(strings.ToLower(s.Name), w) {
			return true
		}
	}
	return false
}

// significantWords extracts the meaningful lowercase tokens of a class label.
func significantWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(s)) {
		w = strings.Trim(w, ".,;:!?()[]{}'\"")
		if len(w) < 3 || stopWords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"for": true, "to": true, "in": true, "on": true, "with": true, "error": true,
	"failed": true, "fail": true, "is": true, "was": true, "are": true, "not": true,
	"when": true, "while": true, "during": true, "from": true, "at": true, "by": true,
	"tool": true, "recurring": true, "failure": true,
}

// existingSkillSummaries lists the workspace's skills with their activation
// status and content hash for the proposer and the cache key.
func (l *Learner) existingSkillSummaries() ([]SkillSummary, error) {
	if l.WS.Dir == "" {
		return nil, nil
	}
	skills, err := Discover(l.WS.SkillsDir())
	if err != nil {
		return nil, err
	}
	out := make([]SkillSummary, 0, len(skills))
	for _, s := range skills {
		raw, err := os.ReadFile(s.Path)
		if err != nil {
			continue
		}
		fields, _, err := spec.ParseFrontmatter(string(raw))
		if err != nil {
			continue
		}
		status := spec.StatusField(fields)
		if status == "" {
			status = "inactive"
		}
		h := sha256.New()
		_, _ = h.Write(raw)
		out = append(out, SkillSummary{
			Name:        s.Name,
			Status:      status,
			Description: s.Description,
			ContentHash: hex.EncodeToString(h.Sum(nil)),
		})
	}
	l.lastExistingSkills = out
	return out, nil
}

// relevantSkills returns the existing skills plausibly related to the pattern
// (bounded) so the prompt stays small and focused.
func relevantSkills(all []SkillSummary, p FailurePattern) []SkillSummary {
	var out []SkillSummary
	for _, s := range all {
		if strings.Contains(strings.ToLower(s.Description), strings.ToLower(p.Class)) ||
			strings.Contains(strings.ToLower(s.Name), strings.ToLower(p.Class)) {
			out = append(out, s)
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// priorAttempts loads previously stored proposals for the same pattern so
// later rounds avoid repeating the same content (#410).
func (l *Learner) priorAttempts(ctx context.Context, patternSig string) ([]AttemptSummary, error) {
	if l.DB == nil {
		return nil, nil
	}
	rows, err := l.DB.QueryContext(ctx, `
		SELECT id, status, payload FROM learn_proposals
		WHERE pattern_sig = ? AND status IN ('accepted', 'rejected')
		ORDER BY created_at`, patternSig)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AttemptSummary
	for rows.Next() {
		var a AttemptSummary
		var payload string
		if err := rows.Scan(&a.ID, &a.Status, &payload); err != nil {
			return nil, err
		}
		var body string
		_ = json.Unmarshal([]byte(payload), &struct {
			Body *string `json:"body"`
		}{Body: &body})
		if body != "" {
			h := sha256.New()
			_, _ = h.Write([]byte(body))
			a.ContentHash = hex.EncodeToString(h.Sum(nil))
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	l.lastPriorAttempts = out
	return out, nil
}

// proposalPrompt builds the bounded structured prompt for one pattern.
func proposalPrompt(req ProposalRequest) string {
	var b strings.Builder
	b.WriteString("You are improving an agent's reusable skills. For one recurring tool failure, produce up to ")
	fmt.Fprintf(&b, "%d materially distinct candidate edits (2-3 only when evidence supports alternatives).\n", req.MaxCandidates)
	b.WriteString("Each candidate: surface (one of " + strings.Join(req.AllowedSurfaces, "|") + "), name (lowercase-hyphen, only for skill), a one-line rationale naming the concrete mechanism to fix, and a body that is a mechanism-specific procedure (exact commands, paths, or tool semantics). Never write generic advice like \"reproduce, fix root cause, re-run\".\n\n")
	fmt.Fprintf(&b, "FAILURE SIGNATURE: %s\n", req.Pattern.Class)
	if req.Pattern.Attribution != "" {
		fmt.Fprintf(&b, "ATTRIBUTION: %s\n", req.Pattern.Attribution)
	}
	fmt.Fprintf(&b, "OCCURRENCES: %d\nEVIDENCE SESSIONS: %s\n", req.Pattern.Count, strings.Join(req.Pattern.SessionIDs, ", "))
	if len(req.Pattern.Samples) > 0 {
		b.WriteString("SAMPLES:\n")
		for _, s := range req.Pattern.Samples {
			fmt.Fprintf(&b, "- %s\n", textcut.Cut(s, 120))
		}
	}
	if len(req.ExistingSkills) > 0 {
		b.WriteString("\nEXISTING SKILLS (prefer updating an inactive matching one; never target active):\n")
		for _, s := range req.ExistingSkills {
			fmt.Fprintf(&b, "- %s [%s] %s\n", s.Name, s.Status, s.Description)
		}
	}
	if len(req.PriorAttempts) > 0 {
		b.WriteString("\nPRIOR ATTEMPTS (do not re-propose the same content):\n")
		for _, a := range req.PriorAttempts {
			fmt.Fprintf(&b, "- %s (%s)\n", a.ID, a.Status)
		}
	}
	b.WriteString("\nReply with only JSON: {\"candidates\":[{\"surface\":\"skill\",\"name\":\"...\",\"rationale\":\"...\",\"body\":\"...\"}]}\n")
	return b.String()
}

// fallbackPropose is the deterministic safe proposer used when no utility
// model is configured: mechanism-specific rules keyed on the signature. A
// pattern with no matching rule produces no candidate rather than boilerplate.
func fallbackPropose(req ProposalRequest) []CandidateEdit {
	if req.MaxCandidates <= 0 {
		req.MaxCandidates = defaultMaxCandidates
	}
	class := strings.ToLower(req.Pattern.Class)
	for _, rule := range mechanismRules {
		if rule.matches(class) {
			return []CandidateEdit{{
				Surface:   SurfaceSkill,
				Name:      "recover-" + rule.slug,
				Rationale: rule.rationale,
				Body:      rule.body(req.Pattern),
			}}
		}
	}
	return nil
}

type mechanismRule struct {
	keywords  []string
	slug      string
	rationale string
	steps     []string
}

func (r mechanismRule) matches(class string) bool {
	for _, k := range r.keywords {
		if strings.Contains(class, k) {
			return true
		}
	}
	return false
}

func (r mechanismRule) body(p FailurePattern) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Recover from: %s\n\nAttribution: %s\nSeen %d times.\nEvidence sessions: %s\n\n## Mechanism\n%s\n\n## Procedure\n",
		p.Class, p.Attribution, p.Count, strings.Join(p.SessionIDs, ", "), r.rationale)
	for i, s := range r.steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	b.WriteString("4. Re-run the original action and confirm the error is gone.\n")
	if len(p.Samples) > 0 {
		b.WriteString("\n## Observed errors\n")
		for _, s := range p.Samples {
			b.WriteString("- " + textcut.Cut(s, 120) + "\n")
		}
	}
	return b.String()
}

// mechanismRules is the deterministic fallback table: each rule targets a
// concrete fix mechanism, never boilerplate advice.
var mechanismRules = []mechanismRule{{
	keywords:  []string{"permission denied", "access denied", "permission"},
	slug:      "permissions",
	rationale: "Ownership or mode is blocking the write/execution.",
	steps: []string{
		"Identify the failing path: run `ls -la` on the parent directory to see owner, group, and mode.",
		"Grant the needed access with `chmod` (e.g. `chmod u+w <path>`) or `chown` when ownership is wrong.",
		"Confirm the change took effect with `ls -la` before re-running.",
	},
},
	{
		keywords:  []string{"no such file or directory", "not found", "does not exist"},
		slug:      "missing-path",
		rationale: "A required path does not exist or points to the wrong place.",
		steps: []string{
			"Verify the exact path with `ls -la` or `test -e <path>`; check for typos and relative-vs-absolute mismatches.",
			"Create missing parent directories with `mkdir -p <dir>` or generate the file from its source.",
			"Re-check the path resolves before re-running.",
		},
	},
	{
		keywords:  []string{"command not found", "executable not found", "unknown command"},
		slug:      "missing-tool",
		rationale: "A required binary is not installed or not on PATH.",
		steps: []string{
			"Locate the binary with `which <tool>` (or `command -v <tool>`); check the shell PATH is complete.",
			"Install the tool through the environment's package manager (`apt`, `brew`, `pip`, `npm`) pinned to a known version.",
			"Confirm `which <tool>` resolves in the same environment that runs the agent before re-running.",
		},
	},
	{
		keywords:  []string{"connection refused", "connection timed out", "timeout", "connection failed", "could not connect"},
		slug:      "service-unreachable",
		rationale: "A remote service is down, misconfigured, or unreachable from this network.",
		steps: []string{
			"Check the service endpoint: `curl -v` the URL and verify DNS resolves and the port accepts connections.",
			"Restart the service or wait for it to become healthy; verify readiness with a health endpoint.",
			"Retry with a bounded backoff and confirm a successful round-trip before re-running.",
		},
	},
	{
		keywords:  []string{"already exists", "already in use", "conflict", "duplicate"},
		slug:      "idempotency",
		rationale: "The operation collides with existing state; it must be check-then-create.",
		steps: []string{
			"Inspect existing state (`ls`, `git status`, registry list) to find the colliding resource.",
			"Make the step idempotent: skip when present, or update in place rather than recreating.",
			"Re-run and confirm the second run is a no-op or applies cleanly.",
		},
	},
	{
		keywords:  []string{"parse", "invalid syntax", "unmarshal", "invalid json", "unexpected token"},
		slug:      "malformed-input",
		rationale: "Input does not match the expected schema or format.",
		steps: []string{
			"Validate the input against the documented schema (`--dry-run`, parser, or lint tool).",
			"Correct the malformed field: quoting, types, required keys, or trailing separators.",
			"Re-run and confirm the payload parses cleanly.",
		},
	},
	{
		keywords:  []string{"missing dependency", "not installed", "module not found", "cannot find module", "no module named"},
		slug:      "missing-dependency",
		rationale: "A library or module dependency is absent or the wrong version.",
		steps: []string{
			"List the declared dependencies and compare with what is installed (`go list -m all`, `pip list`, `npm ls`).",
			"Install the missing dependency at a pinned version, or fix the declared version range.",
			"Verify the module resolves before re-running.",
		},
	},
	{
		keywords:  []string{"out of memory", "oom", "memory limit", "killed"},
		slug:      "resource-limit",
		rationale: "The operation exhausted a resource cap (memory, fds, disk).",
		steps: []string{
			"Check the limit: `ulimit -a`, disk free with `df -h`, and the process's peak usage.",
			"Raise the relevant limit or reduce the workload (batch size, concurrency, retention).",
			"Re-run and confirm it completes within the new budget.",
		},
	},
}
