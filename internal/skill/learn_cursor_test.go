package skill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// seedSessions creates n sessions, each with one recurring failure, and
// returns their ids in deterministic creation order.
func seedSessions(t *testing.T, sessions *session.Store, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := seedFailureClass(t, sessions, fmt.Sprintf("s%d", i), fmt.Sprintf("error: lost artifact %d", i), 2)
		ids = append(ids, id)
	}
	return ids
}

// TestMinePagesEveryQualifyingSessionOnce is the 75-session pagination proof:
// with a page size of 10, all 75 sessions are scanned exactly once and their
// failure classes all surface, so a busy window is never capped at 50 (#412).
func TestMinePagesEveryQualifyingSessionOnce(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "learn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	ids := seedSessions(t, sessions, 75)

	patterns, next, scanned, pages, err := MineFailurePatterns(ctx, sessions, LearnCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 75 {
		t.Fatalf("scanned = %d, want all 75", scanned)
	}
	if pages < 8 {
		t.Fatalf("pages = %d, want >= 8 for pageSize 10", pages)
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		for _, sid := range p.SessionIDs {
			seen[sid] = true
		}
	}
	if len(seen) != 75 {
		t.Fatalf("distinct sessions seen = %d, want 75 (lossless across pages)", len(seen))
	}
	// The next cursor sits exactly after the last mined session.
	if next.UpdatedAt == "" || next.SessionID != ids[len(ids)-1] {
		t.Fatalf("next cursor = %+v, want last session %s", next, ids[len(ids)-1])
	}
	// A follow-up page from the next cursor finds nothing new.
	_, next2, scanned2, _, err := MineFailurePatterns(ctx, sessions, next, 10)
	if err != nil || scanned2 != 0 {
		t.Fatalf("drain after cursor scanned=%d err=%v (cursor %+v -> %+v)", scanned2, err, next, next2)
	}
}

// TestLearnCursorBoundaryTieBreaker forces identical updated_at across many
// sessions and proves the (updated_at, id) tie-breaker scans each boundary
// row exactly once with no duplicate or skipped row (#412).
func TestLearnCursorBoundaryTieBreaker(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	ids := seedSessions(t, sessions, 25)
	// Same updated_at for every session (RFC3339Nano strings compare equal).
	same := "2026-08-14T10:00:00.000000001Z"
	for _, id := range ids {
		if _, err := st.DB.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, same, id); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := LearnCursor{}
	for {
		list, err := sessions.ListUpdatedAfter(ctx, cursor.UpdatedAt, cursor.SessionID, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			break
		}
		for _, sess := range list {
			if seen[sess.ID] {
				t.Fatalf("session %s scanned twice at cursor %+v", sess.ID, cursor)
			}
			seen[sess.ID] = true
			cursor = LearnCursor{UpdatedAt: sess.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), SessionID: sess.ID}
		}
	}
	if len(seen) != 25 {
		t.Fatalf("distinct sessions = %d, want 25 (boundary rows skipped or duplicated)", len(seen))
	}
}

// failingAttributor fails attribution so a run fails after mining.
type failingAttributor struct{}

func (failingAttributor) Complete(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	return nil, errors.New("attribution provider down")
}

// TestLearnRunFailureLeavesCursorAndMarksRunFailed proves a run that fails
// during attribution leaves the previous committed cursor unchanged, records
// an explicit failed status with an error summary, and the failed page is
// retried on the next successful run (#412).
func TestLearnRunFailureLeavesCursorAndMarksRunFailed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	ids := seedSessions(t, sessions, 6)
	ws := memory.Workspace{Dir: t.TempDir()}

	// Run 1 succeeds: cursor commits past all 6 sessions.
	l := NewLearnerFromStore(st, sessions, ws)
	res1, err := l.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res1.ScannedSessions != 6 || res1.Cursor.SessionID != ids[5] {
		t.Fatalf("run1 scanned=%d cursor=%+v", res1.ScannedSessions, res1.Cursor)
	}

	// New evidence arrives after run 1 (simulating the busy window).
	extra := seedFailureClass(t, sessions, "extra", "error: late failure burst", 2)

	// Run 2 fails during attribution: cursor must NOT advance.
	l2 := NewLearnerFromStore(st, sessions, ws)
	l2.Provider = failingAttributor{}
	l2.Model = "m"
	if _, err := l2.Run(ctx); err == nil || !strings.Contains(err.Error(), "attribution provider down") {
		t.Fatalf("run2 err = %v, want attribution failure", err)
	}
	var status, runErr string
	if err := st.DB.QueryRowContext(ctx, `SELECT status, error FROM learn_runs WHERE id = ?`, res1.ID).Scan(&status, &runErr); err != nil {
		t.Fatal(err)
	}
	if status != "finished" {
		t.Fatalf("run1 status = %q, want finished (unchanged)", status)
	}
	var failedStatus, failedErr string
	if err := st.DB.QueryRowContext(ctx, `SELECT status, error FROM learn_runs WHERE id <> ? ORDER BY started_at DESC LIMIT 1`, res1.ID).Scan(&failedStatus, &failedErr); err != nil {
		t.Fatal(err)
	}
	if failedStatus != "failed" || !strings.Contains(failedErr, "attribution provider down") {
		t.Fatalf("failed run = (%q, %q)", failedStatus, failedErr)
	}

	// Run 3 (no provider → heuristic) succeeds and retries the failed page:
	// the extra session is scanned, and only it is new since the last cursor.
	l3 := NewLearnerFromStore(st, sessions, ws)
	res3, err := l3.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res3.ScannedSessions != 1 {
		t.Fatalf("run3 scanned = %d, want exactly the retried page (1 new session)", res3.ScannedSessions)
	}
	if res3.Cursor.SessionID != extra {
		t.Fatalf("run3 cursor = %+v, want %s", res3.Cursor, extra)
	}
	// And the committed cursor still points after the last finished run.
	committed, err := LoadCommittedCursor(ctx, st.DB)
	if err != nil || committed.SessionID != extra {
		t.Fatalf("committed cursor = %+v err=%v", committed, err)
	}
}

// TestLearnRunConcurrentTriggerRefused proves concurrent /learn triggers are
// serialized: the second in-flight trigger exits clearly without advancing
// state (#412).
func TestLearnRunConcurrentTriggerRefused(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedSessions(t, sessions, 4)
	ws := memory.Workspace{Dir: t.TempDir()}

	// Insert a fresh in-progress run (as a concurrent process would).
	fresh := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO learn_runs (id, started_at, status) VALUES ('learn-concurrent', ?, 'running')`, fresh); err != nil {
		t.Fatal(err)
	}
	l := NewLearnerFromStore(st, sessions, ws)
	if _, err := l.Run(ctx); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent run err = %v, want refusal", err)
	}
	// The refused run must not have created a second running row.
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_runs WHERE status = 'running'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("running rows = %d, %v; want 1", n, err)
	}
}

// TestLearnRunReclaimsStaleInProgressRow proves a crashed run (left in
// running state by an interrupted process) is reclaimed after the stale
// window and the next run proceeds from the last committed cursor (#412).
func TestLearnRunReclaimsStaleInProgressRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedSessions(t, sessions, 4)
	ws := memory.Workspace{Dir: t.TempDir()}

	// A crashed run from an hour ago is still 'running' (no finished_at).
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO learn_runs (id, started_at, status) VALUES ('learn-crashed', '2026-08-14T08:00:00Z', 'running')`); err != nil {
		t.Fatal(err)
	}
	l := NewLearnerFromStore(st, sessions, ws)
	l.StaleRunAfter = 30 * time.Minute
	res, err := l.Run(ctx)
	if err != nil {
		t.Fatalf("run after crash err = %v (stale row must be reclaimed)", err)
	}
	if res.ScannedSessions != 4 {
		t.Fatalf("scanned = %d, want all 4 retried after the crash", res.ScannedSessions)
	}
	var status, runErr string
	if err := st.DB.QueryRowContext(ctx, `SELECT status, error FROM learn_runs WHERE id = 'learn-crashed'`).Scan(&status, &runErr); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(runErr, "interrupted") {
		t.Fatalf("crashed run = (%q, %q), want failed/interrupted", status, runErr)
	}
}

// TestLearnCursorQueryIndexBacked proves the cursor query has an index-backed
// plan as the session history grows (#412).
func TestLearnCursorQueryIndexBacked(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.DB.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT id FROM sessions
		WHERE ? = '' OR (updated_at > ? OR (updated_at = ? AND id > ?))
		ORDER BY updated_at ASC, id ASC
		LIMIT ?`, "2026-08-14T10:00:00Z", "2026-08-14T10:00:00Z", "2026-08-14T10:00:00Z", "s-1", 50)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	usedIndex := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "idx_sessions_learn_cursor") {
			usedIndex = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !usedIndex {
		t.Fatal("learn cursor query did not use idx_sessions_learn_cursor")
	}
}

// TestLearnRunInProcessSerialization proves two goroutine-triggered runs on
// the same Learner serialize (the second waits, then runs on the committed
// cursor without interleaving).
func TestLearnRunInProcessSerialization(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "serial.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	seedSessions(t, sessions, 4)
	ws := memory.Workspace{Dir: t.TempDir()}
	l := NewLearnerFromStore(st, sessions, ws)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = l.Run(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("run %d err = %v (in-process calls must serialize, not fail)", i, err)
		}
	}
	// Both runs finished; the second scanned nothing new.
	var finished int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_runs WHERE status = 'finished'`).Scan(&finished); err != nil || finished != 2 {
		t.Fatalf("finished runs = %d, %v", finished, err)
	}
	var running int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_runs WHERE status = 'running'`).Scan(&running); err != nil || running != 0 {
		t.Fatalf("running rows = %d, %v", running, err)
	}
}

// TestMigrationBackfillsLearnRunStatus proves migration 0033 marks pre-existing
// runs finished (with a cursor backfilled from finished_at) or failed — never
// left in a blocking running state — by applying the real migration against a
// hand-built legacy (pre-0033) database.
func TestMigrationBackfillsLearnRunStatus(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "w.db")

	// Build the pre-0033 schema by hand: schema_migrations records versions
	// 1..32 (so only 0033 is pending) and learn_runs keeps only legacy columns.
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyDDL := []string{
		`CREATE TABLE schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT`,
		`CREATE TABLE sessions (
			id         TEXT PRIMARY KEY,
			title      TEXT NOT NULL DEFAULT '',
			summary    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		) STRICT`,
		`CREATE TABLE learn_runs (
			id              TEXT    NOT NULL PRIMARY KEY,
			started_at      TEXT    NOT NULL,
			finished_at     TEXT    NOT NULL DEFAULT '',
			since_at        TEXT    NOT NULL DEFAULT '',
			pattern_count   INTEGER NOT NULL DEFAULT 0,
			proposal_count  INTEGER NOT NULL DEFAULT 0,
			accepted_count  INTEGER NOT NULL DEFAULT 0,
			rejected_count  INTEGER NOT NULL DEFAULT 0,
			provider_calls  INTEGER NOT NULL DEFAULT 0,
			digest          TEXT    NOT NULL DEFAULT ''
		) STRICT`,
	}
	for _, ddl := range legacyDDL {
		if _, err := legacy.Exec(ddl); err != nil {
			_ = legacy.Close()
			t.Fatalf("build legacy schema: %v", err)
		}
	}
	for v := 1; v <= 32; v++ {
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, v, fmt.Sprintf("legacy-%d", v)); err != nil {
			_ = legacy.Close()
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec(`
		INSERT INTO learn_runs (id, started_at, finished_at) VALUES
		('legacy-done', '2026-08-14T08:00:00Z', '2026-08-14T08:05:00Z'),
		('legacy-crash', '2026-08-14T09:00:00Z', '')`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open through the real migration runner: 0033 applies and backfills.
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	var status, cursor string
	if err := st2.DB.QueryRowContext(ctx, `SELECT status, cursor_updated_at FROM learn_runs WHERE id = 'legacy-done'`).Scan(&status, &cursor); err != nil {
		t.Fatal(err)
	}
	if status != "finished" || cursor != "2026-08-14T08:05:00Z" {
		t.Fatalf("legacy finished run = (%q, %q), want finished with finished_at backfill", status, cursor)
	}
	if err := st2.DB.QueryRowContext(ctx, `SELECT status FROM learn_runs WHERE id = 'legacy-crash'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("legacy crashed run status = %q, want failed", status)
	}
	var n int
	if err := st2.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_runs WHERE status = 'running'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("running rows after migration = %d, %v", n, err)
	}
}
