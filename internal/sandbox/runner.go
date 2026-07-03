package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/matt-riley/waffle/internal/tool"
)

// Runner is the container side of the queue pair: it polls inbound.db for
// requests, executes them against its toolbox, and writes results to
// outbound.db. It is the process behind `waffle runner` — the same static
// waffle binary, bind-mounted into any image.
type Runner struct {
	Tools tool.Toolbox
}

// Serve processes requests from the queue in dir until a shutdown request
// arrives or ctx ends. Requests already answered in outbound.db are
// skipped, so a restarted runner resumes exactly where it left off.
func (r *Runner) Serve(ctx context.Context, dir string) error {
	out, err := openQueueDB(dir+"/"+outboundFile, outboundSchema)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // process is exiting
	// Same idempotent-schema trick as the client: whoever opens first
	// creates the file; each side only ever writes its own.
	in, err := openQueueDB(dir+"/"+inboundFile, inboundSchema)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // process is exiting

	var last int64
	if err := out.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(request_id), 0) FROM results`).Scan(&last); err != nil {
		return err
	}

	// Background heartbeat so that clients can detect whether this runner
	// is alive (or has died) *while* a long-running tool is executing.
	// The main Serve loop blocks in step/Run for the duration of e.g. bash.
	go func() {
		hb := time.NewTicker(2 * time.Second)
		defer hb.Stop()
		for {
			select {
			case <-hb.C:
				ts := time.Now().UTC().Format(time.RFC3339Nano)
				_, _ = out.ExecContext(context.Background(),
					`INSERT OR REPLACE INTO results (request_id, content, is_error, created_at)
					 VALUES (?, 'alive', 0, ?)`, runnerHealthID, ts)
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		next, stop, err := r.step(ctx, in, out, last)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		if next == last {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return nil
			}
		}
		last = next
	}
}

// step handles at most one pending request; returns the new high-water
// mark and whether a shutdown was requested.
func (r *Runner) step(ctx context.Context, in, out *sql.DB, last int64) (int64, bool, error) {
	var (
		id    int64
		name  string
		input string
	)
	err := in.QueryRowContext(ctx, `
		SELECT id, tool, input FROM requests WHERE id > ? ORDER BY id LIMIT 1`, last).
		Scan(&id, &name, &input)
	if errors.Is(err, sql.ErrNoRows) {
		return last, false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return last, true, nil
		}
		return last, false, err
	}

	if name == shutdownTool {
		return id, true, nil
	}

	content, runErr := r.Tools.Run(ctx, name, json.RawMessage(input))
	isError := 0
	if runErr != nil {
		content = "error: " + runErr.Error()
		isError = 1
	}
	// Enforce truncation inside the runner (before the outbound write) so
	// that the result row never exceeds the host limit, regardless of what
	// the tool implementation returned. This also covers non-builtin tools.
	content = tool.Truncate(content, tool.OutputLimit)
	if _, err := out.ExecContext(ctx, `
		INSERT OR IGNORE INTO results (request_id, content, is_error, created_at)
		VALUES (?, ?, ?, ?)`,
		id, content, isError, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return last, false, err
	}
	return id, false, nil
}
