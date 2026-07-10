-- Run-level usage and timing metrics for the local observability snapshot.
-- Active runs stay in memory; rows are written when they complete.
CREATE TABLE run_metrics (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL,
    source        TEXT NOT NULL,
    phase         TEXT NOT NULL,
    outcome       TEXT NOT NULL,
    started_at_ms INTEGER NOT NULL,
    ended_at_ms   INTEGER NOT NULL,
    input_tokens  INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_run_metrics_ended_at ON run_metrics(ended_at_ms DESC);
