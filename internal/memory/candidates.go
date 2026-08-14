// CandidateService is the host-authoritative boundary for the pending memory
// and skill candidate queue (#416). It is shared by the `waffle candidates`
// CLI and any Desk route, and it is the only production path that lists,
// inspects, approves, or denies pending candidates. Decisions are atomic and
// serialized by the Gate mutex, and each decision verifies the pending file
// still matches the digest the operator inspected.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CandidateSummary is the list view of a candidate: enough to triage without
// loading full bodies into the terminal. Provenance, trust class, evidence
// ids, created time, and status are all exposed; the full diff and body come
// from Get.
type CandidateSummary struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name,omitempty"`
	Status     string     `json:"status"`
	Preview    string     `json:"preview"`
	Diff       string     `json:"diff,omitempty"`
	Provenance Provenance `json:"provenance"`
	CreatedAt  time.Time  `json:"created_at"`
	ReviewHint string     `json:"review_hint"`
	TargetID   string     `json:"target_id,omitempty"`
	Action     string     `json:"action,omitempty"`
}

// summary renders the list view of a candidate without executing content.
func (c Candidate) summary() CandidateSummary {
	preview := OneLine(c.Body)
	if len(preview) > 120 {
		preview = preview[:117] + "…"
	}
	hint := c.ReviewHint
	if hint == "" {
		hint = "waffle candidates show " + c.ID
	}
	return CandidateSummary{
		ID:         c.ID,
		Kind:       c.Kind,
		Name:       c.Name,
		Status:     c.Status,
		Preview:    preview,
		Diff:       c.Diff,
		Provenance: c.Provenance,
		CreatedAt:  c.CreatedAt,
		ReviewHint: hint,
		TargetID:   c.TargetID,
		Action:     c.Action,
	}
}

// Inspection is one candidate plus the sha256 of its pending file at read
// time; approve/deny require this digest to confirm the payload the operator
// reviewed is the payload being decided (#416).
type Inspection struct {
	Candidate  Candidate `json:"candidate"`
	FileDigest string    `json:"file_digest"`
}

// CandidateService operates the pending queue through a Gate.
type CandidateService struct {
	Gate *Gate
}

// List returns candidates (optionally filtered by status) plus a list of
// individual corrupt-file reports. A corrupt file is reported by name and
// skipped; it never hides the other candidates (#416).
func (s *CandidateService) List(ctx context.Context, status string) ([]CandidateSummary, []string, error) {
	if s == nil || s.Gate == nil {
		return nil, nil, errors.New("candidate service is not configured")
	}
	dir := filepath.Join(s.Gate.WS.Dir, "pending")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	status = strings.TrimSpace(strings.ToLower(status))
	var out []CandidateSummary
	var corrupt []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			corrupt = append(corrupt, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		var c Candidate
		if err := json.Unmarshal(b, &c); err != nil {
			corrupt = append(corrupt, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		out = append(out, c.summary())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, corrupt, nil
}

// Get loads one candidate with the digest of its pending file at read time.
func (s *CandidateService) Get(ctx context.Context, id string) (Inspection, error) {
	if s == nil || s.Gate == nil {
		return Inspection{}, errors.New("candidate service is not configured")
	}
	b, err := os.ReadFile(s.Gate.pendingPath(id))
	if err != nil {
		return Inspection{}, err
	}
	var c Candidate
	if err := json.Unmarshal(b, &c); err != nil {
		return Inspection{}, fmt.Errorf("corrupt candidate file %s: %w", id, err)
	}
	return Inspection{Candidate: c, FileDigest: fileDigestOf(b)}, nil
}

// Approve applies exactly the reviewed candidate. fileDigest is the digest
// from Get; an empty digest skips the check (direct Gate callers).
func (s *CandidateService) Approve(ctx context.Context, id, approver, fileDigest string) (Candidate, error) {
	if s == nil || s.Gate == nil {
		return Candidate{}, errors.New("candidate service is not configured")
	}
	if id == "" {
		return Candidate{}, errors.New("candidate id is required")
	}
	return s.Gate.ApproveWithDigest(id, approver, fileDigest)
}

// Deny records a decision and never mutates live memory or skills. fileDigest
// is the digest from Get; an empty digest skips the check.
func (s *CandidateService) Deny(ctx context.Context, id, approver, reason, fileDigest string) (Candidate, error) {
	if s == nil || s.Gate == nil {
		return Candidate{}, errors.New("candidate service is not configured")
	}
	if id == "" {
		return Candidate{}, errors.New("candidate id is required")
	}
	return s.Gate.DenyWithDigest(id, approver, reason, fileDigest)
}
