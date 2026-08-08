-- Durable per-channel delivery cursor (#257). Telegram's getUpdates offset
-- lived only in process memory, so a restart replayed every update the
-- process had accepted but not yet confirmed. Storing it here survives the
-- restart; the row is created on first use and updated in place.
CREATE TABLE channel_offsets (
    channel     TEXT PRIMARY KEY,
    next_offset INTEGER NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;
