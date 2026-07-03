package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// runnerNoHealthWait is how long Exec will wait for a first runner heartbeat
// before concluding the runner is missing/dead (avoids blocking full timeout).
const runnerNoHealthWait = 10 * time.Second

// runnerHealthGrace is the staleness threshold for the runner heartbeat after
// which we declare it dead (a live runner heartbeats in bg even during long
// tool execution).
const runnerHealthGrace = 30 * time.Second

// Client is the host side of the queue pair: it writes exec requests to
// inbound.db and polls outbound.db for results.
type Client struct {
	inbound  *sql.DB // writer
	outbound *sql.DB // reader
}

// NewClient opens the host side of the queue in dir, creating inbound.db.
func NewClient(dir string) (*Client, error) {
	in, err := openQueueDB(dir+"/"+inboundFile, inboundSchema)
	if err != nil {
		return nil, err
	}
	// The runner creates outbound.db; opening with schema here too makes
	// startup order irrelevant (CREATE IF NOT EXISTS is idempotent and
	// happens before either side writes).
	out, err := openQueueDB(dir+"/"+outboundFile, outboundSchema)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	return &Client{inbound: in, outbound: out}, nil
}

// Close releases the queue handles (the runner is told to stop separately;
// see Shutdown).
func (c *Client) Close() error {
	err1 := c.inbound.Close()
	err2 := c.outbound.Close()
	return errors.Join(err1, err2)
}

// Exec sends one tool request and waits for its result.
// It uses a per-poll short deadline for the query itself and, via runner
// heartbeats, detects a stuck or missing runner and returns early with a
// clear "runner appears dead" error (instead of blocking until the caller's
// ctx or the 11m tool timeout expires).
func (c *Client) Exec(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
	res, err := c.inbound.ExecContext(ctx,
		`INSERT INTO requests (tool, input, created_at) VALUES (?, ?, ?)`,
		name, string(input), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", false, fmt.Errorf("sandbox: enqueue: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", false, err
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		// Use a short per-poll deadline so a wedged query (rare, e.g. under
		// lock contention on the shared mount) does not block the select.
		pollCtx, pollCancel := context.WithTimeout(ctx, 2*time.Second)
		var content string
		var isError bool
		qerr := c.outbound.QueryRowContext(pollCtx,
			`SELECT content, is_error FROM results WHERE request_id = ?`, id).
			Scan(&content, &isError)
		pollCancel()
		if qerr == nil {
			return content, isError, nil
		}
		if !errors.Is(qerr, sql.ErrNoRows) && !errors.Is(qerr, context.DeadlineExceeded) && !errors.Is(qerr, context.Canceled) {
			return "", false, fmt.Errorf("sandbox: poll result: %w", qerr)
		}
		select {
		case <-ticker.C:
			// Heartbeat-based dead-runner detection (independent of ctx).
			waited := time.Since(start)
			h, herr := c.lastHealth(ctx)
			if herr == nil {
				if !h.IsZero() {
					if stale := time.Since(h); stale > runnerHealthGrace {
						c.attemptShutdown(ctx)
						return "", false, fmt.Errorf("sandbox: waiting for %s: runner appears dead (no heartbeat for %s)", name, stale.Round(time.Second))
					}
				} else if waited > runnerNoHealthWait {
					c.attemptShutdown(ctx)
					return "", false, fmt.Errorf("sandbox: waiting for %s: runner appears dead (no runner heartbeat seen)", name)
				}
			}
		case <-ctx.Done():
			c.attemptShutdown(ctx)
			return "", false, fmt.Errorf("sandbox: waiting for %s: %w (runner may be stuck or dead)", name, ctx.Err())
		}
	}
}

// lastHealth returns the timestamp of the most recent runner heartbeat row
// (or zero time if none present). It uses a short internal timeout to avoid
// blocking on SQLite locks beyond the per-poll deadline.
func (c *Client) lastHealth(ctx context.Context) (time.Time, error) {
	hctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var ts string
	err := c.outbound.QueryRowContext(hctx,
		`SELECT created_at FROM results WHERE request_id = ?`, runnerHealthID).Scan(&ts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	t, perr := time.Parse(time.RFC3339Nano, ts)
	if perr != nil {
		return time.Time{}, nil // malformed ts treated as absent
	}
	return t, nil
}

// attemptShutdown tries to tell a possibly-alive runner to exit; best-effort.
// Uses background so it runs even if caller ctx is already done/canceled.
func (c *Client) attemptShutdown(ctx context.Context) {
	// ignore passed ctx; use fresh background for best-effort
	hctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = c.Shutdown(hctx)
}

// Shutdown asks the runner to exit after finishing in-flight work.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.inbound.ExecContext(ctx,
		`INSERT INTO requests (tool, input, created_at) VALUES (?, '{}', ?)`,
		shutdownTool, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
