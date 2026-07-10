// Package observability tracks active and recently completed agent runs.
package observability

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

// Service records active runs in memory and completed runs in SQLite.
type Service struct {
	store *store.Store
	now   func() time.Time

	mu     sync.Mutex
	active map[string]*activeRun
}

type activeRun struct {
	id, sessionID, source, phase string
	startedAt                    time.Time
	inputTokens, outputTokens    int
	lastUsage                    llm.Usage
}

// Snapshot is the complete local status representation.
type Snapshot struct {
	Active     []ActiveRun `json:"active"`
	Recent     []RecentRun `json:"recent"`
	RetryQueue []any       `json:"retry_queue"`
}

// ActiveRun is an in-progress run and its metrics at snapshot time.
type ActiveRun struct {
	ID           string `json:"id"`
	SessionID    string `json:"session_id"`
	Source       string `json:"source"`
	Phase        string `json:"phase"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// RecentRun is a completed run retained in SQLite.
type RecentRun struct {
	ID           string `json:"id"`
	SessionID    string `json:"session_id"`
	Source       string `json:"source"`
	Phase        string `json:"phase"`
	Outcome      string `json:"outcome"`
	RuntimeMS    int64  `json:"runtime_ms"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// New creates a run metrics service. The clock is injected to make elapsed
// times deterministic in tests; a nil clock uses the current time.
func New(st *store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: st, now: now, active: make(map[string]*activeRun)}
}

// Start registers a new active run.
func (s *Service) Start(_ context.Context, id, sessionID, source, phase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.active[id]; exists {
		return fmt.Errorf("run %q is already active", id)
	}
	s.active[id] = &activeRun{
		id: id, sessionID: sessionID, source: source, phase: phase, startedAt: s.now(),
	}
	return nil
}

// RecordUsage adds the positive delta from a provider-neutral cumulative
// observation. Repeated cumulative observations therefore add no tokens.
func (s *Service) RecordUsage(_ context.Context, id string, usage llm.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, exists := s.active[id]
	if !exists {
		return fmt.Errorf("run %q is not active", id)
	}
	run.inputTokens += max(0, usage.InputTokens-run.lastUsage.InputTokens)
	run.outputTokens += max(0, usage.OutputTokens-run.lastUsage.OutputTokens)
	run.lastUsage = usage
	return nil
}

// Finish persists an active run's final metrics and removes it from memory.
func (s *Service) Finish(ctx context.Context, id, outcome string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, exists := s.active[id]
	if !exists {
		return fmt.Errorf("run %q is not active", id)
	}
	endedAt := s.now()
	if _, err := s.store.DB.ExecContext(ctx, `
		INSERT INTO run_metrics
			(id, session_id, source, phase, outcome, started_at_ms, ended_at_ms, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.id, run.sessionID, run.source, run.phase, outcome,
		run.startedAt.UnixMilli(), endedAt.UnixMilli(), run.inputTokens, run.outputTokens); err != nil {
		return fmt.Errorf("persist run %q: %w", id, err)
	}
	delete(s.active, id)
	return nil
}

// Snapshot returns active runs, recent completed runs, and the stable empty
// retry queue placeholder.
func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	now := s.now()
	s.mu.Lock()
	active := make([]ActiveRun, 0, len(s.active))
	for _, run := range s.active {
		active = append(active, ActiveRun{
			ID: run.id, SessionID: run.sessionID, Source: run.source, Phase: run.phase,
			ElapsedMS:   now.Sub(run.startedAt).Milliseconds(),
			InputTokens: run.inputTokens, OutputTokens: run.outputTokens,
		})
	}
	s.mu.Unlock()
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT id, session_id, source, phase, outcome,
			ended_at_ms - started_at_ms, input_tokens, output_tokens
		FROM run_metrics
		ORDER BY ended_at_ms DESC, id ASC
		LIMIT 20`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read recent runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only result
	recent := make([]RecentRun, 0)
	for rows.Next() {
		var run RecentRun
		if err := rows.Scan(&run.ID, &run.SessionID, &run.Source, &run.Phase, &run.Outcome,
			&run.RuntimeMS, &run.InputTokens, &run.OutputTokens); err != nil {
			return Snapshot{}, fmt.Errorf("scan recent run: %w", err)
		}
		recent = append(recent, run)
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("read recent runs: %w", err)
	}
	return Snapshot{Active: active, Recent: recent, RetryQueue: make([]any, 0)}, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
