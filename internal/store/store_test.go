package store

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotMissingSource(t *testing.T) {
	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "snap.db")

	ok, err := Snapshot(ctx, filepath.Join(t.TempDir(), "nope.db"), dst)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if ok {
		t.Error("ok = true for a missing source")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("snapshot written for a missing source")
	}
}

func TestSnapshotCopiesLiveData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "waffle.db")

	// Populate a live WAL-mode DB so recent writes sit in the -wal sidecar,
	// which a raw file copy would miss.
	live, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.DB.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('probe', 'live-value')`); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "snap.db")
	ok, err := Snapshot(ctx, src, dst)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !ok {
		t.Fatal("ok = false for an existing source")
	}
	_ = live.Close()

	// The snapshot opens and carries the row written before the copy.
	snap, err := Open(ctx, dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() {
		if err := snap.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	var value string
	if err := snap.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'probe'`).Scan(&value); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if value != "live-value" {
		t.Errorf("snapshot value = %q, want live-value", value)
	}
}

func TestSnapshotRefusesExistingDest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "waffle.db")
	if s, err := Open(ctx, src); err != nil {
		t.Fatal(err)
	} else {
		_ = s.Close()
	}
	dst := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(dst, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	// VACUUM INTO requires the destination not exist; the error must surface.
	if _, err := Snapshot(ctx, src, dst); err == nil {
		t.Error("Snapshot overwrote an existing destination without error")
	}
}

func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "waffle.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	// The 0001 migration must have created meta and recorded itself.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('probe', 'ok')`); err != nil {
		t.Fatalf("meta table missing after migrate: %v", err)
	}
	var applied int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if want := len(mustLoadMigrations(t)); applied != want {
		t.Errorf("applied migrations = %d, want %d", applied, want)
	}
	// Spot-check the newest schema is usable (0002: sessions + FTS).
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at) VALUES ('s1', 'now', 'now')`); err != nil {
		t.Fatalf("sessions table missing: %v", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")

	for i := 0; i < 2; i++ {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	return ms
}

func TestLoadMigrationsSortedAndUnique(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations found")
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].version <= ms[i-1].version {
			t.Errorf("migrations out of order: %d then %d", ms[i-1].version, ms[i].version)
		}
	}
}

func TestValidateContiguous(t *testing.T) {
	mk := func(vs ...int) []migration {
		ms := make([]migration, len(vs))
		for i, v := range vs {
			ms[i] = migration{version: v, name: "x"}
		}
		return ms
	}
	if err := validateContiguous(mk(1, 2, 3, 4, 5, 6)); err != nil {
		t.Errorf("contiguous set rejected: %v", err)
	}
	if err := validateContiguous(mk(1, 2, 4)); err == nil {
		t.Error("gap {1,2,4} accepted, want error naming missing 3")
	} else if !strings.Contains(err.Error(), "3") {
		t.Errorf("gap error = %q, want it to name missing version 3", err)
	}
	if err := validateContiguous(mk(1, 2, 2, 3)); err == nil {
		t.Error("duplicate version accepted")
	}
	// A leading gap (missing 0001) must be rejected, not silently accepted.
	if err := validateContiguous(mk(2, 3, 4)); err == nil {
		t.Error("leading gap {2,3,4} accepted, want error")
	} else if !strings.Contains(err.Error(), "start at version 1") {
		t.Errorf("leading-gap error = %q, want it to mention starting at version 1", err)
	}
}

func TestPendingSkipsApplied(t *testing.T) {
	ms := []migration{{version: 1}, {version: 2}, {version: 3}, {version: 4}}
	got := pending(ms, map[int]bool{1: true, 2: true, 4: true})
	if len(got) != 1 || got[0].version != 3 {
		t.Fatalf("pending = %+v, want only version 3", got)
	}
}

// TestMigrateAppliesOutOfOrder reproduces the branch-merge hazard: a lower
// migration lands after a higher one already ran. It must still be applied and
// recorded — the old MAX(version) rule skipped it forever.
func TestMigrateAppliesOutOfOrder(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	// Simulate branch A: versions 100 and 102 applied, leaving a hole at 101.
	branchA := []migration{
		{version: 100, name: "a", sql: `CREATE TABLE t100 (x INTEGER)`},
		{version: 102, name: "c", sql: `CREATE TABLE t102 (x INTEGER)`},
	}
	if err := migrateWith(ctx, s.DB, branchA); err != nil {
		t.Fatalf("apply branch A: %v", err)
	}

	// Branch B merges later, introducing the lower-numbered 101. Capture the
	// default logger: applying a migration below the current max must emit the
	// out-of-order warning that forms the operator audit trail.
	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prevLogger)

	branchB := []migration{
		{version: 100, name: "a", sql: `CREATE TABLE t100 (x INTEGER)`},
		{version: 101, name: "b", sql: `CREATE TABLE t101 (x INTEGER)`},
		{version: 102, name: "c", sql: `CREATE TABLE t102 (x INTEGER)`},
	}
	if err := migrateWith(ctx, s.DB, branchB); err != nil {
		t.Fatalf("apply branch B: %v", err)
	}

	if out := logs.String(); !strings.Contains(out, "out of order") || !strings.Contains(out, "version=101") {
		t.Errorf("out-of-order warning not emitted for 101; logs:\n%s", out)
	}

	// 101 must now be recorded and its table must exist.
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 101`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("version 101 recorded %d times, want 1", n)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO t101 (x) VALUES (1)`); err != nil {
		t.Errorf("out-of-order migration 101 not applied: %v", err)
	}

	// Re-running is a no-op (idempotent).
	if err := migrateWith(ctx, s.DB, branchB); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

// TestApplyOneRollsBackFailingMigration proves both halves of the migration
// transaction roll back: neither schema objects nor the bookkeeping row may
// survive a statement failure.
func TestApplyOneRollsBackFailingMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	err = applyOne(ctx, db, migration{version: 77, name: "must_rollback", sql: `
		CREATE TABLE should_rollback (id INTEGER PRIMARY KEY);
		INSERT INTO table_that_does_not_exist (id) VALUES (1);`})
	if err == nil {
		t.Fatal("applyOne succeeded for a failing migration")
	}
	var objects int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'should_rollback'`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if objects != 0 {
		t.Fatalf("failing migration left %d schema objects, want 0", objects)
	}
	var records int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 77`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("failing migration left %d migration records, want 0", records)
	}
}

// TestFTSSurvivesSessionDelete exercises the 0007 sync triggers: a cascaded
// turns delete must leave no orphaned rows in the external-content FTS index.
func TestFTSSurvivesSessionDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at) VALUES ('s1', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		 VALUES ('s1', 0, 'user', '[]', 'alpaca picnic', 'now')`); err != nil {
		t.Fatal(err)
	}
	// The term is findable before deletion.
	var before int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH 'alpaca'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("pre-delete FTS match = %d, want 1", before)
	}

	// Cascade the delete through sessions.
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}

	var after int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH 'alpaca'`).Scan(&after); err != nil {
		t.Fatalf("FTS query after delete (index likely corrupt): %v", err)
	}
	if after != 0 {
		t.Errorf("post-delete FTS match = %d, want 0 (orphaned index row)", after)
	}
	// FTS5 integrity check must pass for an external-content table.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO turns_fts (turns_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		t.Errorf("FTS integrity check failed: %v", err)
	}
}

// TestMemoryNotesMigrationOnPopulatedDB applies 0019 over a DB that already
// has sessions/turns from earlier schema, then verifies the new table works (#60).
func TestMemoryNotesMigrationOnPopulatedDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")

	// Open once so all current migrations apply (including 0019), then drop
	// the memory_notes objects and un-record 0019 to simulate a pre-change DB
	// that already holds live conversation data.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (id, title, created_at, updated_at)
		VALUES ('pre', 'pre-change', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		VALUES ('pre', 1, 'user', '[]', 'docker networking turn pre-migration', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Tear down 0019 as if it had never run.
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS memory_notes_ai`,
		`DROP TRIGGER IF EXISTS memory_notes_ad`,
		`DROP TRIGGER IF EXISTS memory_notes_au`,
		`DROP TABLE IF EXISTS memory_notes_fts`,
		`DROP TABLE IF EXISTS memory_notes`,
		`DELETE FROM schema_migrations WHERE version = 19`,
	} {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open applies pending 0019 against the populated DB.
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen after 0019: %v", err)
	}
	defer func() {
		if err := s2.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	// Pre-existing turn still searchable.
	var n int
	if err := s2.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH 'docker'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("turns_fts hits = %d, want 1", n)
	}
	// New table is usable.
	if _, err := s2.DB.ExecContext(ctx, `
		INSERT INTO memory_notes (id, agent, body, raw_line, archived, pinned, note_date, created_at, updated_at)
		VALUES ('n1', 'main', 'docker networking fact', '- note', 0, 0, '2026-07-01', '2026-07-01T00:00:00Z', '2026-07-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert memory_notes: %v", err)
	}
	if err := s2.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_notes_fts WHERE memory_notes_fts MATCH 'networking'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("memory_notes_fts hits = %d, want 1", n)
	}
}

func TestFTSSurvivesTurnUpdate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at) VALUES ('s1', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		 VALUES ('s1', 0, 'user', '[]', 'original text', 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE turns SET text = 'replacement text' WHERE session_id = 's1' AND seq = 0`); err != nil {
		t.Fatal(err)
	}
	var oldHits, newHits int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH 'original'`).Scan(&oldHits); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH 'replacement'`).Scan(&newHits); err != nil {
		t.Fatal(err)
	}
	if oldHits != 0 {
		t.Errorf("stale term still indexed after update: %d hits", oldHits)
	}
	if newHits != 1 {
		t.Errorf("new term not indexed after update: %d hits", newHits)
	}
}
