package intake

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/schedule"
)

// WatchConfig is one per-repo intake watcher.
type WatchConfig struct {
	Repo           string
	Label          string
	MaxConcurrency int
	Deliver        string
	PollInterval   time.Duration
}

// Validate checks required fields.
func (c WatchConfig) Validate() error {
	if c.Repo == "" {
		return fmt.Errorf("intake: repo is required")
	}
	if _, _, err := splitRepo(c.Repo); err != nil {
		return err
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("intake: max_concurrency must be at least 1")
	}
	if c.PollInterval < 0 {
		return fmt.Errorf("intake: poll_interval must be non-negative")
	}
	return nil
}

// Dispatcher opens a workspace and runs one issue through the agent.
// Implementations must enforce the restricted issue tier (no host bash,
// no memory writes) and label the issue body as untrusted.
type Dispatcher interface {
	Dispatch(ctx context.Context, cfg WatchConfig, iss Issue) (summary string, err error)
	// Cancel stops an in-flight run for the claim (workspace cleanup).
	Cancel(ctx context.Context, claim Claim) error
}

// Watcher polls a tracker and dispatches issue work under concurrency limits.
type Watcher struct {
	Config     WatchConfig
	Tracker    Tracker
	Claims     *ClaimStore
	Dispatcher Dispatcher
	Deliverer  schedule.Deliverer
	Log        *slog.Logger
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time

	mu      sync.Mutex
	running map[int]context.CancelFunc // issue number → cancel
}

func (w *Watcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// Tick performs one poll/reconcile/dispatch cycle.
func (w *Watcher) Tick(ctx context.Context) error {
	if err := w.Config.Validate(); err != nil {
		return err
	}
	if w.running == nil {
		w.running = map[int]context.CancelFunc{}
	}
	issues, err := w.Tracker.ListOpen(ctx, w.Config.Repo, w.Config.Label)
	if err != nil {
		return fmt.Errorf("list issues: %w", err)
	}
	// Index current open issues for reconcile.
	open := map[int]Issue{}
	for _, iss := range issues {
		open[iss.Number] = iss
	}
	if err := w.reconcile(ctx, open); err != nil {
		return err
	}
	ready, err := FilterReady(ctx, w.Tracker, w.Config.Repo, issues)
	if err != nil {
		return err
	}
	SortCandidates(ready)

	active, err := w.Claims.RunningCount(ctx, w.Config.Repo)
	if err != nil {
		return err
	}
	slots := w.Config.MaxConcurrency - active
	if slots < 0 {
		slots = 0
	}
	started := 0
	for _, iss := range ready {
		if started >= slots {
			break
		}
		// Skip if already actively claimed.
		existing, err := w.Claims.Get(ctx, w.Config.Repo, iss.Number)
		if err != nil {
			return err
		}
		if existing != nil && existing.Status != StatusReleased {
			continue
		}
		ok, err := w.Claims.TryClaim(ctx, w.Config.Repo, iss.Number)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := w.start(ctx, iss); err != nil {
			_ = w.Claims.Release(ctx, w.Config.Repo, iss.Number)
			w.log().Error("issue dispatch failed", "repo", w.Config.Repo, "issue", iss.Number, "err", err)
			continue
		}
		started++
	}
	return nil
}

func (w *Watcher) reconcile(ctx context.Context, open map[int]Issue) error {
	active, err := w.Claims.Active(ctx, w.Config.Repo)
	if err != nil {
		return err
	}
	for _, c := range active {
		iss, stillOpen := open[c.IssueNumber]
		labelOK := w.Config.Label == "" || (stillOpen && hasLabel(iss.Labels, w.Config.Label))
		if stillOpen && labelOK {
			continue
		}
		// Closed, reassigned away, or label removed: cancel and release.
		w.mu.Lock()
		cancel := w.running[c.IssueNumber]
		delete(w.running, c.IssueNumber)
		w.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if w.Dispatcher != nil {
			if err := w.Dispatcher.Cancel(ctx, c); err != nil {
				w.log().Error("cancel issue run", "issue", c.IssueNumber, "err", err)
			}
		}
		if err := w.Claims.Release(ctx, c.Repo, c.IssueNumber); err != nil {
			return err
		}
		w.log().Info("released issue claim after reconcile", "repo", c.Repo, "issue", c.IssueNumber)
	}
	return nil
}

func (w *Watcher) start(ctx context.Context, iss Issue) error {
	if w.Dispatcher == nil {
		return fmt.Errorf("intake: no dispatcher")
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.running[iss.Number] = cancel
	w.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			w.mu.Lock()
			delete(w.running, iss.Number)
			w.mu.Unlock()
			_ = w.Claims.Release(context.WithoutCancel(ctx), w.Config.Repo, iss.Number)
		}()
		_ = w.Claims.MarkRunning(runCtx, w.Config.Repo, iss.Number, "", "")
		summary, err := w.Dispatcher.Dispatch(runCtx, w.Config, iss)
		if err != nil {
			w.log().Error("issue run failed", "issue", iss.Number, "err", err)
			summary = fmt.Sprintf("issue #%d failed: %v", iss.Number, err)
		}
		if summary == "" {
			summary = fmt.Sprintf("issue #%d completed", iss.Number)
		}
		if w.Config.Deliver != "" && w.Deliverer != nil {
			if derr := w.Deliverer.Deliver(context.WithoutCancel(ctx), w.Config.Deliver, summary); derr != nil {
				w.log().Error("issue delivery failed", "issue", iss.Number, "err", derr)
			}
		}
	}()
	return nil
}

// Run polls until ctx is cancelled. Interval defaults to 1m when unset.
func (w *Watcher) Run(ctx context.Context) error {
	interval := w.Config.PollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	if err := w.Tick(ctx); err != nil {
		w.log().Error("intake tick failed", "err", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.log().Error("intake tick failed", "err", err)
			}
		}
	}
}
