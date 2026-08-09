-- Per-workspace effective egress posture and idle timeout (#282).
--
-- Previously repo-policy tightening mutated the process-wide Manager fields,
-- so one restrictive WAFFLE.md could idle or reconfigure unrelated
-- workspaces under `waffle serve`. The effective egress and idle timeout are
-- now computed per open and stored here so resume/close restart the
-- container with the same posture (and the reaper idles each workspace on
-- its own policy) without re-reading the repo, which needs a running
-- container. Empty idle_timeout means the host default applies.
ALTER TABLE workspaces ADD COLUMN egress TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN idle_timeout TEXT NOT NULL DEFAULT '';
