-- MEMORY.md / MEMORY.archive.md FTS index for multi-tier recall (#60).
-- Notes are synced from workspace files on remember / memory_update / append;
-- archive rows stay searchable with archived=1.
CREATE TABLE IF NOT EXISTS memory_notes (
	id         TEXT    NOT NULL PRIMARY KEY,
	agent      TEXT    NOT NULL DEFAULT 'main',
	body       TEXT    NOT NULL,
	raw_line   TEXT    NOT NULL,
	archived   INTEGER NOT NULL DEFAULT 0,
	pinned     INTEGER NOT NULL DEFAULT 0,
	note_date  TEXT    NOT NULL DEFAULT '',
	created_at TEXT    NOT NULL,
	updated_at TEXT    NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_memory_notes_agent ON memory_notes(agent);
CREATE INDEX IF NOT EXISTS idx_memory_notes_archived ON memory_notes(archived);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_notes_fts USING fts5(
	body,
	raw_line,
	content='memory_notes',
	content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS memory_notes_ai AFTER INSERT ON memory_notes BEGIN
	INSERT INTO memory_notes_fts(rowid, body, raw_line) VALUES (new.rowid, new.body, new.raw_line);
END;
CREATE TRIGGER IF NOT EXISTS memory_notes_ad AFTER DELETE ON memory_notes BEGIN
	INSERT INTO memory_notes_fts(memory_notes_fts, rowid, body, raw_line)
		VALUES ('delete', old.rowid, old.body, old.raw_line);
END;
CREATE TRIGGER IF NOT EXISTS memory_notes_au AFTER UPDATE ON memory_notes BEGIN
	INSERT INTO memory_notes_fts(memory_notes_fts, rowid, body, raw_line)
		VALUES ('delete', old.rowid, old.body, old.raw_line);
	INSERT INTO memory_notes_fts(rowid, body, raw_line) VALUES (new.rowid, new.body, new.raw_line);
END;
