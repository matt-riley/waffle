-- Conversation pinning (#470): pinned conversations stay visible ahead of
-- ordinary recents without changing their last-activity ordering.
ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
