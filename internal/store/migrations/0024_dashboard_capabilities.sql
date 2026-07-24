CREATE TABLE session_skills (
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    skill_name   TEXT NOT NULL,
    attached_at  TEXT NOT NULL,
    PRIMARY KEY (session_id, skill_name)
) STRICT;

ALTER TABLE skill_status ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_status ADD COLUMN content_digest TEXT NOT NULL DEFAULT '';
