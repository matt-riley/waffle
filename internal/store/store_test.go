package store

import (
	"context"
	"os"
	"path/filepath"
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
