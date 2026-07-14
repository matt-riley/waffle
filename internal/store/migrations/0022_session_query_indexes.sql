-- Keep bounded session reads and lifecycle cleanup from scanning every session.
-- The partial index is tailored to the idle-reflection candidate query: only
-- sessions without a summary can be considered.
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_reflection_candidates
ON sessions(updated_at ASC) WHERE summary = '';

-- Foreign-key child lookups and lifecycle deletes filter by session_id.
CREATE INDEX IF NOT EXISTS idx_channel_groups_session ON channel_groups(session_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_session ON workspaces(session_id);
