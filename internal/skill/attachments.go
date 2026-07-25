package skill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/lifecycle"
	"github.com/matt-riley/waffle/internal/memory"
)

// Attachments persists the skills explicitly attached to a session.
type Attachments struct {
	DB        *sql.DB
	Workspace memory.Workspace
	Lifecycle *lifecycle.Guard
}

// AttachmentReference identifies a session that still uses a skill.
type AttachmentReference struct {
	SessionID string
	Title     string
}

// Attach idempotently associates a skill name with a session.
func (a *Attachments) Attach(ctx context.Context, sessionID, name string) error {
	if err := a.validate(sessionID, name); err != nil {
		return err
	}
	if a.Lifecycle != nil {
		a.Lifecycle.Lock()
		defer a.Lifecycle.Unlock()
	}
	if a.Workspace.Dir != "" {
		active, err := DiscoverActive(a.Workspace.SkillsDir(), a.DB)
		if err != nil {
			return fmt.Errorf("check skill before attach: %w", err)
		}
		if _, ok := Find(active, strings.TrimSpace(name)); !ok {
			return fmt.Errorf("%w: skill %q is not active or installed", ErrSkillNotFound, strings.TrimSpace(name))
		}
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

// References returns the sessions that currently hold a skill attachment.
// Titles are display metadata; session IDs remain available for exact action.
func (a *Attachments) References(ctx context.Context, name string) (out []AttachmentReference, err error) {
	if a == nil || a.DB == nil {
		return nil, errors.New("attachments database required")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("skill name required")
	}
	rows, err := a.DB.QueryContext(ctx, `
		SELECT ss.session_id, COALESCE(s.title, '')
		FROM session_skills ss
		LEFT JOIN sessions s ON s.id = ss.session_id
		WHERE ss.skill_name = ?
		ORDER BY COALESCE(s.title, ''), ss.session_id`, name)
	if err != nil {
		return nil, fmt.Errorf("list skill references: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}()
	for rows.Next() {
		var reference AttachmentReference
		if err := rows.Scan(&reference.SessionID, &reference.Title); err != nil {
			return nil, fmt.Errorf("scan skill reference: %w", err)
		}
		out = append(out, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list skill references: %w", err)
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
