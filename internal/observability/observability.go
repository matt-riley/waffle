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

	mu            sync.Mutex
	active        map[string]*activeRun
	adapterLast   map[string]time.Time
	schedulerLast time.Time
}

type activeRun struct {
	id, sessionID, source, phase, profile string
	startedAt                             time.Time
	inputTokens, outputTokens             int
	lastUsage                             llm.Usage
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
	Profile      string `json:"profile,omitempty"`
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
	Profile      string `json:"profile,omitempty"`
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
	return &Service{store: st, now: now, active: make(map[string]*activeRun), adapterLast: make(map[string]time.Time)}
}

// MarkAdapter records a successful adapter event. Adapter implementations can
// call this on each successful poll or delivered inbound message.
func (s *Service) MarkAdapter(name string) { s.mu.Lock(); s.adapterLast[name] = s.now(); s.mu.Unlock() }

// RegisterAdapter adds an adapter to the liveness rollup before its first
// successful event, so a never-started adapter is not reported healthy.
func (s *Service) RegisterAdapter(name string) {
	s.mu.Lock()
	if _, ok := s.adapterLast[name]; !ok {
		s.adapterLast[name] = time.Time{}
	}
	s.mu.Unlock()
}
func (s *Service) MarkSchedulerTick() { s.mu.Lock(); s.schedulerLast = s.now(); s.mu.Unlock() }

type Health struct {
	Healthy   bool                     `json:"healthy"`
	Adapters  map[string]AdapterHealth `json:"adapters"`
	Scheduler SchedulerHealth          `json:"scheduler"`
	Database  bool                     `json:"database"`
}
type AdapterHealth struct {
	LastSuccess string `json:"last_success"`
	Stale       bool   `json:"stale"`
}
type SchedulerHealth struct {
	LastTick string `json:"last_tick"`
	Stale    bool   `json:"stale"`
}

// HealthSnapshot returns cheap liveness state. The database probe is run on
// every request so a wedged store returns 503 rather than stale green JSON.
func (s *Service) HealthSnapshot(ctx context.Context, staleAfter time.Duration) (Health, error) {
	now := s.now()
	s.mu.Lock()
	adapters := make(map[string]AdapterHealth, len(s.adapterLast))
	healthy := true
	for name, last := range s.adapterLast {
		h := AdapterHealth{LastSuccess: last.UTC().Format(time.RFC3339Nano), Stale: now.Sub(last) > staleAfter}
		adapters[name] = h
		healthy = healthy && !h.Stale
	}
	lastTick := s.schedulerLast
	s.mu.Unlock()
	if lastTick.IsZero() || now.Sub(lastTick) > staleAfter {
		healthy = false
	}
	dbOK := true
	if s.store == nil || s.store.DB.QueryRowContext(ctx, `SELECT 1`).Scan(new(int)) != nil {
		dbOK = false
		healthy = false
	}
	return Health{Healthy: healthy && dbOK, Adapters: adapters, Scheduler: SchedulerHealth{LastTick: lastTick.UTC().Format(time.RFC3339Nano), Stale: lastTick.IsZero() || now.Sub(lastTick) > staleAfter}, Database: dbOK}, nil
}

// Start registers a new active run. profile is the named agent profile (#71);
// empty means the default (main) posture.
func (s *Service) Start(_ context.Context, id, sessionID, source, phase, profile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.active[id]; exists {
		return fmt.Errorf("run %q is already active", id)
	}
	s.active[id] = &activeRun{
		id: id, sessionID: sessionID, source: source, phase: phase, profile: profile, startedAt: s.now(),
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
// When the store is nil, the run is still removed from the active map but
// metrics are not persisted (graceful degrade, matching HealthSnapshot).
func (s *Service) Finish(ctx context.Context, id, outcome string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, exists := s.active[id]
	if !exists {
		return fmt.Errorf("run %q is not active", id)
	}
	if s.store == nil {
		delete(s.active, id)
		return nil
	}
	endedAt := s.now()
	if _, err := s.store.DB.ExecContext(ctx, `
		INSERT INTO run_metrics
			(id, session_id, source, phase, outcome, started_at_ms, ended_at_ms, input_tokens, output_tokens, profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.id, run.sessionID, run.source, run.phase, outcome,
		run.startedAt.UnixMilli(), endedAt.UnixMilli(), run.inputTokens, run.outputTokens, run.profile); err != nil {
		return fmt.Errorf("persist run %q: %w", id, err)
	}
	delete(s.active, id)
	return nil
}

// Snapshot returns active runs, recent completed runs, and the stable empty
// retry queue placeholder. When the store is nil, recent runs are empty
// (graceful degrade, matching HealthSnapshot).
func (s *Service) Snapshot(ctx context.Context) (snap Snapshot, err error) {
	now := s.now()
	s.mu.Lock()
	active := make([]ActiveRun, 0, len(s.active))
	for _, run := range s.active {
		active = append(active, ActiveRun{
			ID: run.id, SessionID: run.sessionID, Source: run.source, Phase: run.phase, Profile: run.profile,
			ElapsedMS:   now.Sub(run.startedAt).Milliseconds(),
			InputTokens: run.inputTokens, OutputTokens: run.outputTokens,
		})
	}
	s.mu.Unlock()
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	if s.store == nil {
		return Snapshot{Active: active, Recent: make([]RecentRun, 0), RetryQueue: make([]any, 0)}, nil
	}

	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT id, session_id, source, phase, outcome,
			ended_at_ms - started_at_ms, input_tokens, output_tokens, profile
		FROM run_metrics
		ORDER BY ended_at_ms DESC, id ASC
		LIMIT 20`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read recent runs: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	recent := make([]RecentRun, 0)
	for rows.Next() {
		var run RecentRun
		if err := rows.Scan(&run.ID, &run.SessionID, &run.Source, &run.Phase, &run.Outcome,
			&run.RuntimeMS, &run.InputTokens, &run.OutputTokens, &run.Profile); err != nil {
			return Snapshot{}, fmt.Errorf("scan recent run: %w", err)
		}
		recent = append(recent, run)
	}
	if err = rows.Err(); err != nil {
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
