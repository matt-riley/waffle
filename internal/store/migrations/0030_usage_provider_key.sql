-- Per-provider usage rows (#247 review): a budget key can legitimately
-- route requests to more than one upstream (the broker proxies any
-- /<connection>/ path for a session token), so one row per
-- (session_id, period, period_start) merged Anthropic and OpenAI-compatible
-- counters into a single row whose provider was the last writer. Budget
-- binding then priced every counter in the row at that provider's cache
-- multipliers, over- or under-charging the earlier provider's usage.
-- Keying the row by provider keeps each provider's counters separate so the
-- per-provider GROUP BY in the day-sum query prices each group with its own
-- cost model. Existing rows are copied as-is; their provider column is
-- already populated (0029 defaulted legacy rows to 'anthropic').
CREATE TABLE usage_new (
    session_id   TEXT NOT NULL,
    period       TEXT NOT NULL,
    period_start TEXT NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reserved_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
    provider     TEXT NOT NULL DEFAULT 'anthropic',
    PRIMARY KEY (session_id, period, period_start, provider)
) STRICT;

INSERT INTO usage_new (session_id, period, period_start, requests, input_tokens, output_tokens, reserved_tokens, cache_creation_input_tokens, cache_read_input_tokens, provider)
    SELECT session_id, period, period_start, requests, input_tokens, output_tokens, reserved_tokens, cache_creation_input_tokens, cache_read_input_tokens, provider FROM usage;

DROP TABLE usage;
ALTER TABLE usage_new RENAME TO usage;
