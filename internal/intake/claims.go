package intake

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Claim statuses.
const (
	StatusClaimed  = "claimed"
	StatusRunning  = "running"
	StatusReleased = "released"
)

// Claim is a persisted lease on one issue.
type Claim struct {
	Repo        string
	IssueNumber int
	Status      string
	WorkspaceID string
	SessionID   string
	ClaimedAt   time.Time
	UpdatedAt   time.Time
}

// ClaimStore persists issue leases in SQLite.
type ClaimStore struct {
	DB *sql.DB
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// TryClaim inserts a claim when the issue is free or previously released.
// Returns false when another active claim exists.
func (s *ClaimStore) TryClaim(ctx context.Context, repo string, number int) (bool, error) {
	ts := now()
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO issue_claims (repo, issue_number, status, workspace_id, session_id, claimed_at, updated_at)
		VALUES (?, ?, ?, '', '', ?, ?)
		ON CONFLICT(repo, issue_number) DO UPDATE SET
			status = excluded.status,
			workspace_id = '',
			session_id = '',
			claimed_at = excluded.claimed_at,
			updated_at = excluded.updated_at
		WHERE issue_claims.status = ?`,
		repo, number, StatusClaimed, ts, ts, StatusReleased)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkRunning records that dispatch started.
func (s *ClaimStore) MarkRunning(ctx context.Context, repo string, number int, workspaceID, sessionID string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE issue_claims SET status = ?, workspace_id = ?, session_id = ?, updated_at = ?
		WHERE repo = ? AND issue_number = ? AND status IN (?, ?)`,
		StatusRunning, workspaceID, sessionID, now(), repo, number, StatusClaimed, StatusRunning)
	return err
}

// Release frees a claim so a future tick may re-take it if the issue is still open.
func (s *ClaimStore) Release(ctx context.Context, repo string, number int) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE issue_claims SET status = ?, updated_at = ?
		WHERE repo = ? AND issue_number = ?`,
		StatusReleased, now(), repo, number)
	return err
}

// Active returns non-released claims for a repo.
func (s *ClaimStore) Active(ctx context.Context, repo string) ([]Claim, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT repo, issue_number, status, workspace_id, session_id, claimed_at, updated_at
		FROM issue_claims WHERE repo = ? AND status != ?`, repo, StatusReleased)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Claim
	for rows.Next() {
		var c Claim
		var claimed, updated string
		if err := rows.Scan(&c.Repo, &c.IssueNumber, &c.Status, &c.WorkspaceID, &c.SessionID, &claimed, &updated); err != nil {
			return nil, err
		}
		c.ClaimedAt, _ = time.Parse(time.RFC3339Nano, claimed)
		c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get loads one claim.
func (s *ClaimStore) Get(ctx context.Context, repo string, number int) (*Claim, error) {
	var c Claim
	var claimed, updated string
	err := s.DB.QueryRowContext(ctx, `
		SELECT repo, issue_number, status, workspace_id, session_id, claimed_at, updated_at
		FROM issue_claims WHERE repo = ? AND issue_number = ?`, repo, number).
		Scan(&c.Repo, &c.IssueNumber, &c.Status, &c.WorkspaceID, &c.SessionID, &claimed, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.ClaimedAt, _ = time.Parse(time.RFC3339Nano, claimed)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &c, nil
}

// RunningCount is the number of active (claimed|running) claims for a repo.
func (s *ClaimStore) RunningCount(ctx context.Context, repo string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM issue_claims
		WHERE repo = ? AND status IN (?, ?)`, repo, StatusClaimed, StatusRunning).Scan(&n)
	return n, err
}

// EnsureSchema is for tests that open a bare DB without migrations.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS issue_claims (
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			status TEXT NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			claimed_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (repo, issue_number)
		)`)
	if err != nil {
		return fmt.Errorf("ensure issue_claims: %w", err)
	}
	return nil
}
