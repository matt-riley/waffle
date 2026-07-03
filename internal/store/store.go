// Package store owns waffle's SQLite database: opening it with the right
// pragmas and keeping the schema current via embedded migrations. SQLite is
// the only database waffle uses (docs/plan.md, "SQLite for everything").
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go sqlite driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the database handle.
type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations. Parent directories are created with owner-only
// permissions, since the database will hold conversation history.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: SQLite has a single writer anyway, and a single
	// conn makes transaction semantics with database/sql predictable.
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.DB.Close() }

// Snapshot writes a consistent copy of the database at src to dst using
// SQLite's `VACUUM INTO`, which folds in any WAL contents — safe to run
// against a live database from another process. It reports whether a
// snapshot was written; a missing src is not an error (ok is false), so
// callers can fall back to a fresh database. dst must not already exist.
func Snapshot(ctx context.Context, src, dst string) (ok bool, err error) {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return false, nil
	}
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return false, fmt.Errorf("open %s: %w", src, err)
	}
	defer db.Close() //nolint:errcheck // read-only handle
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return false, fmt.Errorf("snapshot %s: %w", src, err)
	}
	return true, nil
}

// migration is one embedded migrations/NNNN_name.sql file.
type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	ms := make([]migration, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		num, rest, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: want NNNN_name.sql", name)
		}
		version, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", name, err)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: version, name: rest, sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", ms[i].version)
		}
	}
	return ms, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if current.Valid && int64(m.version) <= current.Int64 {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
		m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}
