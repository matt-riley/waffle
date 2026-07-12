// Package schedule implements waffle's cron scheduler (docs/plan.md,
// "Scheduling"): a job is a cron expression + a prompt + a delivery
// target, persisted in SQLite. Each firing runs as a normal session and
// delivers its result to a channel (or the log).
package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/usage"
)

// Job is one scheduled task.
type Job struct {
	ID      string
	Name    string
	Cron    string
	Prompt  string
	Deliver string // "channel:chat_id" or "" for log-only
	// Profile is an optional named agent profile (#71). Empty uses the cron tier default.
	Profile      string
	Enabled      bool
	LastRun      time.Time
	LastStatus   string
	CreatedAt    time.Time
	Attempt      int
	NextRetry    time.Time
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	StallTimeout time.Duration
}

type RetryPolicy struct {
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	StallTimeout time.Duration
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 1, BaseBackoff: 10 * time.Second, MaxBackoff: 10 * time.Minute, StallTimeout: 5 * time.Minute}
}

func (p RetryPolicy) normalized() RetryPolicy {
	d := DefaultRetryPolicy()
	if p.MaxAttempts > 0 {
		d.MaxAttempts = p.MaxAttempts
	}
	if p.BaseBackoff > 0 {
		d.BaseBackoff = p.BaseBackoff
	}
	if p.MaxBackoff > 0 {
		d.MaxBackoff = p.MaxBackoff
	}
	if p.StallTimeout > 0 {
		d.StallTimeout = p.StallTimeout
	}
	return d
}

// Deliverer sends a job's result somewhere (a channel adapter, typically).
type Deliverer interface {
	Deliver(ctx context.Context, target, text string) error
}

// Store persists jobs.
type Store struct{ db *sql.DB }

// NewStore wraps an opened waffle store.
func NewStore(st *store.Store) *Store { return &Store{db: st.DB} }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// parser accepts standard 5-field cron expressions.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Add validates the cron expression and stores a job.
func (s *Store) Add(ctx context.Context, name, spec, prompt, deliver string) (*Job, error) {
	return s.AddWithProfile(ctx, name, spec, prompt, deliver, "")
}

// AddWithProfile is Add with an optional named agent profile (#71).
func (s *Store) AddWithProfile(ctx context.Context, name, spec, prompt, deliver, profile string) (*Job, error) {
	if _, err := parser.Parse(spec); err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", spec, err)
	}
	jobID, err := id.New("job-")
	if err != nil {
		return nil, fmt.Errorf("new job id: %w", err)
	}
	j := &Job{ID: jobID, Name: name, Cron: spec, Prompt: prompt, Deliver: deliver, Profile: profile, Enabled: true}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, name, cron, prompt, deliver, enabled, created_at, max_attempts, base_backoff, max_backoff, stall_timeout, profile)
		VALUES (?, ?, ?, ?, ?, 1, ?, 1, '10s', '10m', '5m', ?)`, j.ID, name, spec, prompt, deliver, now(), profile)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// Remove deletes a job.
func (s *Store) Remove(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no job %q", id)
	}
	return nil
}

// List returns all jobs.
func (s *Store) List(ctx context.Context) (out []Job, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cron, prompt, deliver, enabled, last_run, last_status, created_at, attempt, next_retry, max_attempts, base_backoff, max_backoff, stall_timeout, profile
		FROM jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Get loads one job.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `
		SELECT id, name, cron, prompt, deliver, enabled, last_run, last_status, created_at, attempt, next_retry, max_attempts, base_backoff, max_backoff, stall_timeout, profile
		FROM jobs WHERE id = ?`, id))
}

func (s *Store) markRun(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET last_run = ?, last_status = ? WHERE id = ?`, now(), status, id)
	return err
}

func (s *Store) startAttempt(ctx context.Context, id string, attempt int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET attempt = ?, next_retry = '' WHERE id = ?`, attempt, id)
	return err
}

func (s *Store) scheduleRetry(ctx context.Context, id, status string, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET last_run = ?, last_status = ?, next_retry = ? WHERE id = ?`,
		now(), status, next.UTC().Format(time.RFC3339Nano), id)
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	var enabled int
	var lastRun, created, nextRetry, base, max, stall string
	err := row.Scan(&j.ID, &j.Name, &j.Cron, &j.Prompt, &j.Deliver, &enabled, &lastRun, &j.LastStatus, &created, &j.Attempt, &nextRetry, &j.MaxAttempts, &base, &max, &stall, &j.Profile)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("job not found")
	}
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	var parseErr error
	if lastRun != "" {
		if j.LastRun, parseErr = time.Parse(time.RFC3339Nano, lastRun); parseErr != nil {
			return nil, fmt.Errorf("parse job last_run: %w", parseErr)
		}
	}
	if j.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, created); parseErr != nil && created != "" {
		return nil, fmt.Errorf("parse job created_at: %w", parseErr)
	}
	if nextRetry != "" {
		if j.NextRetry, parseErr = time.Parse(time.RFC3339Nano, nextRetry); parseErr != nil {
			return nil, fmt.Errorf("parse job next_retry: %w", parseErr)
		}
	}
	j.BaseBackoff, parseErr = time.ParseDuration(base)
	if parseErr != nil {
		return nil, fmt.Errorf("parse job base_backoff: %w", parseErr)
	}
	j.MaxBackoff, parseErr = time.ParseDuration(max)
	if parseErr != nil {
		return nil, fmt.Errorf("parse job max_backoff: %w", parseErr)
	}
	j.StallTimeout, parseErr = time.ParseDuration(stall)
	if parseErr != nil {
		return nil, fmt.Errorf("parse job stall_timeout: %w", parseErr)
	}
	return &j, nil
}

// Runner executes a job's prompt through a fresh session and delivers the
// result. It is provided by the caller (serve wires the real agent).
type Runner struct {
	Agent     *agent.Agent
	Sessions  *session.Store
	Deliverer Deliverer
	Log       *slog.Logger

	// AgentsByProfile maps named agent profiles to pre-built agents (#71).
	// When a job sets Profile and this map is non-nil, the matching agent is
	// used; an unknown profile errors rather than falling back to Agent.
	// When the map is nil, Profile is ignored and Agent is always used
	// (tests and partial wiring).
	AgentsByProfile map[string]*agent.Agent

	// Observability records cron agent runs when configured.
	Observability *observability.Service
}

// Run executes one job now: fresh session, agent turn, deliver the reply.
func (r *Runner) Run(ctx context.Context, j Job) (string, error) {
	return r.RunAttempt(ctx, j, 1)
}

// RunAttempt executes one numbered attempt. Retries add context to the
// prompt while the first attempt remains byte-for-byte compatible.
func (r *Runner) RunAttempt(ctx context.Context, j Job, attempt int) (string, error) {
	a := r.Agent
	if j.Profile != "" {
		if r.AgentsByProfile != nil {
			if p, ok := r.AgentsByProfile[j.Profile]; ok && p != nil {
				a = p
			} else {
				return "", fmt.Errorf("cron: unknown profile %q", j.Profile)
			}
		}
	}
	if a == nil {
		return "", fmt.Errorf("cron: no agent configured")
	}

	policy := RetryPolicy{MaxAttempts: j.MaxAttempts, BaseBackoff: j.BaseBackoff, MaxBackoff: j.MaxBackoff, StallTimeout: j.StallTimeout}.normalized()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activity := make(chan struct{}, 1)
	pulse := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	timer := time.NewTimer(policy.StallTimeout)
	defer timer.Stop()
	watchCtx := runCtx
	go func() {
		for {
			select {
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(policy.StallTimeout)
			case <-timer.C:
				cancel()
				return
			case <-watchCtx.Done():
				return
			}
		}
	}()
	sess, err := r.Sessions.Create(ctx, "cron: "+j.Name)
	if err != nil {
		return "", err
	}
	prompt := j.Prompt
	if attempt > 1 {
		prompt += fmt.Sprintf("\n\n[Retry context: this is attempt %d of %d; the previous attempt did not complete successfully. Continue the work and avoid repeating completed steps.]", attempt, policy.MaxAttempts)
	}
	history := []llm.Message{llm.UserText(prompt)}
	if err := r.Sessions.AppendTurn(ctx, sess.ID, history[0]); err != nil {
		return "", err
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("session_id", sess.ID, "job_id", j.ID)
	if j.Profile != "" {
		log = log.With("profile", j.Profile)
	}
	log.Info("cron run started")

	var runID string
	if r.Observability != nil {
		var err error
		runID, err = id.New("run-")
		if err != nil {
			log.Error("new observability run id", "err", err)
		} else if err := r.Observability.Start(ctx, runID, sess.ID, "cron", "job"); err != nil {
			log.Error("start observability run", "err", err)
			runID = ""
		}
	}
	outcome := "error"
	defer func() {
		if runID == "" {
			return
		}
		if err := r.Observability.Finish(context.WithoutCancel(ctx), runID, outcome); err != nil {
			log.Error("finish observability run", "err", err)
		}
	}()

	runCtx = agent.WithSession(runCtx, sess.ID)
	out, runErr := a.Run(runCtx, history, agent.Hooks{
		OnText:      func(string) { pulse() },
		OnToolStart: func(llm.ToolUse) { pulse() },
		OnToolDone:  func(llm.ToolUse, llm.ToolResult) { pulse() },
		OnUsage: func(usage llm.Usage) {
			pulse()
			if runID == "" {
				return
			}
			if err := r.Observability.RecordUsage(ctx, runID, usage); err != nil {
				log.Error("record observability usage", "err", err)
			}
		},
	})
	for _, m := range out[1:] {
		_ = r.Sessions.AppendTurn(ctx, sess.ID, m)
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) && ctx.Err() == nil {
			return "", fmt.Errorf("stalled after %s: %w", policy.StallTimeout, runErr)
		}
		return "", runErr
	}

	// Reflect cron sessions when the job completes (#59).
	if r.Sessions != nil && a.Provider != nil {
		model := a.Model
		if a.UtilityModel != "" {
			model = a.UtilityModel
		}
		if summary, err := session.Reflect(ctx, a.Provider, out, session.ReflectOptions{Model: model}); err == nil && summary != "" {
			_ = r.Sessions.SetSummary(ctx, sess.ID, summary)
		}
	}

	var reply string
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == llm.RoleAssistant {
			reply = out[i].Text()
			break
		}
	}
	if j.Deliver != "" && reply != "" && r.Deliverer != nil {
		if err := r.Deliverer.Deliver(ctx, j.Deliver, reply); err != nil {
			return reply, fmt.Errorf("deliver: %w", err)
		}
	}
	outcome = "ok"
	return reply, nil
}

// DefaultReconcile is how often the scheduler re-lists jobs from the
// store when Scheduler.Reconcile is unset.
const DefaultReconcile = 30 * time.Second

// Scheduler runs enabled jobs on their cron schedules until ctx ends.
type Scheduler struct {
	Store  *Store
	Runner *Runner
	Log    *slog.Logger
	Policy RetryPolicy
	// Reconcile is how often the scheduler re-lists jobs and syncs the
	// cron set, so adds/removals/edits made while serving take effect
	// without a restart. Zero means DefaultReconcile.
	Reconcile time.Duration
	Usage     *usage.Store
	Health    *observability.Service

	mu         sync.Mutex
	registered map[string]registration // job id -> live cron entry
}

// registration is one job's live cron entry plus the definition it was
// registered with, so reconcile can detect edits.
type registration struct {
	entry cron.EntryID
	job   Job
}

// Run loads jobs, schedules them, and blocks until ctx is done,
// reconciling the cron set with the store every Reconcile interval.
// Before returning it waits for any in-flight job to finish.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	interval := s.Reconcile
	if interval <= 0 {
		interval = DefaultReconcile
	}
	s.mu.Lock()
	s.registered = make(map[string]registration)
	s.mu.Unlock()

	c := cron.New(cron.WithParser(parser))
	if err := s.reconcile(ctx, c); err != nil {
		return err
	}
	s.Log.Info("scheduler started", "jobs", len(s.registeredIDs()))
	c.Start()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			stopCtx := c.Stop()
			<-stopCtx.Done()
			return nil
		case <-tick.C:
			if err := s.reconcile(ctx, c); err != nil {
				s.Log.Error("scheduler reconcile failed", "err", err)
			}
		}
	}
}

// reconcile syncs the cron set with the store: it registers new enabled
// jobs, drops deleted or disabled ones, and re-registers jobs whose
// definition changed.
func (s *Scheduler) reconcile(ctx context.Context, c *cron.Cron) error {
	jobs, err := s.Store.List(ctx)
	if err != nil {
		return err
	}
	if s.Health != nil {
		s.Health.MarkSchedulerTick()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		seen[j.ID] = true
		if reg, ok := s.registered[j.ID]; ok {
			if sameDefinition(reg.job, j) {
				continue
			}
			c.Remove(reg.entry)
			delete(s.registered, j.ID)
		}
		job := j
		entry, err := c.AddFunc(job.Cron, func() { s.fire(ctx, job) })
		if err != nil {
			s.Log.Error("skip job with bad cron", "job", job.ID, "err", err)
			continue
		}
		s.registered[job.ID] = registration{entry: entry, job: job}
	}
	for id, reg := range s.registered {
		if !seen[id] {
			c.Remove(reg.entry)
			delete(s.registered, id)
		}
	}
	return nil
}

// sameDefinition reports whether two snapshots of a job would schedule
// and execute identically (run bookkeeping like LastRun is ignored).
func sameDefinition(a, b Job) bool {
	return a.Cron == b.Cron && a.Name == b.Name && a.Prompt == b.Prompt && a.Deliver == b.Deliver && a.Profile == b.Profile
}

// registeredIDs returns the ids of jobs currently held by the cron set.
func (s *Scheduler) registeredIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.registered))
	for id := range s.registered {
		ids = append(ids, id)
	}
	return ids
}

func (s *Scheduler) fire(ctx context.Context, j Job) {
	if s.Usage != nil {
		paused, err := s.Usage.Paused(ctx)
		if err != nil || paused {
			return
		}
	}
	if current, err := s.Store.Get(context.WithoutCancel(ctx), j.ID); err == nil &&
		!current.NextRetry.IsZero() && time.Now().Before(current.NextRetry) {
		return
	}
	s.Log.Info("job firing", "job", j.ID, "name", j.Name)
	policy := s.Policy.normalized()
	if j.MaxAttempts > 1 || policy.MaxAttempts == 1 {
		policy.MaxAttempts = j.MaxAttempts
	}
	if j.BaseBackoff > 0 && (j.BaseBackoff != DefaultRetryPolicy().BaseBackoff || policy.BaseBackoff == DefaultRetryPolicy().BaseBackoff) {
		policy.BaseBackoff = j.BaseBackoff
	}
	if j.MaxBackoff > 0 && (j.MaxBackoff != DefaultRetryPolicy().MaxBackoff || policy.MaxBackoff == DefaultRetryPolicy().MaxBackoff) {
		policy.MaxBackoff = j.MaxBackoff
	}
	if j.StallTimeout > 0 && (j.StallTimeout != DefaultRetryPolicy().StallTimeout || policy.StallTimeout == DefaultRetryPolicy().StallTimeout) {
		policy.StallTimeout = j.StallTimeout
	}
	attempt := 1
	for {
		if err := s.Store.startAttempt(context.WithoutCancel(ctx), j.ID, attempt); err != nil {
			s.Log.Error("record job attempt failed", "job", j.ID, "err", err)
		}
		j.MaxAttempts, j.BaseBackoff, j.MaxBackoff, j.StallTimeout = policy.MaxAttempts, policy.BaseBackoff, policy.MaxBackoff, policy.StallTimeout
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		_, err := s.Runner.RunAttempt(runCtx, j, attempt)
		cancel()
		if err == nil {
			_ = s.Store.markRun(context.WithoutCancel(ctx), j.ID, "ok")
			return
		}
		s.Log.Error("job attempt failed", "job", j.ID, "attempt", attempt, "err", err)
		if attempt >= policy.MaxAttempts {
			status := "failed: " + err.Error()
			_ = s.Store.markRun(context.WithoutCancel(ctx), j.ID, status)
			if j.Deliver != "" && s.Runner.Deliverer != nil {
				notice := fmt.Sprintf("Job %q failed after %d attempt(s): %v", j.Name, attempt, err)
				if derr := s.Runner.Deliverer.Deliver(context.WithoutCancel(ctx), j.Deliver, notice); derr != nil {
					s.Log.Error("final failure delivery failed", "job", j.ID, "err", derr)
				}
			}
			return
		}
		delay := policy.BaseBackoff
		for n := 1; n < attempt; n++ {
			delay *= 2
			if delay >= policy.MaxBackoff {
				delay = policy.MaxBackoff
				break
			}
		}
		next := time.Now().Add(delay)
		_ = s.Store.scheduleRetry(context.WithoutCancel(ctx), j.ID, fmt.Sprintf("retrying: %v", err), next)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		attempt++
	}
}

// ParseTarget splits a delivery target "channel:chat_id".
func ParseTarget(target string) (channel, chatID string, ok bool) {
	channel, chatID, ok = strings.Cut(target, ":")
	return channel, chatID, ok && channel != "" && chatID != ""
}
