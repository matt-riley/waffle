-- Per-workspace effective egress posture (#282).
--
-- Previously repo-policy tightening mutated the process-wide Manager fields,
-- so one restrictive WAFFLE.md could idle or reconfigure unrelated
-- workspaces under `waffle serve`. The effective egress is now computed per
-- open and stored here so resume/close restart the container with the same
-- posture without re-reading the repo (which needs a running container).
ALTER TABLE workspaces ADD COLUMN egress TEXT NOT NULL DEFAULT '';
