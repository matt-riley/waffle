package workspace

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/sandbox"
)

// realBusyError produces a genuine SQLITE_BUSY from the driver rather than a
// fabricated one: sqlite.Error has unexported fields, and a hand-rolled stand-in
// would not prove the classifier recognises what the driver actually returns.
func realBusyError(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	// busy_timeout(0) so the contended read returns immediately instead of
	// waiting out a timeout inside the test.
	writer, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	reader, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := writer.Exec(`CREATE TABLE results (created_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	// BEGIN EXCLUSIVE, not a plain write transaction: a RESERVED lock still
	// permits readers, so only an exclusive lock reproduces the contention the
	// runner creates.
	ctx := context.Background()
	held, err := writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, err := held.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = held.ExecContext(ctx, `ROLLBACK`) }()

	var scanned string
	busy := reader.QueryRow(`SELECT created_at FROM results`).Scan(&scanned)
	if busy == nil {
		t.Skip("this SQLite build did not surface contention as an error")
	}
	if !sandbox.IsBusyErr(busy) {
		t.Fatalf("contended read produced %v, which IsBusyErr does not classify as busy", busy)
	}
	return busy
}

func TestWaitForInspectionHeartbeatRetriesTransientContention(t *testing.T) {
	t.Parallel()
	busy := realBusyError(t)
	startedAt := time.Date(2026, time.July, 25, 13, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 4)
	for i := 0; i < 4; i++ {
		ticks <- startedAt.Add(time.Duration(i+1) * time.Second)
	}
	queries := 0

	err := waitForInspectionHeartbeat(context.Background(), startedAt, inspectionRunnerReadyTimeout,
		func(context.Context) (time.Time, error) {
			queries++
			// The runner holds the queue mid-write for the first two probes.
			if queries <= 2 {
				return time.Time{}, busy
			}
			return startedAt.Add(20 * time.Second), nil
		}, ticks)

	if err != nil {
		t.Fatalf("contended heartbeat read abandoned a closeable workspace: %v", err)
	}
	if queries != 3 {
		t.Errorf("heartbeat probes = %d, want 3 (two contended, one observed)", queries)
	}
}

func TestWaitForInspectionHeartbeatFailsFastOnRealErrors(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, time.July, 25, 13, 0, 0, 0, time.UTC)
	fatal := errors.New("no such table: results")
	ticks := make(chan time.Time, 1)
	ticks <- startedAt.Add(time.Second)
	queries := 0

	err := waitForInspectionHeartbeat(context.Background(), startedAt, inspectionRunnerReadyTimeout,
		func(context.Context) (time.Time, error) {
			queries++
			return time.Time{}, fatal
		}, ticks)

	if !errors.Is(err, fatal) {
		t.Fatalf("error = %v, want the underlying failure", err)
	}
	if queries != 1 {
		t.Errorf("heartbeat probes = %d, want 1: a non-transient error must not be retried", queries)
	}
}

func TestWaitForInspectionHeartbeatStillBoundsPersistentContention(t *testing.T) {
	t.Parallel()
	busy := realBusyError(t)
	startedAt := time.Date(2026, time.July, 25, 13, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time)
	close(ticks) // every tick fires immediately, so only the deadline can stop the loop

	err := waitForInspectionHeartbeat(context.Background(), startedAt, 50*time.Millisecond,
		func(context.Context) (time.Time, error) { return time.Time{}, busy }, ticks)

	if err == nil {
		t.Fatal("persistent contention returned nil; the wait must stay bounded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a deadline-exceeded wait failure", err)
	}
}
