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
	var version int
	var name string
	if err := s.DB.QueryRowContext(ctx,
		`SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).
		Scan(&version, &name); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 1 || name != "init" {
		t.Errorf("latest migration = %d %q, want 1 \"init\"", version, name)
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
