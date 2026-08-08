package observability

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

func TestStatusHandlerReturnsSnapshotJSONWithEmptyArrays(t *testing.T) {
	ctx, svc, _ := testService(t)

	recorder := httptest.NewRecorder()
	NewHandler(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status", nil).WithContext(ctx))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var snapshot Snapshot
	if err := json.NewDecoder(recorder.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if snapshot.Active == nil || snapshot.Recent == nil || snapshot.RetryQueue == nil {
		t.Fatalf("status JSON arrays must not be nil: %+v", snapshot)
	}
	if len(snapshot.Active) != 0 || len(snapshot.Recent) != 0 || len(snapshot.RetryQueue) != 0 {
		t.Errorf("status JSON = %+v, want empty snapshot", snapshot)
	}
}

func TestHealthHandlerReturns503ForStaleSubsystem(t *testing.T) {
	ctx, svc, clock := testService(t)
	svc.MarkSchedulerTick()
	svc.MarkAdapter("telegram")
	*clock = clock.Add(3 * time.Minute)
	recorder := httptest.NewRecorder()
	NewHandler(svc).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(ctx))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	var health Health
	if err := json.NewDecoder(recorder.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Healthy || !health.Scheduler.Stale || !health.Adapters["telegram"].Stale {
		t.Fatalf("health = %+v", health)
	}
}

func TestRegisterRoutesPreservesStatusAndHealthContracts(t *testing.T) {
	ctx, svc, clock := testService(t)
	svc.MarkSchedulerTick()
	svc.MarkAdapter("telegram")
	*clock = clock.Add(3 * time.Minute)
	mux := http.NewServeMux()
	RegisterRoutes(mux, svc)

	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/status", want: http.StatusOK},
		{path: "/healthz", want: http.StatusServiceUnavailable},
	} {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil).WithContext(ctx))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestServeHandlerStopsOnContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeHandler(ctx, listener, http.NotFoundHandler()) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHandler() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeHandler did not stop after cancellation")
	}
}

func TestRecordUsageDoesNotDoubleCountDuplicateCumulativeObservation(t *testing.T) {
	ctx, svc, clock := testService(t)
	if err := svc.Start(ctx, "run-1", "session-1", "gateway", "agent", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordUsage(ctx, "run-1", llm.Usage{InputTokens: 10, OutputTokens: 4}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordUsage(ctx, "run-1", llm.Usage{InputTokens: 10, OutputTokens: 4}); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(2 * time.Second)
	if err := svc.Finish(ctx, "run-1", "ok"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Recent) != 1 {
		t.Fatalf("recent runs = %d, want 1", len(snapshot.Recent))
	}
	got := snapshot.Recent[0]
	if got.InputTokens != 10 || got.OutputTokens != 4 {
		t.Errorf("token totals = %d/%d, want 10/4", got.InputTokens, got.OutputTokens)
	}
}

func TestSnapshotReportsCompletedRunRuntimeAndTotals(t *testing.T) {
	ctx, svc, clock := testService(t)
	if err := svc.Start(ctx, "run-1", "session-1", "cron", "job", "researcher"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordUsage(ctx, "run-1", llm.Usage{InputTokens: 3, OutputTokens: 7}); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(1200 * time.Millisecond)
	if err := svc.Finish(ctx, "run-1", "ok"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Recent) != 1 {
		t.Fatalf("recent runs = %d, want 1", len(snapshot.Recent))
	}
	got := snapshot.Recent[0]
	if got.ID != "run-1" || got.SessionID != "session-1" || got.Source != "cron" || got.Phase != "job" || got.Outcome != "ok" {
		t.Errorf("recent run = %+v", got)
	}
	if got.Profile != "researcher" {
		t.Errorf("profile = %q, want researcher", got.Profile)
	}
	if got.RuntimeMS != 1200 || got.InputTokens != 3 || got.OutputTokens != 7 {
		t.Errorf("metrics = runtime %dms, tokens %d/%d; want 1200ms, 3/7", got.RuntimeMS, got.InputTokens, got.OutputTokens)
	}
}

func TestSnapshotReportsActiveElapsedRuntime(t *testing.T) {
	ctx, svc, clock := testService(t)
	if err := svc.Start(ctx, "run-1", "session-1", "gateway", "agent", "main"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordUsage(ctx, "run-1", llm.Usage{InputTokens: 1, OutputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(1250 * time.Millisecond)

	snapshot, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Active) != 1 {
		t.Fatalf("active runs = %d, want 1", len(snapshot.Active))
	}
	got := snapshot.Active[0]
	if got.ElapsedMS != 1250 || got.InputTokens != 1 || got.OutputTokens != 2 {
		t.Errorf("metrics = elapsed %dms, tokens %d/%d; want 1250ms, 1/2", got.ElapsedMS, got.InputTokens, got.OutputTokens)
	}
}

func TestSnapshotIsEmptyWithStableEmptyArrays(t *testing.T) {
	ctx, svc, _ := testService(t)

	snapshot, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Active == nil || snapshot.Recent == nil || snapshot.RetryQueue == nil {
		t.Fatalf("snapshot arrays must not be nil: %+v", snapshot)
	}
	if len(snapshot.Active) != 0 || len(snapshot.Recent) != 0 || len(snapshot.RetryQueue) != 0 {
		t.Errorf("empty snapshot = %+v", snapshot)
	}
}

func TestNilStoreFinishAndSnapshotDoNotPanic(t *testing.T) {
	svc := New(nil, nil)
	ctx := context.Background()

	if err := svc.Start(ctx, "r1", "s1", "src", "agent", ""); err != nil {
		t.Fatal(err)
	}

	// Snapshot with an active run and no store must not panic.
	snap, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot with nil store: %v", err)
	}
	if len(snap.Active) != 1 {
		t.Fatalf("active runs = %d, want 1", len(snap.Active))
	}
	if snap.Recent == nil || snap.RetryQueue == nil {
		t.Fatalf("nil-store snapshot arrays must not be nil: %+v", snap)
	}
	if len(snap.Recent) != 0 || len(snap.RetryQueue) != 0 {
		t.Errorf("nil-store recent/retry = %+v, want empty", snap)
	}

	// Finish removes from active without persisting when store is nil.
	if err := svc.Finish(ctx, "r1", "ok"); err != nil {
		t.Fatalf("Finish with nil store: %v", err)
	}

	snap, err = svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot after Finish: %v", err)
	}
	if len(snap.Active) != 0 {
		t.Errorf("active after Finish = %d, want 0", len(snap.Active))
	}
	if len(snap.Recent) != 0 {
		t.Errorf("recent after Finish with nil store = %d, want 0", len(snap.Recent))
	}

	// HealthSnapshot already degrades gracefully with a nil store.
	health, err := svc.HealthSnapshot(ctx, time.Minute)
	if err != nil {
		t.Fatalf("HealthSnapshot with nil store: %v", err)
	}
	if health.Database || health.Healthy {
		t.Errorf("nil-store health = %+v, want Database=false Healthy=false", health)
	}
}

func testService(t *testing.T) (context.Context, *Service, *time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, time.July, 10, 12, 0, 0, 0, time.UTC)
	return ctx, New(st, func() time.Time { return now }), &now
}

// TestFinishClearsActiveRunWhenMetricsPersistFails covers #261: a failed
// run_metrics insert used to leave the run pinned active for the life of the
// process, and no retry could clear it because run_metrics.id is the primary
// key.
func TestFinishClearsActiveRunWhenMetricsPersistFails(t *testing.T) {
	ctx, svc, _ := testService(t)
	// A row already holding this id makes the insert fail on the primary key —
	// the same shape as the retry that could never succeed.
	if _, err := svc.store.DB.ExecContext(ctx, `
		INSERT INTO run_metrics
			(id, session_id, source, phase, outcome, started_at_ms, ended_at_ms, input_tokens, output_tokens, profile)
		VALUES ('run-1', 'session-1', 'gateway', 'agent', 'ok', 0, 1, 0, 0, 'main')`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx, "run-1", "session-1", "gateway", "agent", "main"); err != nil {
		t.Fatal(err)
	}

	finishErr := svc.Finish(ctx, "run-1", "error")
	if finishErr == nil {
		t.Fatal("Finish reported success with no metrics row written")
	}
	// The caller only logs this error, so it has to carry what was lost.
	for _, want := range []string{"run-1", "error"} {
		if !strings.Contains(finishErr.Error(), want) {
			t.Errorf("Finish error = %v, want it to name %q", finishErr, want)
		}
	}

	snapshot, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Active) != 0 {
		t.Fatalf("active runs = %+v, want the finished run gone despite the lost metrics", snapshot.Active)
	}
	// The entry is gone for good, not merely hidden from this snapshot: a
	// second Finish reports the run as unknown rather than re-attempting a
	// doomed insert.
	if err := svc.Finish(ctx, "run-1", "error"); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("second Finish = %v, want a not-active error", err)
	}
}
