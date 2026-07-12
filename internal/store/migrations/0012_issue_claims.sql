-- Issue-tracker intake claims (board-driven work; issue #51).
CREATE TABLE IF NOT EXISTS issue_claims (
	repo         TEXT    NOT NULL,
	issue_number INTEGER NOT NULL,
	status       TEXT    NOT NULL, -- claimed | running | released
	workspace_id TEXT    NOT NULL DEFAULT '',
	session_id   TEXT    NOT NULL DEFAULT '',
	claimed_at   TEXT    NOT NULL,
	updated_at   TEXT    NOT NULL,
	PRIMARY KEY (repo, issue_number)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_issue_claims_status ON issue_claims(status);
