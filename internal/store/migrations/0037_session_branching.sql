-- Conversation branching (#471): a session may record that it forked from
-- another session at a specific completed exchange. The source session is
-- never modified; forked_from/forked_at_seq are durable lineage so Desk can
-- show provenance without a visual branch tree.
ALTER TABLE sessions ADD COLUMN forked_from TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN forked_at_seq INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_sessions_forked_from ON sessions(forked_from) WHERE forked_from != '';
