-- Named agent profiles (#71): bind a profile to a channel group or cron job.
ALTER TABLE channel_groups ADD COLUMN profile TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN profile TEXT NOT NULL DEFAULT '';
