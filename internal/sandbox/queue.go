// Package sandbox implements waffle's isolation layer (docs/plan.md,
// "Sandboxing & IPC"). The agent loop stays on the host; tool execution
// moves into a container. Host and container communicate through a pair of
// SQLite files on a shared mount — inbound.db (host writes, runner reads)
// and outbound.db (runner writes, host reads). One writer per file: no
// sockets, no exec-attach fragility, and results survive restarts on
// either side.
package sandbox

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver
)

const (
	inboundFile  = "inbound.db"  // exec requests: host → runner
	outboundFile = "outbound.db" // results: runner → host

	// shutdownTool is a sentinel request asking the runner to exit.
	shutdownTool = "__shutdown"

	// runnerHealthID is the sentinel (negative to avoid clashing with real
	// positive request ids) PK used for the runner's liveness heartbeat row
	// in the outbound results table. The client uses it to detect a dead or
	// missing runner without blocking for the full tool timeout.
	runnerHealthID = -1
)

// DefaultToolTimeout is the default overall per-tool timeout enforced by
// QueueToolbox and DockerExecutor (independent of or capping the caller's
// context). Combined with runner heartbeats this prevents a stuck runner
// from blocking callers for the full duration.
const DefaultToolTimeout = 11 * time.Minute

const inboundSchema = `
CREATE TABLE IF NOT EXISTS requests (
    id         INTEGER PRIMARY KEY,
    tool       TEXT NOT NULL,
    input      TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT`

const outboundSchema = `
CREATE TABLE IF NOT EXISTS results (
    request_id INTEGER PRIMARY KEY,
    content    TEXT NOT NULL,
    is_error   INTEGER NOT NULL,
    created_at TEXT NOT NULL
) STRICT`

// openQueueDB opens one side of the queue and initializes its schema when
// provided. Both client and runner pass the idempotent schema for each file,
// so either process can start first.
func openQueueDB(path, schema string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// TRUNCATE journaling instead of WAL: the queue files live on a bind
	// mount shared across a container boundary, and rollback journals are
	// the most conservative choice there. Throughput is irrelevant at
	// tool-call rates.
	dsn := "file:" + path +
		"?_pragma=journal_mode(TRUNCATE)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open queue %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if schema != "" {
		if _, err := db.Exec(schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init queue %s: %w", path, err)
		}
	}
	return db, nil
}
