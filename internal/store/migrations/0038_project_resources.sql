-- Workspace-scoped project context (#478): a reusable library of pinned
-- workspace file references and explicit owner notes. Resources never cross
-- workspace boundaries; attachments bind one resource to one session and
-- enter the session's bounded working set with provenance.
CREATE TABLE project_resources (
    id          TEXT NOT NULL PRIMARY KEY,          -- pr-xxxxxxxx
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('file', 'note')),
    name        TEXT NOT NULL,
    path        TEXT NOT NULL DEFAULT '',           -- safe repo-relative path for files
    note        TEXT NOT NULL DEFAULT '',           -- body for notes
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    digest      TEXT NOT NULL DEFAULT '',           -- sha256 hex of file content
    state       TEXT NOT NULL DEFAULT 'available' CHECK (state IN ('available', 'stale', 'missing')),
    provenance  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;
CREATE INDEX idx_project_resources_workspace ON project_resources(workspace_id);

CREATE TABLE project_attachments (
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    resource_id TEXT NOT NULL REFERENCES project_resources(id) ON DELETE CASCADE,
    attached_at TEXT NOT NULL,
    PRIMARY KEY (session_id, resource_id)
) STRICT;
CREATE INDEX idx_project_attachments_session ON project_attachments(session_id);
