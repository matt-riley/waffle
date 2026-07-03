package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

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
	for {
		var content string
		var isError bool
		err := c.outbound.QueryRowContext(ctx,
			`SELECT content, is_error FROM results WHERE request_id = ?`, id).
			Scan(&content, &isError)
		if err == nil {
			return content, isError, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, fmt.Errorf("sandbox: poll result: %w", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return "", false, fmt.Errorf("sandbox: waiting for %s: %w", name, ctx.Err())
		}
	}
}

// Shutdown asks the runner to exit after finishing in-flight work.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.inbound.ExecContext(ctx,
		`INSERT INTO requests (tool, input, created_at) VALUES (?, '{}', ?)`,
		shutdownTool, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
