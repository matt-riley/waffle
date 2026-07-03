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
	"time"

	"github.com/robfig/cron/v3"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// Job is one scheduled task.
type Job struct {
	ID         string
	Name       string
	Cron       string
	Prompt     string
	Deliver    string // "channel:chat_id" or "" for log-only
	Enabled    bool
	LastRun    time.Time
	LastStatus string
	CreatedAt  time.Time
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
	if _, err := parser.Parse(spec); err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", spec, err)
	}
	jobID, err := id.New("job-")
	if err != nil {
		return nil, err
	}
	j := &Job{ID: jobID, Name: name, Cron: spec, Prompt: prompt, Deliver: deliver, Enabled: true}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, name, cron, prompt, deliver, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)`, j.ID, name, spec, prompt, deliver, now())
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
func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, cron, prompt, deliver, enabled, last_run, last_status, created_at
		FROM jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []Job
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
		SELECT id, name, cron, prompt, deliver, enabled, last_run, last_status, created_at
		FROM jobs WHERE id = ?`, id))
}

func (s *Store) markRun(ctx context.Context, id, status string) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE jobs SET last_run = ?, last_status = ? WHERE id = ?`, now(), status, id)
}

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (*Job, error) {
	var j Job
	var enabled int
	var lastRun, created string
	err := row.Scan(&j.ID, &j.Name, &j.Cron, &j.Prompt, &j.Deliver, &enabled, &lastRun, &j.LastStatus, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("job not found")
	}
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	j.LastRun, _ = time.Parse(time.RFC3339Nano, lastRun)
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &j, nil
}

// Runner executes a job's prompt through a fresh session and delivers the
// result. It is provided by the caller (serve wires the real agent).
type Runner struct {
	Agent     *agent.Agent
	Sessions  *session.Store
	Deliverer Deliverer
	Log       *slog.Logger
}

// Run executes one job now: fresh session, agent turn, deliver the reply.
func (r *Runner) Run(ctx context.Context, j Job) (string, error) {
	sess, err := r.Sessions.Create(ctx, "cron: "+j.Name)
	if err != nil {
		return "", err
	}
	history := []llm.Message{llm.UserText(j.Prompt)}
	if err := r.Sessions.AppendTurn(ctx, sess.ID, history[0]); err != nil {
		return "", err
	}
	out, runErr := r.Agent.Run(ctx, history, agent.Hooks{})
	for _, m := range out[1:] {
		_ = r.Sessions.AppendTurn(ctx, sess.ID, m)
	}
	if runErr != nil {
		return "", runErr
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
	return reply, nil
}

// Scheduler runs enabled jobs on their cron schedules until ctx ends.
type Scheduler struct {
	Store  *Store
	Runner *Runner
	Log    *slog.Logger
}

// Run loads jobs, schedules them, and blocks until ctx is done.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	jobs, err := s.Store.List(ctx)
	if err != nil {
		return err
	}
	c := cron.New(cron.WithParser(parser))
	scheduled := 0
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		job := j
		if _, err := c.AddFunc(job.Cron, func() { s.fire(ctx, job) }); err != nil {
			s.Log.Error("skip job with bad cron", "job", job.ID, "err", err)
			continue
		}
		scheduled++
	}
	s.Log.Info("scheduler started", "jobs", scheduled)
	c.Start()
	<-ctx.Done()
	stopCtx := c.Stop()
	<-stopCtx.Done()
	return nil
}

func (s *Scheduler) fire(ctx context.Context, j Job) {
	s.Log.Info("job firing", "job", j.ID, "name", j.Name)
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, err := s.Runner.Run(runCtx, j)
	status := "ok"
	if err != nil {
		status = "error: " + err.Error()
		s.Log.Error("job failed", "job", j.ID, "err", err)
	}
	s.Store.markRun(context.WithoutCancel(runCtx), j.ID, status)
}

// ParseTarget splits a delivery target "channel:chat_id".
func ParseTarget(target string) (channel, chatID string, ok bool) {
	channel, chatID, ok = strings.Cut(target, ":")
	return channel, chatID, ok && channel != "" && chatID != ""
}
