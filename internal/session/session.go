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
	// Now, when set, freezes the clock for recency ranking and timestamps
	// (tests: equal-relevance hits prefer the newer turn under a fixed now).
	Now func() time.Time
}

// New wraps an opened waffle store.
func New(s *store.Store) *Store { return &Store{db: s.DB} }
func (s *Store) DB() *sql.DB    { return s.db }

func (s *Store) clock() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) nowStr() string { return s.clock().Format(time.RFC3339Nano) }

// Session is one conversation.
type Session struct {
	ID         string
	Title      string
	Summary    string
	ModelAlias string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Create starts a new session.
func (s *Store) Create(ctx context.Context, title string) (*Session, error) {
	idstr, err := id.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session id: %w", err)
	}
	sess := &Session{ID: idstr, Title: title}
	ts := s.nowStr()
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
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`, title, s.nowStr(), id)
	if err != nil {
		return fmt.Errorf("set session title: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read set-title result: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSummary records the reflection pass's summary.
func (s *Store) SetSummary(ctx context.Context, id, summary string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET summary = ?, updated_at = ? WHERE id = ?`, summary, s.nowStr(), id)
	if err != nil {
		return fmt.Errorf("set session summary: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read set-summary result: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetModelAlias records the configured model alias selected for a session.
func (s *Store) SetModelAlias(ctx context.Context, id, alias string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET model_alias = ?, updated_at = ? WHERE id = ?",
		strings.TrimSpace(alias), s.nowStr(), id)
	if err != nil {
		return fmt.Errorf("set session model alias: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read set-model result: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Get loads one session by id.
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT id, title, summary, model_alias, created_at, updated_at FROM sessions WHERE id = ?`, id).Scan(&sess.ID, &sess.Title, &sess.Summary, &sess.ModelAlias, &created, &updated)
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

func (s *Store) list(ctx context.Context, limit int) (out []Session, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, summary, model_alias, created_at, updated_at
		FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		var sess Session
		var created, updated string
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Summary, &sess.ModelAlias, &created, &updated); err != nil {
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
	ts := s.nowStr()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
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
func (s *Store) Turns(ctx context.Context, sessionID string) (msgs []llm.Message, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, blocks FROM turns WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
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
	// Partial is true when query terms match but not as a contiguous phrase (#60).
	Partial bool
}

// Delete removes a session and its turns atomically. The foreign-key cascade
// and FTS delete trigger keep the searchable index consistent. Tool spills
// and working-set rows for the session are also removed (#69 / #67).
func (s *Store) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_groups WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session binding: %w", err)
	}
	// Spills: delete FTS rows via trigger after content delete.
	if _, err := tx.ExecContext(ctx, `DELETE FROM tool_spills WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session spills: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM working_set_entries WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session working set: %w", err)
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
	return nil
}

// DeleteTurns removes matching turn rows in one transaction.
func (s *Store) DeleteTurns(ctx context.Context, ids []int64) (err error) {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM turns WHERE id = ?`)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := stmt.Close(); err == nil {
			err = cerr
		}
	}()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

// Retain deletes sessions older than cutoff, excluding workspaces that are
// still open or idle so lifecycle ownership remains valid.
func (s *Store) Retain(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
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
	n, _ := res.RowsAffected()
	return n, nil
}

// SearchSummaries finds sessions whose summary or title matches all query
// terms (simple LIKE AND; no FTS table for summaries).
func (s *Store) SearchSummaries(ctx context.Context, query string, limit int) (hits []Hit, err error) {
	terms := strings.Fields(strings.TrimSpace(query))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 4
	}
	// Prefer FTS index when present (#60); fall back to LIKE scan.
	ftsTerms := make([]string, len(terms))
	for i, t := range terms {
		ftsTerms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	now := s.clock().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title, s.summary, s.updated_at,
		       snippet(sessions_fts, 0, '[', ']', ' … ', 24)
		FROM sessions_fts
		JOIN sessions s ON s.rowid = sessions_fts.rowid
		WHERE sessions_fts MATCH ?
		ORDER BY (rank + (strftime('%s', ?) - strftime('%s', s.updated_at)) / 86400.0 * 0.1)
		LIMIT ?`, strings.Join(ftsTerms, " "), now, limit)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var h Hit
			var updated string
			if err := rows.Scan(&h.SessionID, &h.Title, &h.Summary, &updated, &h.Snippet); err != nil {
				return nil, err
			}
			if h.Snippet == "" {
				h.Snippet = h.Summary
			}
			hits = append(hits, h)
		}
		return hits, rows.Err()
	}
	// Fallback when FTS table is missing (pre-migration).
	rows, err = s.db.QueryContext(ctx, `
		SELECT id, title, summary, updated_at FROM sessions
		WHERE summary IS NOT NULL AND summary != ''
		ORDER BY updated_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h Hit
		var created string
		if err := rows.Scan(&h.SessionID, &h.Title, &h.Summary, &created); err != nil {
			return nil, err
		}
		hay := strings.ToLower(h.Title + " " + h.Summary)
		ok := true
		for _, term := range terms {
			if !strings.Contains(hay, strings.ToLower(term)) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		h.Snippet = h.Summary
		if len(h.Snippet) > 240 {
			h.Snippet = h.Snippet[:240] + "…"
		}
		if h.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil && created != "" {
			return nil, err
		}
		hits = append(hits, h)
		if len(hits) >= limit {
			break
		}
	}
	return hits, rows.Err()
}

// Search runs a full-text query over all stored turns. The user-supplied
// query is quoted term-by-term so FTS5 operator syntax can't error out.
// Results blend FTS relevance with recency: equal-relevance hits prefer the
// newer turn (#60).
func (s *Store) Search(ctx context.Context, query string, limit int) (hits []Hit, err error) {
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
	now := s.clock().Format(time.RFC3339)
	// FTS5 rank is more negative for better matches; age (days) is added so
	// older equal-relevance hits sort after newer ones.
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.session_id, s.title, s.summary,
		       snippet(turns_fts, 0, '[', ']', ' … ', 24),
		       t.created_at, t.text
		FROM turns_fts
		JOIN turns t ON t.id = turns_fts.rowid
		JOIN sessions s ON s.id = t.session_id
		WHERE turns_fts MATCH ?
		ORDER BY (rank + (strftime('%s', ?) - strftime('%s', t.created_at)) / 86400.0 * 0.1)
		LIMIT ?`, strings.Join(terms, " "), now, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	qLower := strings.ToLower(strings.Join(strings.Fields(query), " "))
	for rows.Next() {
		var h Hit
		var created, fullText string
		if err := rows.Scan(&h.TurnID, &h.SessionID, &h.Title, &h.Summary, &h.Snippet, &created, &fullText); err != nil {
			return nil, err
		}
		if h.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil && created != "" {
			return nil, fmt.Errorf("parse hit created_at: %w", err)
		}
		// Partial match: terms hit but the query phrase is not contiguous.
		h.Partial = qLower != "" && !strings.Contains(strings.ToLower(fullText), qLower)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// TurnCount returns how many turns a session has.
func (s *Store) TurnCount(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}
