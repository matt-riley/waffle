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
var (
	ErrNotFound          = errors.New("session not found")
	ErrModelAliasChanged = errors.New("session model alias changed")
	// ErrSessionWorkspaceActive is returned when a session still owns a live
	// (open or idle) workspace and therefore cannot be deleted.
	ErrSessionWorkspaceActive = errors.New("session workspace is active")
	// ErrInvalidBranchBoundary is returned when a fork boundary would cut a
	// tool-use/tool-result sequence into invalid history or is out of range.
	ErrInvalidBranchBoundary = errors.New("invalid branch boundary")
)

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
	ID                string
	Title             string
	Summary           string
	ModelAlias        string
	ModelAliasVersion int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// Pinned keeps a conversation visible ahead of ordinary recents without
	// changing its last-activity ordering (#470).
	Pinned bool
	// ForkedFrom is the source session this conversation branched from, and
	// ForkedAtSeq is the sequence of the completed exchange it was cut at
	// (#471). Empty source means the session was started fresh.
	ForkedFrom  string
	ForkedAtSeq int64
	// SummaryWatermark is the highest turn sequence the Summary covers (#411).
	// A resumed session with new turns is eligible for idle reflection again
	// even though a summary already exists.
	SummaryWatermark int64
	// ReflectedAt is when the summary was last written. It is metadata only;
	// idle timing is based on UpdatedAt (conversation activity), never this.
	ReflectedAt time.Time
}

// ModelAliasChange records one exact session choice transition. The removal
// flow uses these records rather than a broad alias update so a concurrent
// Today edit fails closed instead of being overwritten.
type ModelAliasChange struct {
	SessionID            string `json:"session_id"`
	OriginalAlias        string `json:"original_alias"`
	ReplacementAlias     string `json:"replacement_alias"`
	OriginalVersion      int64  `json:"original_version,omitempty"`
	ReplacementVersion   int64  `json:"replacement_version,omitempty"`
	OriginalUpdatedAt    string `json:"original_updated_at,omitempty"`
	ReplacementUpdatedAt string `json:"replacement_updated_at,omitempty"`
}

// Create starts a new session.
func (s *Store) Create(ctx context.Context, title string) (*Session, error) {
	return s.CreateWith(ctx, s.db, title)
}

// execer is the query surface shared by *sql.DB and *sql.Tx so a session can
// be created inside a caller's transaction (entity.GroupFor creates the
// session and channel group atomically, #290).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// CreateWith inserts a session using an explicit execer (a *sql.DB or an
// in-flight *sql.Tx). Committing/rolling back the transaction stays the
// caller's responsibility.
func (s *Store) CreateWith(ctx context.Context, ex execer, title string) (*Session, error) {
	idstr, err := id.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session id: %w", err)
	}
	sess := &Session{ID: idstr, Title: title}
	ts := s.nowStr()
	_, err = ex.ExecContext(ctx,
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

// SetPinned pins or unpins a conversation without touching updated_at, so
// pinning never changes last-activity ordering.
func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET pinned = ? WHERE id = ?`, pinned, id)
	if err != nil {
		return fmt.Errorf("set session pinned: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read set-pinned result: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSummary records the reflection pass's summary and marks it as covering
// every turn the session has today (the current max turn sequence). It does
// not touch updated_at: idle reflection timing stays based on conversation
// activity, not summary writes (#411). The watermark derives from MAX(seq) in
// the same statement — indexed by UNIQUE(session_id, seq) — rather than a
// separate COUNT query (#411 review).
func (s *Store) SetSummary(ctx context.Context, id, summary string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET
			summary = ?,
			summary_watermark = (SELECT COALESCE(MAX(seq), 0) FROM turns WHERE session_id = sessions.id),
			reflected_at = ?
		WHERE id = ?`,
		summary, s.nowStr(), id)
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

// SetSummaryWatermark records a summary with an explicit coverage watermark:
// the highest turn sequence the summary actually includes (#411). It never
// bumps updated_at, so appending a new turn after reflection leaves the
// session eligible again after the configured idle period.
func (s *Store) SetSummaryWatermark(ctx context.Context, id, summary string, coveredThrough int64) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET summary = ?, summary_watermark = ?, reflected_at = ? WHERE id = ?`,
		summary, coveredThrough, s.nowStr(), id)
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
		"UPDATE sessions SET model_alias = ?, model_alias_version = model_alias_version + 1, updated_at = ? WHERE id = ?",
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

// SetModelAliasIfVersion records a model choice only if the session has not
// changed since the caller loaded it. Long-lived Today runtimes use this CAS
// so a provider removal cannot be followed by a stale runtime write.
func (s *Store) SetModelAliasIfVersion(ctx context.Context, id, alias string, expectedVersion int64) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET model_alias = ?, model_alias_version = model_alias_version + 1, updated_at = ? WHERE id = ? AND model_alias_version = ?",
		strings.TrimSpace(alias), s.nowStr(), id, expectedVersion)
	if err != nil {
		return fmt.Errorf("set session model alias: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read set-model result: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrModelAliasChanged, id)
	}
	return nil
}

// ModelAliasReferences returns persisted session IDs that explicitly select
// alias, in deterministic order. It does not rewrite any session choice.
func (s *Store) ModelAliasReferences(ctx context.Context, alias string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM sessions WHERE model_alias = ? ORDER BY id`, strings.TrimSpace(alias))
	if err != nil {
		return nil, fmt.Errorf("list model alias references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var references []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list model alias references: %w", err)
		}
		references = append(references, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model alias references: %w", err)
	}
	return references, nil
}

// ReplaceModelAlias moves only sessions that explicitly selected from to to.
// Other sessions retain their local model choices.
func (s *Store) ReplaceModelAlias(ctx context.Context, from, to string) error {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace model alias: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET model_alias = ?, model_alias_version = model_alias_version + 1, updated_at = ? WHERE model_alias = ?`,
		to, s.nowStr(), from); err != nil {
		return fmt.Errorf("replace model alias: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace model alias: %w", err)
	}
	return nil
}

// ReplaceModelAliases applies exact, all-or-nothing session transitions. Every
// row must still contain its previewed alias, model-alias version, and
// updated-at value or the transaction is rolled back without changing any
// session.
func (s *Store) ReplaceModelAliases(ctx context.Context, changes []ModelAliasChange) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace model aliases: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, change := range changes {
		if change.OriginalVersion == 0 || change.OriginalUpdatedAt == "" || change.ReplacementVersion == 0 || change.ReplacementUpdatedAt == "" {
			return errors.New("exact model alias transition version is required")
		}
		query := `UPDATE sessions SET model_alias = ?, model_alias_version = ?, updated_at = ? WHERE id = ? AND model_alias = ?`
		args := []any{strings.TrimSpace(change.ReplacementAlias), change.ReplacementVersion, change.ReplacementUpdatedAt, change.SessionID, strings.TrimSpace(change.OriginalAlias), change.OriginalVersion, change.OriginalUpdatedAt}
		query += ` AND model_alias_version = ? AND updated_at = ?`
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("replace model aliases: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read replace model aliases result: %w", err)
		}
		if rows != 1 {
			return fmt.Errorf("%w: %s", ErrModelAliasChanged, change.SessionID)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace model aliases: %w", err)
	}
	return nil
}

// RestoreModelAliases restores only rows that still contain the removal
// replacement. A session changed by Today, or deleted meanwhile, is left
// alone; this is the compare-and-set boundary that prevents rollback from
// overwriting a newer user choice.
func (s *Store) RestoreModelAliases(ctx context.Context, changes []ModelAliasChange) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("restore model aliases: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, change := range changes {
		if change.OriginalVersion == 0 || change.OriginalUpdatedAt == "" || change.ReplacementVersion == 0 || change.ReplacementUpdatedAt == "" {
			return errors.New("exact model alias recovery version is required")
		}
		query := `UPDATE sessions SET model_alias = ?, model_alias_version = ?, updated_at = ? WHERE id = ? AND model_alias = ?`
		args := []any{strings.TrimSpace(change.OriginalAlias), change.OriginalVersion, change.OriginalUpdatedAt, change.SessionID, strings.TrimSpace(change.ReplacementAlias)}
		query += ` AND model_alias_version = ? AND updated_at = ?`
		args = append(args, change.ReplacementVersion, change.ReplacementUpdatedAt)
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("restore model aliases: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read restore model aliases result: %w", err)
		}
		if rows == 1 {
			continue
		}
		var current string
		err = tx.QueryRowContext(ctx, `SELECT model_alias FROM sessions WHERE id = ?`, change.SessionID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(current) == strings.TrimSpace(change.OriginalAlias) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect model alias recovery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("restore model aliases: %w", err)
	}
	return nil
}

// Get loads one session by id.
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var created, updated, reflected string
	err := s.db.QueryRowContext(ctx, `SELECT id, title, summary, model_alias, model_alias_version, created_at, updated_at, summary_watermark, reflected_at, pinned, forked_from, forked_at_seq FROM sessions WHERE id = ?`, id).Scan(&sess.ID, &sess.Title, &sess.Summary, &sess.ModelAlias, &sess.ModelAliasVersion, &created, &updated, &sess.SummaryWatermark, &reflected, &sess.Pinned, &sess.ForkedFrom, &sess.ForkedAtSeq)
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
	if reflected != "" {
		if sess.ReflectedAt, parseErr = time.Parse(time.RFC3339Nano, reflected); parseErr != nil {
			return nil, parseErr
		}
	}
	return &sess, nil
}

const existIDsBatchSize = 100

// ExistIDs reports which of the given session IDs currently exist. Missing IDs
// are simply absent from the result map. Empty input returns an empty map.
func (s *Store) ExistIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(ids) == 0 {
		return out, nil
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out, nil
	}

	// Keep each IN clause small enough for SQLite's variable limit. Besides
	// avoiding SQLITE_TOOBIG for bulk callers, executing each batch separately
	// preserves the same set semantics as one query.
	for start := 0; start < len(unique); start += existIDsBatchSize {
		end := min(start+existIDsBatchSize, len(unique))
		placeholders := make([]string, end-start)
		args := make([]any, end-start)
		for i, id := range unique[start:end] {
			placeholders[i] = "?"
			args[i] = id
		}
		query := `SELECT id FROM sessions WHERE id IN (` + strings.Join(placeholders, ",") + `)`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("exist sessions: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("exist sessions: %w", err)
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("exist sessions: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("exist sessions: %w", err)
		}
	}
	return out, nil
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

// ListUpdatedAfter returns sessions strictly after the keyset cursor
// (updated_at, id), ordered ascending — the learning loop's lossless
// pagination surface (#412). The fixed limit is a page size, never a
// total-window cap. An empty updatedAt starts from the beginning.
func (s *Store) ListUpdatedAfter(ctx context.Context, updatedAt, id string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, summary, model_alias, model_alias_version, created_at, updated_at, summary_watermark, reflected_at, pinned, forked_from, forked_at_seq
		FROM sessions
		WHERE ? = '' OR (updated_at > ? OR (updated_at = ? AND id > ?))
		ORDER BY updated_at ASC, id ASC
		LIMIT ?`, updatedAt, updatedAt, updatedAt, id, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]Session, 0, limit)
	for rows.Next() {
		var sess Session
		var created, updated, reflected string
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Summary, &sess.ModelAlias, &sess.ModelAliasVersion, &created, &updated, &sess.SummaryWatermark, &reflected, &sess.Pinned, &sess.ForkedFrom, &sess.ForkedAtSeq); err != nil {
			return nil, err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil && created != "" {
			return nil, fmt.Errorf("parse session created_at: %w", err)
		}
		sess.CreatedAt = createdAt
		updatedAtT, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil && updated != "" {
			return nil, fmt.Errorf("parse session updated_at: %w", err)
		}
		sess.UpdatedAt = updatedAtT
		if reflected != "" {
			if sess.ReflectedAt, err = time.Parse(time.RFC3339Nano, reflected); err != nil {
				return nil, fmt.Errorf("parse session reflected_at: %w", err)
			}
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) list(ctx context.Context, limit int) (out []Session, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, summary, model_alias, model_alias_version, created_at, updated_at, summary_watermark, reflected_at, pinned, forked_from, forked_at_seq
		FROM sessions ORDER BY pinned DESC, updated_at DESC LIMIT ?`, limit)
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
		var created, updated, reflected string
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Summary, &sess.ModelAlias, &sess.ModelAliasVersion, &created, &updated, &sess.SummaryWatermark, &reflected, &sess.Pinned, &sess.ForkedFrom, &sess.ForkedAtSeq); err != nil {
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
		if reflected != "" {
			if sess.ReflectedAt, err = time.Parse(time.RFC3339Nano, reflected); err != nil {
				return nil, fmt.Errorf("parse session reflected_at: %w", err)
			}
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Branch creates a new conversation that forks from sourceID at boundarySeq.
// The canonical persisted prefix — turns with seq <= boundarySeq — is copied
// into the new session together with the source's model alias, attached
// skills, and working-set entries as independent snapshots, and the new
// session records durable lineage (forked_from, forked_at_seq). The source
// session is never modified. Usage accounting rows are deliberately not
// copied: branching must not duplicate accounting for copied turns.
//
// The boundary is validated server-side: it must be a completed exchange, so
// tool-use/tool-result sequences can never be cut into invalid history.
// Everything runs in one transaction, so failure is atomic and leaves no
// partial session or copied state.
func (s *Store) Branch(ctx context.Context, sourceID string, boundarySeq int64) (*Session, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, errors.New("session id required")
	}
	if boundarySeq < 1 {
		return nil, fmt.Errorf("%w: boundary must be at least turn 1", ErrInvalidBranchBoundary)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("branch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Load the source session and its turns inside the transaction so the
	// boundary validation and the copy see one consistent snapshot.
	var source Session
	var created, updated, reflected string
	err = tx.QueryRowContext(ctx, `SELECT id, title, summary, model_alias, model_alias_version, created_at, updated_at, summary_watermark, reflected_at, forked_from, forked_at_seq FROM sessions WHERE id = ?`, sourceID).Scan(
		&source.ID, &source.Title, &source.Summary, &source.ModelAlias, &source.ModelAliasVersion, &created, &updated, &source.SummaryWatermark, &reflected, &source.ForkedFrom, &source.ForkedAtSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("branch: load source session: %w", err)
	}

	type turnRow struct {
		seq    int64
		role   string
		blocks string
		text   string
	}
	var turns []turnRow
	rows, err := tx.QueryContext(ctx, `SELECT seq, role, blocks, text FROM turns WHERE session_id = ? ORDER BY seq`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("branch: load source turns: %w", err)
	}
	for rows.Next() {
		var row turnRow
		if err := rows.Scan(&row.seq, &row.role, &row.blocks, &row.text); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("branch: scan source turn: %w", err)
		}
		turns = append(turns, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("branch: read source turns: %w", err)
	}
	_ = rows.Close()

	if boundarySeq > int64(len(turns)) {
		return nil, fmt.Errorf("%w: session %s has %d turns, cannot branch at seq %d", ErrInvalidBranchBoundary, sourceID, len(turns), boundarySeq)
	}
	boundary := turns[boundarySeq-1]
	if err := validateBranchBoundary(boundary.role, boundary.blocks, boundarySeq == int64(len(turns))); err != nil {
		return nil, fmt.Errorf("%w: session %s seq %d: %v", ErrInvalidBranchBoundary, sourceID, boundarySeq, err)
	}

	// Copy the canonical prefix: turns, model alias, skills, and working-set
	// entries all snapshot into the new session. The turn INSERT...SELECT
	// reuses the FTS trigger so the searchable index stays consistent.
	newID, err := id.NewSession()
	if err != nil {
		return nil, fmt.Errorf("branch: new session id: %w", err)
	}
	ts := s.nowStr()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, title, model_alias, model_alias_version, created_at, updated_at, forked_from, forked_at_seq)
		VALUES (?, '', ?, ?, ?, ?, ?, ?)`,
		newID, source.ModelAlias, source.ModelAliasVersion, ts, ts, sourceID, boundarySeq); err != nil {
		return nil, fmt.Errorf("branch: create session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		SELECT ?, seq, role, blocks, text, created_at FROM turns WHERE session_id = ? AND seq <= ?`,
		newID, sourceID, boundarySeq); err != nil {
		return nil, fmt.Errorf("branch: copy turns: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_skills (session_id, skill_name, attached_at)
		SELECT ?, skill_name, attached_at FROM session_skills WHERE session_id = ?`,
		newID, sourceID); err != nil {
		return nil, fmt.Errorf("branch: copy skills: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO working_set_entries (session_id, id, kind, body, source, pinned, created_at, updated_at)
		SELECT ?, id, kind, body, source, pinned, created_at, updated_at FROM working_set_entries WHERE session_id = ?`,
		newID, sourceID); err != nil {
		return nil, fmt.Errorf("branch: copy working set: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("branch: commit: %w", err)
	}
	return &Session{
		ID: newID, ModelAlias: source.ModelAlias, ModelAliasVersion: source.ModelAliasVersion,
		CreatedAt: s.clock(), UpdatedAt: s.clock(), ForkedFrom: sourceID, ForkedAtSeq: boundarySeq,
	}, nil
}

// validateBranchBoundary rejects a boundary that would cut a tool loop into
// invalid history. A completed exchange ends with an assistant turn that has
// no pending tool_use blocks; a user turn is only a valid boundary when it is
// the final turn (the transcript ends with the unanswered question). Tool-use
// and tool-result turns between exchanges are never cut.
func validateBranchBoundary(role, blocks string, isLast bool) error {
	if role != string(llm.RoleAssistant) && role != string(llm.RoleUser) {
		return fmt.Errorf("cannot branch inside a %q turn", role)
	}
	if role == string(llm.RoleAssistant) {
		var parsed []llm.Block
		if err := json.Unmarshal([]byte(blocks), &parsed); err != nil {
			return fmt.Errorf("corrupt turn: %w", err)
		}
		for _, block := range parsed {
			if block.Type == llm.BlockToolUse {
				return errors.New("cannot branch mid tool-use exchange; branch after the exchange completes")
			}
		}
		return nil
	}
	if !isLast {
		return errors.New("cannot branch at a user turn that a later exchange completes; branch after the exchange completes")
	}
	return nil
}
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

// Turns loads a session's full history in order. Each message carries its
// persisted sequence as Seq so callers can reference server-validated turn
// boundaries (branching, #471).
func (s *Store) Turns(ctx context.Context, sessionID string) (msgs []llm.Message, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, role, blocks FROM turns WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		var seq int64
		var role, blocks string
		if err := rows.Scan(&seq, &role, &blocks); err != nil {
			return nil, err
		}
		msg := llm.Message{Role: llm.Role(role), Seq: seq}
		if err := json.Unmarshal([]byte(blocks), &msg.Blocks); err != nil {
			return nil, fmt.Errorf("session %s: corrupt turn: %w", sessionID, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

// indexableText extracts the searchable text of a message: visible text and
// tool results, not thinking or tool-call JSON. Text blocks inside a
// block-carrying tool result are indexed too; media payloads are not.
func indexableText(msg llm.Message) string {
	var parts []string
	for _, b := range msg.Blocks {
		switch b.Type {
		case llm.BlockText:
			parts = append(parts, b.Text)
		case llm.BlockToolResult:
			parts = append(parts, b.ToolResult.Content)
			for _, bl := range b.ToolResult.Blocks {
				if bl.Type == llm.BlockText {
					parts = append(parts, bl.Text)
				}
			}
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
	// Fail closed when the session still owns a live workspace: deleting it
	// would strand an open container on a deleted session (#470).
	var live int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE session_id = ? AND status != 'closed'`, id).Scan(&live); err != nil {
		return fmt.Errorf("check session workspaces: %w", err)
	}
	if live > 0 {
		return ErrSessionWorkspaceActive
	}
	// Closed workspaces still reference the session; remove them so no rows
	// dangle after deletion.
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session workspaces: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_claims WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session issue claims: %w", err)
	}
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
// terms via the sessions_fts index (FTS5 relevance blended with recency).
// There is intentionally no LIKE fallback: store.Open always runs migrations
// before any session store is reachable, so a missing-table error here means
// a real schema, corruption, or query failure that must surface rather than
// degrade to incomplete recency-ordered results (#277).
func (s *Store) SearchSummaries(ctx context.Context, query string, limit int) (hits []Hit, err error) {
	terms := strings.Fields(strings.TrimSpace(query))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 4
	}
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
	if err != nil {
		return nil, fmt.Errorf("search summaries via FTS: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h Hit
		var updated string
		if err := rows.Scan(&h.SessionID, &h.Title, &h.Summary, &updated, &h.Snippet); err != nil {
			return nil, err
		}
		if updated != "" {
			parsed, parseErr := time.Parse(time.RFC3339Nano, updated)
			if parseErr != nil {
				return nil, fmt.Errorf("parse summary updated_at: %w", parseErr)
			}
			h.CreatedAt = parsed
		}
		if h.Snippet == "" {
			h.Snippet = h.Summary
		}
		hits = append(hits, h)
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
