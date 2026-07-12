-- Session working set (#67) and tool-output spills (#69).
CREATE TABLE IF NOT EXISTS working_set_entries (
	session_id TEXT    NOT NULL,
	id         TEXT    NOT NULL,
	kind       TEXT    NOT NULL, -- goal | constraint | decision | fact | open_question | assumption
	body       TEXT    NOT NULL,
	source     TEXT    NOT NULL DEFAULT 'user', -- user | system | model
	pinned     INTEGER NOT NULL DEFAULT 0,
	created_at TEXT    NOT NULL,
	updated_at TEXT    NOT NULL,
	PRIMARY KEY (session_id, id)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_working_set_session ON working_set_entries(session_id);

CREATE TABLE IF NOT EXISTS tool_spills (
	id         TEXT    NOT NULL PRIMARY KEY,
	session_id TEXT    NOT NULL,
	tool_name  TEXT    NOT NULL DEFAULT '',
	content    TEXT    NOT NULL,
	created_at TEXT    NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_tool_spills_session ON tool_spills(session_id);

-- FTS over spills for mid-truncation recall (#69).
CREATE VIRTUAL TABLE IF NOT EXISTS tool_spills_fts USING fts5(
	content,
	content='tool_spills',
	content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS tool_spills_ai AFTER INSERT ON tool_spills BEGIN
	INSERT INTO tool_spills_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS tool_spills_ad AFTER DELETE ON tool_spills BEGIN
	INSERT INTO tool_spills_fts(tool_spills_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
