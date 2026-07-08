package store

import (
	"bytes"
	"context"
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
	defer snap.Close() //nolint:errcheck // test teardown
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
	defer s.Close() //nolint:errcheck // read-only test teardown

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
	defer s.Close() //nolint:errcheck // test teardown

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

// TestFTSSurvivesSessionDelete exercises the 0007 sync triggers: a cascaded
// turns delete must leave no orphaned rows in the external-content FTS index.
func TestFTSSurvivesSessionDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // test teardown

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

func TestFTSSurvivesTurnUpdate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // test teardown

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
