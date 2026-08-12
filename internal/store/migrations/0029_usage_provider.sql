-- Per-row provider type (#247 review): budget binding prices cache token
-- counters with the provider's own cache multipliers, so rows must record
-- which provider produced them. Rows written before this migration (or by
-- callers that never learned the provider) price at the Anthropic model,
-- the legacy default and the only provider with an explicit cache API when
-- the column was added. Additive: pre-change rows read back as 'anthropic'.
ALTER TABLE usage ADD COLUMN provider TEXT NOT NULL DEFAULT 'anthropic';
