-- Mechanism-specific proposal cache (#410): candidates for a pattern are keyed
-- by evidence hash, model, prompt/schema version, existing-surface digest, and
-- prior-attempt digest, so an unchanged run makes zero provider calls.
CREATE TABLE IF NOT EXISTS learn_proposal_cache (
    cache_key   TEXT NOT NULL PRIMARY KEY,
    payload     TEXT NOT NULL,
    created_at  TEXT NOT NULL
) STRICT;
