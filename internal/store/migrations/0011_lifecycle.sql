-- Lifecycle timestamps and retention configuration support.
ALTER TABLE workspaces ADD COLUMN last_active TEXT NOT NULL DEFAULT '';
UPDATE workspaces SET last_active = updated_at WHERE last_active = '';
