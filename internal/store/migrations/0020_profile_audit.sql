-- Named agent profiles (#71): durable run/profile audit + workspace bind.
ALTER TABLE run_metrics ADD COLUMN profile TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN profile TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS profile_audit (
    id          INTEGER PRIMARY KEY,
    at          TEXT    NOT NULL,
    channel     TEXT    NOT NULL DEFAULT '',
    chat_id     TEXT    NOT NULL DEFAULT '',
    old_profile TEXT    NOT NULL DEFAULT '',
    new_profile TEXT    NOT NULL DEFAULT '',
    source      TEXT    NOT NULL DEFAULT 'cli'
) STRICT;
CREATE INDEX IF NOT EXISTS idx_profile_audit_at ON profile_audit(at);
