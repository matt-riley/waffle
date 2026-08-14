-- Lossless learning cursor (#412): only a successfully finished learn run may
-- advance the durable mining cursor, and the fixed page limit is a page size,
-- not a total-window cap. learn_runs gains explicit lifecycle status so an
-- interrupted run is never indistinguishable from a healthy in-progress run.
ALTER TABLE learn_runs ADD COLUMN status TEXT NOT NULL DEFAULT 'running';
ALTER TABLE learn_runs ADD COLUMN error TEXT NOT NULL DEFAULT '';
ALTER TABLE learn_runs ADD COLUMN cursor_updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE learn_runs ADD COLUMN cursor_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE learn_runs ADD COLUMN scanned_sessions INTEGER NOT NULL DEFAULT 0;
ALTER TABLE learn_runs ADD COLUMN pages INTEGER NOT NULL DEFAULT 0;

-- Backfill conservatively: rows that finished keep their historical
-- high-water mark (finished_at) as the committed cursor; rows that never
-- finished are marked failed so they can never block or mislead a new run.
UPDATE learn_runs
SET status            = 'finished',
    cursor_updated_at = COALESCE(NULLIF(finished_at, ''), started_at)
WHERE finished_at <> '';
UPDATE learn_runs SET status = 'failed' WHERE finished_at = '';

-- Keyset pagination over (updated_at, id): the mining loop pages forward
-- deterministically even when many sessions share the same updated_at.
CREATE INDEX IF NOT EXISTS idx_sessions_learn_cursor ON sessions(updated_at, id);
