-- Scheduled jobs (phase 6): a cron expression + a prompt + a delivery
-- target. Each firing runs as a normal (sandboxed) session
-- (docs/plan.md, "Scheduling").
CREATE TABLE jobs (
    id          TEXT PRIMARY KEY,        -- job-xxxxxxxx
    name        TEXT NOT NULL,
    cron        TEXT NOT NULL,           -- 5-field cron expression
    prompt      TEXT NOT NULL,
    deliver     TEXT NOT NULL DEFAULT '', -- "channel:chat_id" or "" for log-only
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run    TEXT NOT NULL DEFAULT '',
    last_status TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
) STRICT;
