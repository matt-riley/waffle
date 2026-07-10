package observability

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

func TestRecordUsageDoesNotDoubleCountDuplicateCumulativeObservation(t *testing.T) {
	ctx, svc, clock := testService(t)
	if err := svc.Start(ctx, "run-1", "session-1", "gateway", "agent"); err != nil {
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
	if err := svc.Start(ctx, "run-1", "session-1", "cron", "job"); err != nil {
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
	if got.RuntimeMS != 1200 || got.InputTokens != 3 || got.OutputTokens != 7 {
		t.Errorf("metrics = runtime %dms, tokens %d/%d; want 1200ms, 3/7", got.RuntimeMS, got.InputTokens, got.OutputTokens)
	}
}

func TestSnapshotReportsActiveElapsedRuntime(t *testing.T) {
	ctx, svc, clock := testService(t)
	if err := svc.Start(ctx, "run-1", "session-1", "gateway", "agent"); err != nil {
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
