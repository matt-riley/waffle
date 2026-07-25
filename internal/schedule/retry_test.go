package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	clock  *manualClock
	ch     chan time.Time
	due    time.Time
	active bool
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *manualClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{clock: c, ch: make(chan time.Time, 1), due: c.now.Add(d), active: true}
	c.timers = append(c.timers, t)
	return t
}
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	for _, t := range c.timers {
		if t.active && !t.due.After(now) {
			t.active = false
			select {
			case t.ch <- now:
			default:
			}
		}
	}
	c.mu.Unlock()
}
func (t *manualTimer) C() <-chan time.Time { return t.ch }
func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	return was
}
func (t *manualTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = true
	t.due = t.clock.now.Add(d)
	select {
	case <-t.ch:
	default:
	}
	return was
}

type sequenceProvider struct {
	mu      sync.Mutex
	failFor int
	calls   int
	prompts []string
	started chan struct{}
	release chan struct{}
}

func (p *sequenceProvider) Complete(_ context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].Text()
	if strings.Contains(last, "Summarize it in 2-3 sentences") {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "summary"}}}, StopReason: llm.StopEndTurn}, nil
	}
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.prompts = append(p.prompts, last)
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	p.mu.Unlock()
	if p.release != nil {
		<-p.release
	}
	if call <= p.failFor {
		return nil, errors.New("transient")
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}}, StopReason: llm.StopEndTurn}, nil
}
func (p *sequenceProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

type noticeDeliverer struct {
	mu      sync.Mutex
	notices []string
}

func (d *noticeDeliverer) Deliver(_ context.Context, _ string, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notices = append(d.notices, text)
	return nil
}

func retryFixture(t *testing.T, p llm.Provider, c Clock) (*Store, *Scheduler, Job) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	jobs := NewStore(st)
	j, err := jobs.Add(ctx, "retry", "0 3 * * *", "do work", "telegram:1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB.Exec(`UPDATE jobs SET max_attempts=4, base_backoff='10s', max_backoff='15s', stall_timeout='1m' WHERE id=?`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	j, err = jobs.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Agent: &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}, Sessions: session.New(st), Clock: c}
	s := &Scheduler{Store: jobs, Runner: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: c}
	return jobs, s, *j
}

func waitCalls(t *testing.T, p *sequenceProvider, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for p.count() < n {
		select {
		case <-deadline:
			t.Fatalf("calls=%d want %d", p.count(), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func waitRetry(t *testing.T, jobs *Store, id string, attempt int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		j, err := jobs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Attempt == attempt && !j.NextRetry.IsZero() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("attempt=%d next=%v, want pending attempt %d", j.Attempt, j.NextRetry, attempt)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestRetryBackoffIsDeterministicCappedAndEventuallySucceeds(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 3}
	jobs, s, j := retryFixture(t, p, c)
	done := make(chan struct{})
	go func() { s.fire(context.Background(), j); close(done) }()
	waitCalls(t, p, 1)
	for i, step := range []time.Duration{10 * time.Second, 15 * time.Second, 15 * time.Second} {
		waitRetry(t, jobs, j.ID, i+1)
		pending, err := jobs.Get(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := pending.NextRetry.Sub(c.Now()); got != step {
			t.Fatalf("attempt %d backoff=%s want %s", i+1, got, step)
		}
		c.Advance(step)
		waitCalls(t, p, i+2)
	}
	<-done
	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.count() != 4 || got.LastStatus != "ok" || got.Attempt != 4 {
		t.Fatalf("calls=%d job=%+v", p.count(), got)
	}
	if !strings.Contains(p.prompts[1], "attempt 2 of 4") {
		t.Fatalf("retry prompt=%q", p.prompts[1])
	}
}

func TestMaxAttemptsExhaustionDeliversOneFinalNotice(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 99}
	jobs, s, j := retryFixture(t, p, c)
	d := &noticeDeliverer{}
	s.Runner.Deliverer = d
	_, _ = jobs.db.Exec(`UPDATE jobs SET max_attempts=2 WHERE id=?`, j.ID)
	j.MaxAttempts = 2
	done := make(chan struct{})
	go func() { s.fire(context.Background(), j); close(done) }()
	waitCalls(t, p, 1)
	waitRetry(t, jobs, j.ID, 1)
	c.Advance(10 * time.Second)
	waitCalls(t, p, 2)
	<-done
	if len(d.notices) != 1 || !strings.Contains(d.notices[0], "failed after 2 attempt") {
		t.Fatalf("notices=%v", d.notices)
	}
}

type stalledProvider struct {
	started  chan struct{}
	canceled chan struct{}
}

type meteredFailureProvider struct{ calls int }

func (p *meteredFailureProvider) Complete(_ context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.calls++
	if p.calls%2 == 0 {
		return nil, errors.New("retry me")
	}
	use := llm.ToolUse{ID: "t", Name: "meter", Input: []byte(`{}`)}
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ToolUse: &use}}},
		StopReason: llm.StopToolUse,
		Usage:      llm.Usage{InputTokens: 3, OutputTokens: 2},
	}, nil
}

func TestRetryAttemptsConsumeOneStableJobBudget(t *testing.T) {
	c := newManualClock()
	p := &meteredFailureProvider{}
	jobs, s, j := retryFixture(t, p, c)
	_, _ = jobs.db.Exec(`UPDATE jobs SET max_attempts=2 WHERE id=?`, j.ID)
	j.MaxAttempts = 2
	u := usage.New(&store.Store{DB: jobs.db})
	s.Runner.Agent.Usage = u
	s.Runner.Agent.Tools = tool.NewRegistry(namedTool("meter"))
	done := make(chan struct{})
	go func() { s.fire(context.Background(), j); close(done) }()
	waitRetry(t, jobs, j.ID, 1)
	c.Advance(10 * time.Second)
	<-done
	rows, err := u.List(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var requests, tokens int
	for _, row := range rows {
		if row.Period == "day" {
			requests += row.Requests
			tokens += row.InputTokens + row.OutputTokens
		}
	}
	if requests != 2 || tokens != 10 {
		t.Fatalf("requests=%d tokens=%d rows=%+v", requests, tokens, rows)
	}
}

func (p *stalledProvider) Complete(ctx context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	close(p.canceled)
	return nil, ctx.Err()
}

func TestStallCancelsContextAndPersistsStalledRetry(t *testing.T) {
	c := newManualClock()
	p := &stalledProvider{started: make(chan struct{}, 1), canceled: make(chan struct{})}
	jobs, s, j := retryFixture(t, p, c)
	_, _ = jobs.db.Exec(`UPDATE jobs SET stall_timeout='20s' WHERE id=?`, j.ID)
	j.StallTimeout = 20 * time.Second
	go s.fire(context.Background(), j)
	<-p.started
	c.Advance(20 * time.Second)
	select {
	case <-p.canceled:
	case <-time.After(time.Second):
		t.Fatal("run context was not canceled")
	}
	deadline := time.After(time.Second)
	for {
		got, err := jobs.Get(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastStatus == "Stalled" && got.Attempt == 1 && !got.NextRetry.IsZero() {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job=%+v", got)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestPendingRetrySurvivesReopenAndStartsWithoutCronTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "waffle.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	jobs := NewStore(st)
	j, err := jobs.Add(ctx, "daily", "0 3 * * *", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	c := newManualClock()
	next := c.Now().Add(30 * time.Second)
	_, err = st.DB.Exec(`UPDATE jobs SET attempt=1,next_retry=?,max_attempts=2,base_backoff='10s' WHERE id=?`, next.Format(time.RFC3339Nano), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	p := &sequenceProvider{}
	s := &Scheduler{Store: NewStore(st), Runner: &Runner{Agent: &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}, Sessions: session.New(st), Clock: c}, Clock: c, Reconcile: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	if p.count() != 0 {
		t.Fatalf("retry ran before due: %d", p.count())
	}
	c.Advance(30 * time.Second)
	waitCalls(t, p, 1)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPendingRetryRechecksPauseAndDisableAtDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(context.Context, *Store, *Scheduler, string) error
	}{
		{name: "paused", change: func(ctx context.Context, _ *Store, s *Scheduler, _ string) error { return s.Usage.SetPaused(ctx, true) }},
		{name: "disabled", change: func(ctx context.Context, jobs *Store, _ *Scheduler, id string) error {
			_, err := jobs.db.ExecContext(ctx, `UPDATE jobs SET enabled=0 WHERE id=?`, id)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newManualClock()
			p := &sequenceProvider{}
			jobs, s, j := retryFixture(t, p, c)
			u := usage.New(&store.Store{DB: jobs.db})
			s.Usage = u
			next := c.Now().Add(30 * time.Second)
			_, err := jobs.db.Exec(`UPDATE jobs SET attempt=1,next_retry=?,max_attempts=2 WHERE id=?`, next.Format(time.RFC3339Nano), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			j, err = func() (Job, error) {
				got, err := jobs.Get(context.Background(), j.ID)
				if err != nil {
					return Job{}, err
				}
				return *got, nil
			}()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { s.startFire(context.Background(), j); close(done) }()
			deadline := time.After(time.Second)
			for {
				s.runMu.Lock()
				active := s.inFlight[j.ID]
				s.runMu.Unlock()
				if active {
					break
				}
				select {
				case <-deadline:
					t.Fatal("retry waiter did not start")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			if err := tc.change(context.Background(), jobs, s, j.ID); err != nil {
				t.Fatal(err)
			}
			c.Advance(30 * time.Second)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("retry waiter did not stop")
			}
			if p.count() != 0 {
				t.Fatalf("provider calls=%d", p.count())
			}
		})
	}
}

// TestAwaitRetryReadySkippedLoopRereadsBeforeEnabledCheck covers the
// interleaving from #196 directly, without relying on timing: a retry
// deadline that has already elapsed by the time the wait loop's condition is
// first evaluated means the loop body never runs. Regression would reuse the
// pre-wait snapshot for the Enabled check instead of reloading, so a disable
// that landed between the snapshot and the check would be missed.
func TestAwaitRetryReadySkippedLoopRereadsBeforeEnabledCheck(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{}
	jobs, s, j := retryFixture(t, p, c)
	past := c.Now().Add(-time.Second)
	_, err := jobs.db.Exec(`UPDATE jobs SET attempt=1,next_retry=?,max_attempts=2 WHERE id=?`, past.Format(time.RFC3339Nano), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	staleSnapshot, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !staleSnapshot.Enabled {
		t.Fatal("fixture job must start enabled")
	}
	// Simulate the race: disable the job in the store after staleSnapshot was
	// captured, mirroring a disable landing between fire's initial snapshot
	// and its enabled check.
	if _, err := jobs.db.Exec(`UPDATE jobs SET enabled=0 WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	// The deadline is already in the past, so the wait loop's condition is
	// false on first evaluation and its body never runs.
	if !staleSnapshot.NextRetry.IsZero() && c.Now().Before(staleSnapshot.NextRetry) {
		t.Fatal("test setup invalid: deadline must already be elapsed")
	}
	fresh, ready := s.awaitRetryReady(context.Background(), staleSnapshot)
	if ready {
		t.Fatal("awaitRetryReady reported ready for a job disabled before the check")
	}
	if fresh.Enabled {
		t.Fatal("awaitRetryReady did not reload the job's current enabled state")
	}
}

func TestMaxAttemptsOnePreservesSingleRunBehavior(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 1}
	jobs, s, j := retryFixture(t, p, c)
	_, _ = jobs.db.Exec(`UPDATE jobs SET max_attempts=1 WHERE id=?`, j.ID)
	j.MaxAttempts = 1
	s.fire(context.Background(), j)
	if p.count() != 1 {
		t.Fatalf("calls=%d", p.count())
	}
}

func TestNormalExitWithDefaultPolicyRunsOnce(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{}
	jobs, s, j := retryFixture(t, p, c)
	_, _ = jobs.db.Exec(`UPDATE jobs SET max_attempts=1 WHERE id=?`, j.ID)
	j.MaxAttempts = 1
	s.fire(context.Background(), j)
	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.count() != 1 || got.LastStatus != "ok" || !got.NextRetry.IsZero() {
		t.Fatalf("calls=%d job=%+v", p.count(), got)
	}
}

func TestConcurrentFiringsRunJobOnlyOnce(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{started: make(chan struct{}, 4), release: make(chan struct{})}
	_, s, j := retryFixture(t, p, c)
	start := make(chan struct{})
	p.failFor = 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; s.startFire(context.Background(), j) }()
	}
	close(start)
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("job did not start")
	}
	time.Sleep(10 * time.Millisecond)
	close(p.release)
	wg.Wait()
	if p.count() != 1 {
		t.Fatalf("overlapping firings made %d provider calls", p.count())
	}
}
