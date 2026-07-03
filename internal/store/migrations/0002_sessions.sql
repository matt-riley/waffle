-- Conversation persistence (phase 2). Every turn is stored and indexed for
-- full-text recall; summaries are written by the reflection pass.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL DEFAULT '',
    summary    TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE turns (
    id         INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    role       TEXT NOT NULL,
    blocks     TEXT NOT NULL, -- JSON-encoded []llm.Block
    text       TEXT NOT NULL, -- extracted plain text, what gets indexed
    created_at TEXT NOT NULL,
    UNIQUE (session_id, seq)
) STRICT;

CREATE VIRTUAL TABLE turns_fts USING fts5(text, content='turns', content_rowid='id');

-- turns are insert-only, so one trigger keeps the index current.
CREATE TRIGGER turns_after_insert AFTER INSERT ON turns BEGIN
    INSERT INTO turns_fts (rowid, text) VALUES (new.id, new.text);
END;
