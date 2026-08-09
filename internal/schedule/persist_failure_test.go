package schedule

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// failingStore wraps a real Store and fails the attempt/outcome/retry
// persistence writes under test (#299). The real store still owns the
// underlying SQLite handle so reads keep working.
type failingStore struct {
	*Store
	failStartAttempt  bool
	failMarkRun       bool
	failScheduleRetry bool
}

func (f *failingStore) startAttempt(ctx context.Context, id string, attempt int) error {
	if f.failStartAttempt {
		return errors.New("simulated startAttempt failure")
	}
	return f.Store.startAttempt(ctx, id, attempt)
}

func (f *failingStore) markRun(ctx context.Context, id, status string) error {
	if f.failMarkRun {
		return errors.New("simulated markRun failure")
	}
	return f.Store.markRun(ctx, id, status)
}

func (f *failingStore) scheduleRetry(ctx context.Context, id, status string, next time.Time) error {
	if f.failScheduleRetry {
		return errors.New("simulated scheduleRetry failure")
	}
	return f.Store.scheduleRetry(ctx, id, status, next)
}

// recordingStore wraps a Store and appends to events when persistence writes
// are attempted, so tests can assert the ordering of markRun vs delivery.
type recordingStore struct {
	*Store
	mu     sync.Mutex
	events *[]string
}

func (r *recordingStore) startAttempt(ctx context.Context, id string, attempt int) error {
	r.mu.Lock()
	*r.events = append(*r.events, "startAttempt")
	r.mu.Unlock()
	return r.Store.startAttempt(ctx, id, attempt)
}

func (r *recordingStore) markRun(ctx context.Context, id, status string) error {
	r.mu.Lock()
	*r.events = append(*r.events, "mark:"+status)
	r.mu.Unlock()
	return r.Store.markRun(ctx, id, status)
}

func (r *recordingStore) scheduleRetry(ctx context.Context, id, status string, next time.Time) error {
	r.mu.Lock()
	*r.events = append(*r.events, "scheduleRetry")
	r.mu.Unlock()
	return r.Store.scheduleRetry(ctx, id, status, next)
}

// eventDeliverer records the moment a delivery happens relative to the
// store's persistence writes.
type eventDeliverer struct {
	mu     sync.Mutex
	events *[]string
}

func (d *eventDeliverer) Deliver(_ context.Context, _, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	*d.events = append(*d.events, "deliver")
	return nil
}

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// slog.Handler must not retain the Record: it may reference transient
	// backing storage, so clone before storing.
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.records))
	for _, r := range h.records {
		out = append(out, r.Message)
	}
	return out
}

func TestFireAbortsWhenStartAttemptCannotPersist(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 0}
	jobs, s, j := retryFixture(t, p, c)
	s.Store = &failingStore{Store: jobs, failStartAttempt: true}

	s.fire(context.Background(), j)

	if p.count() != 0 {
		t.Fatalf("job ran despite a failed startAttempt: %d agent calls", p.count())
	}
	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 0 || !got.LastRun.IsZero() {
		t.Fatalf("job bookkeeping changed despite aborted fire: %+v", got)
	}
}

func TestFireDoesNotDeliverSuccessWhenMarkRunFails(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 0}
	jobs, s, j := retryFixture(t, p, c)
	d := &noticeDeliverer{}
	s.Runner.Deliverer = d
	s.Store = &failingStore{Store: jobs, failMarkRun: true}

	s.fire(context.Background(), j)

	if p.count() != 1 {
		t.Fatalf("agent calls = %d, want 1", p.count())
	}
	if len(d.notices) != 0 {
		t.Fatalf("success was delivered even though the outcome could not be persisted: %v", d.notices)
	}
	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus != "" {
		t.Fatalf("job status = %q, want empty (markRun failed)", got.LastStatus)
	}
}

func TestFirePersistsSuccessBeforeDelivering(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 0}
	jobs, s, j := retryFixture(t, p, c)
	events := &[]string{}
	store := &recordingStore{Store: jobs, events: events}
	s.Store = store
	s.Runner.Deliverer = &eventDeliverer{events: events}

	s.fire(context.Background(), j)

	if got := *events; !slices.Equal(got, []string{"startAttempt", "mark:ok", "deliver"}) {
		t.Fatalf("events = %v, want [startAttempt mark:ok deliver]", got)
	}
}

func TestFireLogsOutcomeFailureAndStillNotifiesOnTerminalFailure(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 99}
	jobs, s, j := retryFixture(t, p, c)
	_, _ = jobs.db.Exec(`UPDATE jobs SET max_attempts=1 WHERE id=?`, j.ID)
	j.MaxAttempts = 1
	d := &noticeDeliverer{}
	s.Runner.Deliverer = d
	handler := &captureHandler{}
	s.Log = slog.New(handler)
	s.Store = &failingStore{Store: jobs, failMarkRun: true}

	s.fire(context.Background(), j)

	if len(d.notices) != 1 || !strings.Contains(d.notices[0], "failed after 1 attempt") {
		t.Fatalf("failure notice not delivered despite markRun failure: %v", d.notices)
	}
	if !slices.Contains(handler.messages(), "record job outcome failed") {
		t.Fatalf("markRun failure was not logged: %v", handler.messages())
	}
	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus != "" {
		t.Fatalf("job status = %q, want empty (markRun failed)", got.LastStatus)
	}
}

func TestFireAbortsInProcessRetryWhenScheduleRetryFails(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 99}
	jobs, s, j := retryFixture(t, p, c)
	_, _ = jobs.db.Exec(`UPDATE jobs SET max_attempts=4 WHERE id=?`, j.ID)
	j.MaxAttempts = 4
	s.Store = &failingStore{Store: jobs, failScheduleRetry: true}

	done := make(chan struct{})
	go func() { s.fire(context.Background(), j); close(done) }()
	waitCalls(t, p, 1)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fire kept waiting for a retry that was never persisted")
	}
	if p.count() != 1 {
		t.Fatalf("in-process retry ran without a durable retry record: %d agent calls", p.count())
	}
	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 || !got.NextRetry.IsZero() {
		t.Fatalf("job = %+v, want attempt 1 with no durable retry", got)
	}
}

// failingDeliverer fails the success delivery so tests can assert the durable
// status reflects it (#299 review).
type failingDeliverer struct{ err error }

func (d *failingDeliverer) Deliver(_ context.Context, _, _ string) error { return d.err }

func TestFireRecordsDeliveryFailureInDurableStatus(t *testing.T) {
	c := newManualClock()
	p := &sequenceProvider{failFor: 0}
	jobs, s, j := retryFixture(t, p, c)
	s.Runner.Deliverer = &failingDeliverer{err: errors.New("channel timeout")}

	s.fire(context.Background(), j)

	got, err := jobs.Get(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.LastStatus, "ok") || !strings.Contains(got.LastStatus, "delivery failed: channel timeout") {
		t.Fatalf("last_status = %q, want the delivery failure recorded", got.LastStatus)
	}
}
