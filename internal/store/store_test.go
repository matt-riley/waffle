package store

import (
	"context"
	"path/filepath"
	"testing"
)

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
