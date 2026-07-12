-- Bridge migration: concurrent work landed multiple 0015_* candidates;
-- real schema for those features lives in 0016+ (profiles, learn, eval, notes).
-- This version number is retained so the embedded migration set stays contiguous.
SELECT 1;
