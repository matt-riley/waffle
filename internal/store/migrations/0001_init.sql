-- meta holds instance-level facts (instance id, last-run version, ...).
-- Real domain tables (sessions, turns, memory) arrive with their phases.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
