-- Learning loop (#65) and action-level policy audit (#66).
CREATE TABLE IF NOT EXISTS learn_runs (
    id              TEXT    NOT NULL PRIMARY KEY,
    started_at      TEXT    NOT NULL,
    finished_at     TEXT    NOT NULL DEFAULT '',
    since_at        TEXT    NOT NULL DEFAULT '',
    pattern_count   INTEGER NOT NULL DEFAULT 0,
    proposal_count  INTEGER NOT NULL DEFAULT 0,
    accepted_count  INTEGER NOT NULL DEFAULT 0,
    rejected_count  INTEGER NOT NULL DEFAULT 0,
    provider_calls  INTEGER NOT NULL DEFAULT 0,
    digest          TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE IF NOT EXISTS learn_proposals (
    id              TEXT    NOT NULL PRIMARY KEY,
    run_id          TEXT    NOT NULL,
    surface         TEXT    NOT NULL,
    pattern_sig     TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL,
    payload         TEXT    NOT NULL DEFAULT '',
    audit           TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    resolved_at     TEXT    NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX IF NOT EXISTS idx_learn_proposals_run ON learn_proposals(run_id);
CREATE INDEX IF NOT EXISTS idx_learn_proposals_status ON learn_proposals(status);

CREATE TABLE IF NOT EXISTS skill_status (
    name            TEXT    NOT NULL PRIMARY KEY,
    status          TEXT    NOT NULL,
    source          TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    activated_at    TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE IF NOT EXISTS learn_attr_cache (
    content_hash    TEXT    NOT NULL PRIMARY KEY,
    attribution     TEXT    NOT NULL,
    created_at      TEXT    NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS policy_audit (
    id              INTEGER PRIMARY KEY,
    at              TEXT    NOT NULL,
    session         TEXT    NOT NULL DEFAULT '',
    tool            TEXT    NOT NULL,
    command         TEXT    NOT NULL DEFAULT '',
    rule            TEXT    NOT NULL DEFAULT '',
    verdict         TEXT    NOT NULL,
    detail          TEXT    NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX IF NOT EXISTS idx_policy_audit_session ON policy_audit(session);
