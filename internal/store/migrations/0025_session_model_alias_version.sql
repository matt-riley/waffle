ALTER TABLE sessions ADD COLUMN model_alias_version INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET model_alias_version = 1 WHERE model_alias <> '';
