package skill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Attachments persists the skills explicitly attached to a session.
type Attachments struct {
	DB *sql.DB
}

// Attach idempotently associates a skill name with a session.
func (a *Attachments) Attach(ctx context.Context, sessionID, name string) error {
	if err := a.validate(sessionID, name); err != nil {
		return err
	}
	_, err := a.DB.ExecContext(ctx, `
		INSERT INTO session_skills (session_id, skill_name, attached_at)
		VALUES (?, ?, ?)
		ON CONFLICT(session_id, skill_name) DO NOTHING`,
		sessionID, name, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("attach skill to session: %w", err)
	}
	return nil
}

// Detach idempotently removes a skill association from a session.
func (a *Attachments) Detach(ctx context.Context, sessionID, name string) error {
	if err := a.validate(sessionID, name); err != nil {
		return err
	}
	if _, err := a.DB.ExecContext(ctx, `
		DELETE FROM session_skills
		WHERE session_id = ? AND skill_name = ?`, sessionID, name); err != nil {
		return fmt.Errorf("detach skill from session: %w", err)
	}
	return nil
}

// List returns attached skill names in deterministic name order.
func (a *Attachments) List(ctx context.Context, sessionID string) (out []string, err error) {
	if a == nil || a.DB == nil {
		return nil, errors.New("attachments database required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session ID required")
	}
	out = make([]string, 0)
	rows, err := a.DB.QueryContext(ctx, `
		SELECT skill_name
		FROM session_skills
		WHERE session_id = ?
		ORDER BY skill_name ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list session skills: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan session skill: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session skills: %w", err)
	}
	return out, nil
}

func (a *Attachments) validate(sessionID, name string) error {
	if a == nil || a.DB == nil {
		return errors.New("attachments database required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session ID required")
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("skill name required")
	}
	return nil
}
