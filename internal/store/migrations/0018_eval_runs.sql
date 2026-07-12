-- Eval harness run history (#63): version + timestamp + pass/fail totals.
CREATE TABLE eval_runs (
    id          INTEGER PRIMARY KEY,
    version     TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL,
    passed      INTEGER NOT NULL,
    failed      INTEGER NOT NULL,
    report      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX eval_runs_finished_at ON eval_runs(finished_at DESC);
