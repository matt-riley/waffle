package workset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
)

// UpdateTool mutates the session working set (source=model when the agent
// calls it). Restricted groups deny this tool via AgentPolicy (#67/#68).
type UpdateTool struct {
	Store *Store
}

func (t UpdateTool) Def() llm.Tool {
	return llm.Tool{
		Name: "workspace_update",
		Description: "Mutate this session's working set (goals, constraints, decisions, facts, open questions, assumptions). " +
			"Pinned entries survive summarization. Use for transient task state, not durable MEMORY.md notes.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"op": {"type": "string", "description": "add | replace | drop | list | clear_assumptions"},
				"kind": {"type": "string", "description": "goal|constraint|decision|fact|open_question|assumption (for add)"},
				"body": {"type": "string", "description": "entry body (add/replace)"},
				"id": {"type": "string", "description": "entry id (replace/drop)"},
				"pinned": {"type": "boolean", "description": "pin entry (add only)"}
			},
			"required": ["op"]
		}`),
	}
}

func (t UpdateTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Store == nil {
		return "", fmt.Errorf("working set store not configured")
	}
	sid := session.IDFromContext(ctx)
	if sid == "" {
		return "", fmt.Errorf("no active session")
	}
	var in struct {
		Op     string `json:"op"`
		Kind   string `json:"kind"`
		Body   string `json:"body"`
		ID     string `json:"id"`
		Pinned bool   `json:"pinned"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	in.Op = strings.TrimSpace(strings.ToLower(in.Op))
	switch in.Op {
	case "list":
		entries, err := t.Store.List(ctx, sid)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return "working set is empty", nil
		}
		return strings.TrimSpace(Render(entries)), nil
	case "add":
		e, err := t.Store.Add(ctx, sid, in.Kind, in.Body, SourceModel, in.Pinned)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("added %s id=%s", e.Kind, e.ID), nil
	case "replace":
		e, err := t.Store.Replace(ctx, sid, in.ID, in.Body, SourceModel)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("replaced id=%s", e.ID), nil
	case "drop":
		if err := t.Store.Drop(ctx, sid, in.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("dropped id=%s", in.ID), nil
	case "clear_assumptions":
		n, err := t.Store.DropStaleAssumptions(ctx, sid, 0, false)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("dropped %d unpinned model assumptions", n), nil
	default:
		return "", fmt.Errorf("unknown op %q (want add|replace|drop|list|clear_assumptions)", in.Op)
	}
}

// DropStaleAssumptions removes unpinned model-sourced assumptions older than
// maxAge (zero = all ages). When pinnedOnly is true, only unpinned entries
// matching the filter are removed (always the case for assumptions cleanup).
func (s *Store) DropStaleAssumptions(ctx context.Context, sessionID string, maxAge time.Duration, includePinned bool) (int, error) {
	entries, err := s.List(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	cutoff := time.Time{}
	if maxAge > 0 {
		cutoff = time.Now().UTC().Add(-maxAge)
	}
	n := 0
	for _, e := range entries {
		if e.Kind != KindAssumption {
			continue
		}
		if e.Source != SourceModel {
			continue
		}
		if e.Pinned && !includePinned {
			continue
		}
		if !cutoff.IsZero() && e.UpdatedAt.After(cutoff) {
			continue
		}
		if err := s.Drop(ctx, sessionID, e.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// DropUnpinnedModelAssumptions clears model assumptions that are not pinned
// (used on /reset).
func (s *Store) DropUnpinnedModelAssumptions(ctx context.Context, sessionID string) (int, error) {
	return s.DropStaleAssumptions(ctx, sessionID, 0, false)
}

// DropStaleAssumptionsAll removes unpinned model assumptions across all sessions (#70).
func (s *Store) DropStaleAssumptionsAll(ctx context.Context, maxAge time.Duration) (int, error) {
	if s == nil || s.DB == nil {
		return 0, nil
	}
	if maxAge <= 0 {
		res, err := s.DB.ExecContext(ctx, `
			DELETE FROM working_set_entries
			WHERE source = ? AND kind = ? AND pinned = 0`, SourceModel, KindAssumption)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	cut := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	res, err := s.DB.ExecContext(ctx, `
		DELETE FROM working_set_entries
		WHERE source = ? AND kind = ? AND pinned = 0 AND updated_at < ?`,
		SourceModel, KindAssumption, cut)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
