-- Subagent packet/handoff persistence (#78) and multi-tier recall indexes (#60).
CREATE TABLE IF NOT EXISTS subagent_handoffs (
	id              TEXT    NOT NULL PRIMARY KEY,
	parent_session  TEXT    NOT NULL,
	child_session   TEXT    NOT NULL,
	packet_json     TEXT    NOT NULL,
	handoff_json    TEXT    NOT NULL,
	created_at      TEXT    NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_subagent_handoffs_parent ON subagent_handoffs(parent_session);

-- Session summary FTS for multi-tier recall (#60).
CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
	summary,
	content='sessions',
	content_rowid='rowid'
);
INSERT INTO sessions_fts(rowid, summary)
	SELECT rowid, summary FROM sessions WHERE summary != '';
CREATE TRIGGER IF NOT EXISTS sessions_ai AFTER INSERT ON sessions BEGIN
	INSERT INTO sessions_fts(rowid, summary) VALUES (new.rowid, new.summary);
END;
CREATE TRIGGER IF NOT EXISTS sessions_ad AFTER DELETE ON sessions BEGIN
	INSERT INTO sessions_fts(sessions_fts, rowid, summary) VALUES ('delete', old.rowid, old.summary);
END;
CREATE TRIGGER IF NOT EXISTS sessions_au AFTER UPDATE OF summary ON sessions BEGIN
	INSERT INTO sessions_fts(sessions_fts, rowid, summary) VALUES ('delete', old.rowid, old.summary);
	INSERT INTO sessions_fts(rowid, summary) VALUES (new.rowid, new.summary);
END;

-- Durable hook output logs (#54).
CREATE TABLE IF NOT EXISTS hook_logs (
	id           TEXT    NOT NULL PRIMARY KEY,
	workspace_id TEXT    NOT NULL DEFAULT '',
	session_id   TEXT    NOT NULL DEFAULT '',
	point        TEXT    NOT NULL,
	output       TEXT    NOT NULL,
	error        TEXT    NOT NULL DEFAULT '',
	created_at   TEXT    NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_hook_logs_session ON hook_logs(session_id);
