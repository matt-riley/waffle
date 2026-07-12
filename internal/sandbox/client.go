package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// runnerNoHealthWait is the minimum time one Exec call waits for a first
// runner heartbeat before it may conclude the runner is missing/dead
// (avoids blocking full timeout).
const runnerNoHealthWait = 10 * time.Second

// runnerStartupWait is the cold-start allowance for the first-ever runner
// heartbeat, measured from client creation (for Docker sandboxes the client
// is created right after `docker run`, so this approximates container
// start). A cold container can take well over runnerNoHealthWait to boot
// the in-container runner, so a first tool call must not declare it dead on
// an Exec-relative window alone; a live runner heartbeats within seconds of
// booting, so no heartbeat by this bound means it is genuinely missing.
const runnerStartupWait = 60 * time.Second

// runnerHealthGrace is the staleness threshold for the runner heartbeat after
// which we declare it dead (a live runner heartbeats in bg even during long
// tool execution).
const runnerHealthGrace = 30 * time.Second

// runnerProbeTimeout caps one lastHealth query. It sits above the queue's
// busy_timeout (5s, see openQueueDB) so that a probe issued under lock
// contention on the shared mount still gets a genuine answer instead of
// always timing out — a probe failure says nothing about runner liveness.
const runnerProbeTimeout = 6 * time.Second

// Client is the host side of the queue pair: it writes exec requests to
// inbound.db and polls outbound.db for results.
type Client struct {
	inbound  *sql.DB // writer
	outbound *sql.DB // reader

	// startedAt anchors the cold-start allowance for the first heartbeat;
	// set when the client is created (≈ container start for Docker).
	startedAt time.Time

	// Detection windows, defaulted from the constants above in NewClient;
	// fields so tests can shrink them and stay fast.
	noHealthWait    time.Duration // min per-Exec wait before "no heartbeat" counts
	startupWait     time.Duration // cold-start allowance from startedAt for the first heartbeat
	healthGrace     time.Duration // heartbeat staleness threshold
	probeTimeout    time.Duration // per-lastHealth-query cap
	probeFailWindow time.Duration // consecutive probe-failure budget before presumed dead
	// OnActivity is called when a request is queued or a result is received.
	// It is optional and must not block the queue round trip.
	OnActivity func()
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
	return &Client{
		inbound:         in,
		outbound:        out,
		startedAt:       time.Now(),
		noHealthWait:    runnerNoHealthWait,
		startupWait:     runnerStartupWait,
		healthGrace:     runnerHealthGrace,
		probeTimeout:    runnerProbeTimeout,
		probeFailWindow: runnerHealthGrace,
	}, nil
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
func (c *Client) Exec(ctx context.Context, useIDOrName string, args ...interface{}) (string, bool, error) {
	var useID, name string
	var input json.RawMessage
	asJSON := func(v interface{}) (json.RawMessage, bool) {
		switch x := v.(type) {
		case json.RawMessage:
			return x, true
		case []byte:
			return json.RawMessage(x), true
		case string:
			return json.RawMessage(x), true
		default:
			return nil, false
		}
	}
	if len(args) == 1 {
		var ok bool
		input, ok = asJSON(args[0])
		if !ok {
			return "", false, fmt.Errorf("sandbox: Exec input must be JSON")
		}
		name = useIDOrName
	} else if len(args) == 2 {
		var ok bool
		useID = useIDOrName
		name, ok = args[0].(string)
		if !ok {
			return "", false, fmt.Errorf("sandbox: Exec name must be string")
		}
		input, ok = asJSON(args[1])
		if !ok {
			return "", false, fmt.Errorf("sandbox: Exec input must be JSON")
		}
	} else {
		return "", false, fmt.Errorf("sandbox: Exec expects name,input or useID,name,input")
	}
	var (
		res sql.Result
		err error
	)
	if useID == "" {
		res, err = c.inbound.ExecContext(ctx,
			`INSERT INTO requests (tool, input, created_at) VALUES (?, ?, ?)`,
			name, string(input), time.Now().UTC().Format(time.RFC3339Nano))
	} else {
		res, err = c.inbound.ExecContext(ctx,
			`INSERT OR IGNORE INTO requests (tool_use_id, tool, input, created_at) VALUES (?, ?, ?, ?)`,
			useID, name, string(input), time.Now().UTC().Format(time.RFC3339Nano))
	}
	if c.OnActivity != nil {
		c.OnActivity()
	}
	if err != nil {
		return "", false, fmt.Errorf("sandbox: enqueue: %w", err)
	}
	var id int64
	if useID == "" {
		id, err = res.LastInsertId()
	} else {
		err = c.inbound.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ?`, useID).Scan(&id)
	}
	if err != nil {
		return "", false, err
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	var probeFailSince time.Time // start of the current consecutive-probe-failure streak
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
			if c.OnActivity != nil {
				c.OnActivity()
			}
			return content, isError, nil
		}
		if !errors.Is(qerr, sql.ErrNoRows) && !errors.Is(qerr, context.DeadlineExceeded) && !errors.Is(qerr, context.Canceled) && !isBusyErr(qerr) {
			return "", false, fmt.Errorf("sandbox: poll result: %w", qerr)
		}
		select {
		case <-ticker.C:
			// Heartbeat-based dead-runner detection (independent of ctx).
			h, herr := c.lastHealth(ctx)
			switch {
			case herr != nil:
				// The probe itself failed — typically lock contention on
				// the shared mount, which says nothing about runner
				// liveness. Don't let that disable detection: past a
				// budget of consecutive failures, presume the runner dead
				// instead of blocking until the full tool timeout.
				if probeFailSince.IsZero() {
					probeFailSince = time.Now()
				} else if down := time.Since(probeFailSince); down > c.probeFailWindow {
					c.attemptShutdown(ctx)
					return "", false, fmt.Errorf("sandbox: waiting for %s: runner appears dead (health probe failing for %s: %v)", name, down.Round(time.Second), herr)
				}
			case !h.IsZero():
				probeFailSince = time.Time{}
				if stale := time.Since(h); stale > c.healthGrace {
					c.attemptShutdown(ctx)
					return "", false, fmt.Errorf("sandbox: waiting for %s: runner appears dead (no heartbeat for %s)", name, stale.Round(time.Second))
				}
			default:
				// No heartbeat yet. A cold container may still be booting
				// the runner, so the window is anchored to container/runner
				// start (startedAt), not just this Exec call: declare the
				// runner missing only once both the per-call minimum and
				// the overall cold-start allowance have passed.
				probeFailSince = time.Time{}
				if time.Since(start) > c.noHealthWait && time.Since(c.startedAt) > c.startupWait {
					c.attemptShutdown(ctx)
					return "", false, fmt.Errorf("sandbox: waiting for %s: runner appears dead (no runner heartbeat seen %s after start)", name, time.Since(c.startedAt).Round(time.Second))
				}
			}
		case <-ctx.Done():
			c.attemptShutdown(ctx)
			return "", false, fmt.Errorf("sandbox: waiting for %s: %w (runner may be stuck or dead)", name, ctx.Err())
		}
	}
}

// Reclaim returns durable results for caller identities completed while the
// host was offline.
func (c *Client) Reclaim(ctx context.Context, useIDs []string) (map[string]llm.ToolResult, error) {
	out := make(map[string]llm.ToolResult)
	for _, useID := range useIDs {
		var result llm.ToolResult
		var isError bool
		var id int64
		err := c.inbound.QueryRowContext(ctx, `SELECT id FROM requests WHERE tool_use_id = ?`, useID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sandbox: reclaim %s: %w", useID, err)
		}
		err = c.outbound.QueryRowContext(ctx,
			`SELECT content, is_error FROM results WHERE request_id = ?`, id).
			Scan(&result.Content, &isError)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sandbox: reclaim %s: %w", useID, err)
		}
		result.ToolUseID, result.IsError = useID, isError
		out[useID] = result
	}
	return out, nil
}

// isBusyErr reports whether err is SQLite's transient SQLITE_BUSY
// ("database is locked"), which lock contention on the shared mount can
// surface instead of a context deadline. It is a retryable condition, not
// a transport failure; the heartbeat detector decides whether to give up.
func isBusyErr(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// lastHealth returns the timestamp of the most recent runner heartbeat row
// (or zero time if none present). Its internal timeout (probeTimeout) is
// deliberately above the queue busy_timeout so contention usually yields a
// genuine answer; persistent failures are handled by the caller's
// probe-failure budget rather than by skipping detection.
func (c *Client) lastHealth(ctx context.Context) (time.Time, error) {
	hctx, cancel := context.WithTimeout(ctx, c.probeTimeout)
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
