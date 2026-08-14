-- Learn runs track held-for-review proposals separately from accepted and
-- rejected ones (#414): a proposal kept pending for owner review is not a
-- rejection, and observers need the real split.
ALTER TABLE learn_runs ADD COLUMN pending_count INTEGER NOT NULL DEFAULT 0;
