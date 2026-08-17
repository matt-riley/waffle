-- Session artifacts (#480): files intentionally produced inside an
-- authorized session/workspace and explicitly declared by tools using opaque
-- IDs. Payloads live here as BLOBs so no host path ever enters metadata, the
-- Desk, or the transcript; serving re-verifies session ownership, the stored
-- digest, and the size cap.
CREATE TABLE artifacts (
    id         TEXT NOT NULL PRIMARY KEY,          -- opaque artifact id
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL,
    media_type TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    digest     TEXT NOT NULL DEFAULT '',           -- sha256 hex of payload
    state      TEXT NOT NULL DEFAULT 'available' CHECK (state IN ('available', 'stale', 'missing')),
    payload    BLOB,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX idx_artifacts_session ON artifacts(session_id);
