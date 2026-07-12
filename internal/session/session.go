// Package session persists conversations (docs/plan.md, "Skills & memory"):
// every turn lands in SQLite and is FTS5-indexed, so past conversations are
// searchable from any future session.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

// ErrNotFound is returned when a session doesn't exist.
var ErrNotFound = errors.New("session not found")

// Store persists sessions and turns.
type Store struct {
	db *sql.DB
}

// New wraps an opened waffle store.
func New(s *store.Store) *Store { return &Store{db: s.DB} }
func (s *Store) DB() *sql.DB    { return s.db }

// Session is one conversation.
type Session struct {
	ID        string
	Title     string
	Summary   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Create starts a new session.
func (s *Store) Create(ctx context.Context, title string) (*Session, error) {
	idstr, err := id.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session id: %w", err)
	}
	sess := &Session{ID: idstr, Title: title}
	ts := now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		sess.ID, sess.Title, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// SetTitle names a session (typically from its first user message).
func (s *Store) SetTitle(ctx context.Context, id, title string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`, title, now(), id)
	return err
}

// SetSummary records the reflection pass's summary.
func (s *Store) SetSummary(ctx context.Context, id, summary string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET summary = ?, updated_at = ? WHERE id = ?`, summary, now(), id)
	return err
}

// Get loads one session by id.
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT id, title, summary, created_at, updated_at FROM sessions WHERE id = ?`, id).Scan(&sess.ID, &sess.Title, &sess.Summary, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var parseErr error
	if sess.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, created); parseErr != nil && created != "" {
		return nil, parseErr
	}
	if sess.UpdatedAt, parseErr = time.Parse(time.RFC3339Nano, updated); parseErr != nil && updated != "" {
		return nil, parseErr
	}
	return &sess, nil
}

// Latest returns the most recently updated session, or ErrNotFound.
func (s *Store) Latest(ctx context.Context) (*Session, error) {
	rows, err := s.list(ctx, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// List returns sessions, most recently updated first.
func (s *Store) List(ctx context.Context, limit int) ([]Session, error) {
	return s.list(ctx, limit)
}

func (s *Store) list(ctx context.Context, limit int) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, summary, created_at, updated_at
		FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []Session
	for rows.Next() {
		var sess Session
		var created, updated string
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Summary, &created, &updated); err != nil {
			return nil, err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil && created != "" {
			return nil, fmt.Errorf("parse session created_at: %w", err)
		}
		sess.CreatedAt = createdAt
		updatedAt, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil && updated != "" {
			return nil, fmt.Errorf("parse session updated_at: %w", err)
		}
		sess.UpdatedAt = updatedAt
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AppendTurn stores one message at the end of a session.
func (s *Store) AppendTurn(ctx context.Context, sessionID string, msg llm.Message) error {
	blocks, err := json.Marshal(msg.Blocks)
	if err != nil {
		return err
	}
	ts := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM turns WHERE session_id = ?), ?, ?, ?, ?)`,
		sessionID, sessionID, string(msg.Role), string(blocks), indexableText(msg), ts); err != nil {
		return fmt.Errorf("append turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, ts, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// Turns loads a session's full history in order.
func (s *Store) Turns(ctx context.Context, sessionID string) ([]llm.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, blocks FROM turns WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var msgs []llm.Message
	for rows.Next() {
		var role, blocks string
		if err := rows.Scan(&role, &blocks); err != nil {
			return nil, err
		}
		msg := llm.Message{Role: llm.Role(role)}
		if err := json.Unmarshal([]byte(blocks), &msg.Blocks); err != nil {
			return nil, fmt.Errorf("session %s: corrupt turn: %w", sessionID, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

// indexableText extracts the searchable text of a message: visible text and
// tool results, not thinking or tool-call JSON.
func indexableText(msg llm.Message) string {
	var parts []string
	for _, b := range msg.Blocks {
		switch b.Type {
		case llm.BlockText:
			parts = append(parts, b.Text)
		case llm.BlockToolResult:
			parts = append(parts, b.ToolResult.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// Repair makes a resumed transcript valid: a session that died mid-tool-
// loop ends with unanswered tool_use blocks, which providers reject. Close
// them out with error results.
func Repair(history []llm.Message) []llm.Message {
	return RepairWithReclaim(history, nil)
}

// RepairWithReclaim closes dangling tool calls, adopting durable results
// supplied by the sandbox when available and fabricating only the remainder.
func RepairWithReclaim(history []llm.Message, reclaim func([]string) (map[string]llm.ToolResult, error)) []llm.Message {
	if len(history) == 0 {
		return history
	}
	last := history[len(history)-1]
	if last.Role != llm.RoleAssistant {
		return history
	}
	var results []llm.Block
	var ids []string
	for _, b := range last.Blocks {
		if b.Type == llm.BlockToolUse {
			ids = append(ids, b.ToolUse.ID)
		}
	}
	adopted := map[string]llm.ToolResult{}
	if reclaim != nil && len(ids) > 0 {
		if got, err := reclaim(ids); err == nil {
			adopted = got
		}
	}
	for _, b := range last.Blocks {
		if b.Type == llm.BlockToolUse {
			if result, ok := adopted[b.ToolUse.ID]; ok {
				results = append(results, llm.Block{Type: llm.BlockToolResult, ToolResult: &result})
				continue
			}
			results = append(results, llm.Block{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{
				ToolUseID: b.ToolUse.ID,
				Content:   "session was interrupted before this tool ran",
				IsError:   true,
			}})
		}
	}
	if len(results) == 0 {
		return history
	}
	return append(history, llm.Message{Role: llm.RoleUser, Blocks: results})
}

// Hit is one recall search result.
type Hit struct {
	TurnID    int64
	SessionID string
	Title     string
	Summary   string
	Snippet   string
	CreatedAt time.Time
}

// Delete removes a session and its turns atomically. The foreign-key cascade
// and FTS delete trigger keep the searchable index consistent.
func (s *Store) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_groups WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session binding: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return vacuum(ctx, s.db)
}

// DeleteTurns removes matching turn rows in one transaction.
func (s *Store) DeleteTurns(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM turns WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return vacuum(ctx, s.db)
}

// Retain deletes sessions older than cutoff, excluding workspaces that are
// still open or idle so lifecycle ownership remains valid.
func (s *Store) Retain(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	cut := cutoff.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_groups WHERE session_id IN (SELECT id FROM sessions WHERE updated_at < ? AND id NOT IN (SELECT session_id FROM workspaces))`, cut); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE updated_at < ? AND id NOT IN (SELECT session_id FROM workspaces)`, cut)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := vacuum(ctx, s.db); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func vacuum(ctx context.Context, db *sql.DB) error {
	var mode int
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if mode == 2 {
		_, err := db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
		return err
	}
	_, err := db.ExecContext(ctx, `VACUUM`)
	return err
}

// Search runs a full-text query over all stored turns. The user-supplied
// query is quoted term-by-term so FTS5 operator syntax can't error out.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return nil, nil
	}
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.session_id, s.title, s.summary,
		       snippet(turns_fts, 0, '[', ']', ' … ', 24),
		       t.created_at
		FROM turns_fts
		JOIN turns t ON t.id = turns_fts.rowid
		JOIN sessions s ON s.id = t.session_id
		WHERE turns_fts MATCH ?
		ORDER BY rank
		LIMIT ?`, strings.Join(terms, " "), limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var hits []Hit
	for rows.Next() {
		var h Hit
		var created string
		if err := rows.Scan(&h.TurnID, &h.SessionID, &h.Title, &h.Summary, &h.Snippet, &created); err != nil {
			return nil, err
		}
		if h.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil && created != "" {
			return nil, fmt.Errorf("parse hit created_at: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
