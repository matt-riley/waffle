package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/notify"
	"github.com/matt-riley/waffle/internal/session"
)

// Provenance records the evidence and authority behind a durable write.
type Provenance struct {
	SourceKind       string   `json:"source_kind"` // owner_turn, model_inference, reflection, subagent, import
	SourceID         string   `json:"source_id"`
	TrustClass       string   `json:"trust_class"` // owner_stated, model_derived, untrusted_derived
	UntrustedContext bool     `json:"untrusted_context"`
	SessionID        string   `json:"session_id,omitempty"`
	Channel          string   `json:"channel,omitempty"`
	EvidenceIDs      []string `json:"evidence_ids,omitempty"`
	AgentGroup       string   `json:"agent_group,omitempty"`
}

type Candidate struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"` // memory or skill
	Name        string     `json:"name,omitempty"`
	Description string     `json:"description,omitempty"`
	Body        string     `json:"body"`
	Diff        string     `json:"diff,omitempty"`
	Provenance  Provenance `json:"provenance"`
	Status      string     `json:"status"` // pending, applied, denied
	CreatedAt   time.Time  `json:"created_at"`
	ApprovedBy  string     `json:"approved_by,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	// TargetID, Action, Digest, and Current describe a memory_update candidate
	// (#417): supersede/forget mutations of an existing note. TargetID is the
	// note being mutated, Digest is the digest of its text at proposal time so
	// approval can compare-and-swap, and Current is the live text for review.
	// Action is one of "append", "supersede", or "forget"; "" means append.
	TargetID string `json:"target_id,omitempty"`
	Action   string `json:"action,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Current  string `json:"current,omitempty"`
	// Denial audit (#416): who denied, when, and why. Never mutates live state.
	DeniedBy   string     `json:"denied_by,omitempty"`
	DeniedAt   *time.Time `json:"denied_at,omitempty"`
	DenyReason string     `json:"deny_reason,omitempty"`
}

// Gate is the common write gate for memory and skill candidates.
type Gate struct {
	Mode   string
	WS     Workspace
	Notify func(Candidate)
	mu     sync.Mutex
}

func provenanceFromContext(ctx context.Context, p Provenance) Provenance {
	o := session.OriginFromContext(ctx)
	if p.SessionID == "" {
		p.SessionID = o.SessionID
	}
	if p.Channel == "" {
		p.Channel = o.Channel
	}
	p.UntrustedContext = p.UntrustedContext || o.Untrusted
	if p.SourceKind == "" {
		p.SourceKind = "model_inference"
	}
	if p.TrustClass == "" {
		p.TrustClass = "model_derived"
	}
	if p.UntrustedContext {
		p.TrustClass = "untrusted_derived"
	}
	return p
}

func (g *Gate) pendingPath(id string) string {
	return filepath.Join(g.WS.Dir, "pending", id+".json")
}

func (g *Gate) decide(c Candidate) (Candidate, error) {
	if c.ID == "" {
		c.ID = fmt.Sprintf("candidate-%d", time.Now().UnixNano())
	}
	c.CreatedAt = time.Now().UTC()
	if c.Provenance.TrustClass == "" {
		c.Provenance.TrustClass = "owner_stated"
	}
	// Untrusted evidence and instruction-shaped skills always require review
	// unless the owner explicitly approves them.
	pending := g.Mode == "review" || c.Provenance.UntrustedContext ||
		c.Provenance.TrustClass == "untrusted_derived"
	if pending {
		c.Status = "pending"
		if err := os.MkdirAll(filepath.Dir(g.pendingPath(c.ID)), 0o700); err != nil {
			return c, err
		}
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return c, err
		}
		if err := os.WriteFile(g.pendingPath(c.ID), b, 0o600); err != nil {
			return c, err
		}
		return c, nil
	}
	c.Status = "applied"
	return c, nil
}

func (g *Gate) submit(ctx context.Context, c Candidate, apply func() error) (Candidate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, err := g.decide(c)
	if err != nil || c.Status == "pending" {
		return c, err
	}
	if err := apply(); err != nil {
		return c, err
	}
	// Update candidates carry a human-readable diff built at proposal time
	// (#417); only synthesize one for plain appends and skills.
	if c.Diff == "" {
		if c.Kind == "skill" {
			c.Diff = "+ skill " + c.Name + "\n+ " + c.Body
		} else {
			c.Diff = "+ " + c.Body
		}
	}
	if g.Mode == "notify" && g.Notify != nil {
		g.Notify(c)
	} else if g.Mode == "notify" {
		// Deliver through the generalized session-scoped sender (#253): the
		// gateway attaches one sender per run that the memory gate and the
		// notify tool both reach. A run with no channel origin has no sender;
		// the write still applies silently (terminal chat / eval).
		if sender, ok := notify.SenderFromContext(ctx); ok {
			if err := sender(ctx, fmt.Sprintf("%s change:\n%s", c.Kind, c.Diff)); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}

// Pending lists candidates awaiting owner approval.
func (g *Gate) Pending() ([]Candidate, error) {
	entries, err := os.ReadDir(filepath.Join(g.WS.Dir, "pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(g.WS.Dir, "pending", e.Name()))
		if err != nil {
			return nil, err
		}
		var c Candidate
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
		if c.Status == "pending" {
			out = append(out, c)
		}
	}
	return out, nil
}

// SubmitForReview forces a candidate into pending without applying it.
// Used by weakness mining and other offline proposers (#65).
func (g *Gate) SubmitForReview(c Candidate) (Candidate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c.ID == "" {
		c.ID = fmt.Sprintf("candidate-%d", time.Now().UnixNano())
	}
	c.CreatedAt = time.Now().UTC()
	c.Status = "pending"
	if c.Provenance.TrustClass == "" {
		c.Provenance.TrustClass = "untrusted_derived"
	}
	if err := os.MkdirAll(filepath.Dir(g.pendingPath(c.ID)), 0o700); err != nil {
		return c, err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return c, err
	}
	if err := os.WriteFile(g.pendingPath(c.ID), b, 0o600); err != nil {
		return c, err
	}
	return c, nil
}

// Approve applies one pending candidate. approver is recorded for audit.
// Memory update candidates (supersede/forget, #417) are applied through the
// shared lock with a compare-and-swap on the target digest: if the target
// note changed after proposal, approval fails stale and never mutates the
// newer note.
func (g *Gate) Approve(id, approver string) (Candidate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, err := os.ReadFile(g.pendingPath(id))
	if err != nil {
		return Candidate{}, err
	}
	var c Candidate
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Status != "pending" {
		return c, errors.New("candidate is not pending")
	}
	if c.Kind == "memory" {
		switch c.Action {
		case "forget":
			if err := g.WS.ForgetNoteCAS(c.TargetID, c.Digest); err != nil {
				return c, err
			}
		case "supersede":
			if _, err := g.WS.SupersedeNoteCAS(c.TargetID, c.Body, c.Provenance, c.Digest); err != nil {
				return c, err
			}
		default:
			if _, err := g.WS.appendCandidate(c); err != nil {
				return c, err
			}
		}
	} else if err := g.WS.writeSkillCandidate(c); err != nil {
		return c, err
	}
	now := time.Now().UTC()
	c.Status, c.ApprovedBy, c.ApprovedAt = "applied", approver, &now
	b, _ = json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(g.pendingPath(id), b, 0o600); err != nil {
		return c, err
	}
	return c, nil
}

// Deny records a decision without ever mutating live memory or skills. The
// pending file keeps the full candidate plus approver/time/reason so the
// queue maintains a durable audit trail (#416).
func (g *Gate) Deny(id, approver, reason string) (Candidate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, err := os.ReadFile(g.pendingPath(id))
	if err != nil {
		return Candidate{}, err
	}
	var c Candidate
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Status != "pending" {
		return c, errors.New("candidate is not pending")
	}
	now := time.Now().UTC()
	c.Status, c.DeniedBy, c.DeniedAt, c.DenyReason = "denied", approver, &now, reason
	b, _ = json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(g.pendingPath(id), b, 0o600); err != nil {
		return c, err
	}
	return c, nil
}
