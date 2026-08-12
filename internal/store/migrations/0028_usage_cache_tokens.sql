-- Prompt caching (#247): persist the provider-reported cache token counters
-- separately from uncached input so `waffle usage` can distinguish cached
-- from uncached input and budgets can bind on true cost. Additive: rows
-- written before this migration read back with zeroed counters.
ALTER TABLE usage ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN cache_read_input_tokens INTEGER NOT NULL DEFAULT 0;
