-- Repo workspaces (phase 5): a container + named volume dedicated to one
-- repository, bound to a session (docs/plan.md, "Repo workspaces").
CREATE TABLE workspaces (
    id         TEXT PRIMARY KEY,          -- ws-xxxxxxxx
    repo       TEXT NOT NULL,             -- owner/name
    url        TEXT NOT NULL,             -- clone URL
    image      TEXT NOT NULL,
    container  TEXT NOT NULL,
    volume     TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    status     TEXT NOT NULL CHECK (status IN ('open', 'idle', 'closed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Partial unique index to prevent duplicate active workspaces for the same repo.
-- Applied at migration time for new DBs; ensureActiveRepoIndex is fallback for old DBs.
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_repo_active
ON workspaces(repo) WHERE status != 'closed';
