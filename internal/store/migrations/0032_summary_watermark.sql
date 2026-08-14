-- Summary watermark (#411): a session is eligible for idle reflection when
-- its latest turn sequence is greater than summary_watermark, even if a
-- previous summary exists. reflected_at records when the summary was written
-- and never drives idle timing (updated_at stays conversation activity).
ALTER TABLE sessions ADD COLUMN summary_watermark INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN reflected_at TEXT NOT NULL DEFAULT '';

-- Preserve existing summaries conservatively: an existing summary is treated
-- as covering every turn that exists today, so already-summarized sessions
-- are not re-reflected after the upgrade. Only sessions summarized by the
-- older model get a watermark; empty summaries stay at 0 (still eligible).
UPDATE sessions
SET summary_watermark = COALESCE((SELECT MAX(seq) FROM turns WHERE turns.session_id = sessions.id), 0),
    reflected_at     = updated_at
WHERE summary <> '';
