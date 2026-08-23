package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/matt-riley/waffle/internal/tool"
)

// defaultRunnerHeartbeatInterval is how often a runner refreshes its liveness
// heartbeat row. It bounds how quickly a host-side inspection can notice a
// restarted runner (and how quickly dead-runner detection fires).
const defaultRunnerHeartbeatInterval = 2 * time.Second

// defaultRunnerPollInterval is how often the runner checks inbound.db for a
// pending request. It bounds one queue round trip (request → result).
const defaultRunnerPollInterval = 100 * time.Millisecond

// Runner is the container side of the queue pair: it polls inbound.db for
// requests, executes them against its toolbox, and writes results to
// outbound.db. It is the process behind `waffle runner` — the same static
// waffle binary, bind-mounted into any image.
type Runner struct {
	Tools tool.Toolbox
	// HeartbeatInterval is how often the liveness heartbeat row is refreshed.
	// Zero uses defaultRunnerHeartbeatInterval; tests shrink it to avoid
	// wall-clock waits (same pattern as Client's detection-window fields).
	HeartbeatInterval time.Duration
	// PollInterval is how often inbound.db is checked for a pending request.
	// Zero uses defaultRunnerPollInterval; tests shrink it alongside
	// HeartbeatInterval so queue round trips stay fast.
	PollInterval time.Duration
}

// Serve processes requests from the queue in dir until a shutdown request
// arrives or ctx ends. Requests already answered in outbound.db are
// skipped, so a restarted runner resumes exactly where it left off.
func (r *Runner) Serve(ctx context.Context, dir string) (err error) {
	out, err := openQueueDB(dir+"/"+outboundFile, outboundSchema)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	// Same idempotent-schema trick as the client: whoever opens first
	// creates the file; each side only ever writes its own.
	in, err := openQueueDB(dir+"/"+inboundFile, inboundSchema)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); err == nil {
			err = cerr
		}
	}()

	var last int64
	if err := out.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(request_id), 0) FROM results`).Scan(&last); err != nil {
		return err
	}

	// Heartbeat goroutine tied to a derived ctx canceled on any Serve return
	// (including early errors), so it exits before DB close even if caller ctx
	// is still alive.
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	go func() {
		interval := r.HeartbeatInterval
		if interval <= 0 {
			interval = defaultRunnerHeartbeatInterval
		}
		hb := time.NewTicker(interval)
		defer hb.Stop()
		for {
			select {
			case <-hb.C:
				ts := time.Now().UTC().Format(time.RFC3339Nano)
				_, _ = out.ExecContext(serveCtx,
					`INSERT OR REPLACE INTO results (request_id, content, is_error, created_at)
					 VALUES (?, 'alive', 0, ?)`, runnerHealthID, ts)
			case <-serveCtx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()
	// Docker Desktop's VirtioFS can pin a long-lived fd to a stale inode
	// after the host-side client writes: fresh opens see the current data
	// while the old fd keeps serving orphaned pages, stalling the queue for
	// a minute or more under load. Reopening the inbound handle after a
	// short idle streak re-resolves the path and caps such a stall at about
	// a second; the reopen costs the same class of work as the poll itself.
	const idlePollsPerReopen = 10
	idlePolls := 0
	for {
		next, stop, err := r.step(ctx, in, out, last)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		if next == last {
			idlePolls++
			if idlePolls >= idlePollsPerReopen {
				idlePolls = 0
				if fresh, err := openQueueDB(dir+"/"+inboundFile, ""); err == nil {
					_ = in.Close()
					in = fresh
				}
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return nil
			}
		} else {
			idlePolls = 0
		}
		last = next
	}
}

// pollInterval returns the inbound.poll cadence, defaulted when unset.
func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return defaultRunnerPollInterval
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
	// that the result row never exceeds the host OutputLimit, regardless of
	// what the tool implementation returned. This also covers non-builtin tools.
	//
	// Sandbox limitation (#69): truncating here means Agent.runOne never sees
	// the full host-side payload and cannot spill oversized sandbox tool
	// output. expand_output / tool_spills only cover host-executed (and MCP)
	// results that return up to HostReturnCap without OutputLimit truncation.
	// Full in-container spill would require a larger outbound row or a side
	// channel; not implemented.
	content = tool.Truncate(content, tool.OutputLimit)
	if _, err := out.ExecContext(ctx, `
		INSERT OR IGNORE INTO results (request_id, content, is_error, created_at)
		VALUES (?, ?, ?, ?)`,
		id, content, isError, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return last, false, err
	}
	return id, false, nil
}
