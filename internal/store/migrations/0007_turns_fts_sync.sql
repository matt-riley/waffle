-- turns_fts is an external-content FTS5 index (content='turns'): it stores no
-- copy of the text, only rowids into turns, so every DELETE/UPDATE on turns
-- must be mirrored into the index or it corrupts (stale rowids -> snippet()
-- errors). 0002 shipped only the AFTER INSERT trigger on the assumption that
-- turns are insert-only, but the sessions cascade (turns.session_id REFERENCES
-- sessions(id) ON DELETE CASCADE) means the first session-delete path (see the
-- data-lifecycle work) would delete turns and skew the index. Add the standard
-- external-content sync triggers now, before any delete path exists. This
-- supersedes the "turns are insert-only" note in 0002.

CREATE TRIGGER turns_after_delete AFTER DELETE ON turns BEGIN
    INSERT INTO turns_fts (turns_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;

CREATE TRIGGER turns_after_update AFTER UPDATE ON turns BEGIN
    INSERT INTO turns_fts (turns_fts, rowid, text) VALUES ('delete', old.id, old.text);
    INSERT INTO turns_fts (rowid, text) VALUES (new.id, new.text);
END;

-- Cheap insurance: repair any skew an out-of-band edit may already have left
-- in the index before the triggers existed.
INSERT INTO turns_fts (turns_fts) VALUES ('rebuild');
