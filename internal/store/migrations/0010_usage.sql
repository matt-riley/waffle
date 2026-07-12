CREATE TABLE usage (
    session_id   TEXT NOT NULL,
    period       TEXT NOT NULL,
    period_start TEXT NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, period, period_start)
) STRICT;

CREATE TABLE runtime_flags (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
