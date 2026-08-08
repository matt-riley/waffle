package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChannelOffset is the durable delivery cursor for one channel adapter (#257).
// An adapter whose cursor lives only in memory replays or loses updates across
// a restart; this row is the record of what has actually been handled.
type ChannelOffset struct {
	db      *sql.DB
	channel string
}

// NewChannelOffset binds a cursor to one channel name.
func NewChannelOffset(db *sql.DB, channel string) *ChannelOffset {
	return &ChannelOffset{db: db, channel: channel}
}

// Load returns the stored cursor, or 0 when the channel has none yet.
func (c *ChannelOffset) Load(ctx context.Context) (int64, error) {
	if c == nil || c.db == nil {
		return 0, nil
	}
	var offset int64
	err := c.db.QueryRowContext(ctx,
		`SELECT next_offset FROM channel_offsets WHERE channel = ?`, c.channel).Scan(&offset)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load %s channel offset: %w", c.channel, err)
	}
	return offset, nil
}

// Save records the cursor. Callers save only after the updates below it have
// been handled, so a crash resumes at the first unhandled update.
func (c *ChannelOffset) Save(ctx context.Context, offset int64) error {
	if c == nil || c.db == nil {
		return nil
	}
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO channel_offsets (channel, next_offset, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(channel) DO UPDATE SET next_offset = excluded.next_offset, updated_at = excluded.updated_at`,
		c.channel, offset, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save %s channel offset: %w", c.channel, err)
	}
	return nil
}
