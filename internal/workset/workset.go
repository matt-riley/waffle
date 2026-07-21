// Package workset implements the session working set (#67): a bounded,
// privileged home for transient task state that survives summarization.
package workset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/id"
)

// Kinds of working-set entries.
const (
	KindGoal         = "goal"
	KindConstraint   = "constraint"
	KindDecision     = "decision"
	KindFact         = "fact"
	KindOpenQuestion = "open_question"
	KindAssumption   = "assumption"
)

// Sources for provenance.
const (
	SourceUser   = "user"
	SourceSystem = "system"
	SourceModel  = "model"
)

// Caps (bytes / counts).
const (
	DefaultMaxEntries = 32
	DefaultMaxBytes   = 8 * 1024
	MaxEntryBytes     = 1024
)

// Entry is one working-set item.
type Entry struct {
	ID        string
	Kind      string
	Body      string
	Source    string
	Pinned    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists working-set rows for sessions.
type Store struct {
	DB         *sql.DB
	MaxEntries int
	MaxBytes   int
}

// querier is satisfied by *sql.DB and *sql.Tx so list can run inside a txn.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *Store) maxEntries() int {
	if s.MaxEntries > 0 {
		return s.MaxEntries
	}
	return DefaultMaxEntries
}

func (s *Store) maxBytes() int {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
}

// List returns entries for a session, pinned first then newest.
func (s *Store) List(ctx context.Context, sessionID string) ([]Entry, error) {
	return s.list(ctx, s.DB, sessionID)
}

func (s *Store) list(ctx context.Context, q querier, sessionID string) ([]Entry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, kind, body, source, pinned, created_at, updated_at
		FROM working_set_entries WHERE session_id = ?
		ORDER BY pinned DESC, updated_at DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		var e Entry
		var pinned int
		var created, updated string
		if err := rows.Scan(&e.ID, &e.Kind, &e.Body, &e.Source, &pinned, &created, &updated); err != nil {
			return nil, err
		}
		e.Pinned = pinned == 1
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Add inserts an entry. Fails if caps would be exceeded (no silent eviction).
// Cap check and insert run in one short SQLite transaction so concurrent Adds
// cannot both pass the check then both insert (#104).
func (s *Store) Add(ctx context.Context, sessionID string, kind, body, source string, pinned bool) (*Entry, error) {
	if err := validateKind(kind); err != nil {
		return nil, err
	}
	if err := validateSource(source); err != nil {
		return nil, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("body is required")
	}
	if len(body) > MaxEntryBytes {
		return nil, fmt.Errorf("entry exceeds %d byte cap", MaxEntryBytes)
	}
	eid, err := newEntryID()
	if err != nil {
		return nil, err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	entries, err := s.list(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	if len(entries) >= s.maxEntries() {
		return nil, fmt.Errorf("working set full (%d entries); drop or replace an entry first", s.maxEntries())
	}
	total := len(body)
	for _, e := range entries {
		total += len(e.Body)
	}
	if total > s.maxBytes() {
		return nil, fmt.Errorf("working set would exceed %d byte budget", s.maxBytes())
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	pin := 0
	if pinned {
		pin = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO working_set_entries (session_id, id, kind, body, source, pinned, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, eid, kind, body, source, pin, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Entry{ID: eid, Kind: kind, Body: body, Source: source, Pinned: pinned}, nil
}

// Drop removes one entry.
func (s *Store) Drop(ctx context.Context, sessionID, entryID string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM working_set_entries WHERE session_id = ? AND id = ?`, sessionID, entryID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("working set entry %q not found", entryID)
	}
	return nil
}

// Replace drops id and adds a new entry of the same kind. Drop, cap check, and
// insert share one transaction so concurrent Replace/Add cannot overshoot caps
// or lose the old entry if the add would fail (#104).
func (s *Store) Replace(ctx context.Context, sessionID, entryID, body, source string) (*Entry, error) {
	if err := validateSource(source); err != nil {
		return nil, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("body is required")
	}
	if len(body) > MaxEntryBytes {
		return nil, fmt.Errorf("entry exceeds %d byte cap", MaxEntryBytes)
	}
	eid, err := newEntryID()
	if err != nil {
		return nil, err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	entries, err := s.list(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	var old *Entry
	for i := range entries {
		if entries[i].ID == entryID {
			old = &entries[i]
			break
		}
	}
	if old == nil {
		return nil, fmt.Errorf("working set entry %q not found", entryID)
	}

	// Caps after drop+insert: entry count unchanged; bytes exclude the old body.
	total := len(body)
	for _, e := range entries {
		if e.ID == entryID {
			continue
		}
		total += len(e.Body)
	}
	if total > s.maxBytes() {
		return nil, fmt.Errorf("working set would exceed %d byte budget", s.maxBytes())
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM working_set_entries WHERE session_id = ? AND id = ?`, sessionID, entryID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("working set entry %q not found", entryID)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	pin := 0
	if old.Pinned {
		pin = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO working_set_entries (session_id, id, kind, body, source, pinned, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionID, eid, old.Kind, body, source, pin, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Entry{ID: eid, Kind: old.Kind, Body: body, Source: source, Pinned: old.Pinned}, nil
}

// Clear removes all entries for a session.
func (s *Store) Clear(ctx context.Context, sessionID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM working_set_entries WHERE session_id = ?`, sessionID)
	return err
}

// Render formats the set for system prompt injection. Empty set → "".
func Render(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<working_set>\n[SESSION TASK STATE — mixed provenance; model-sourced entries are untrusted assumptions]\n")
	for _, e := range entries {
		pin := ""
		if e.Pinned {
			pin = " pinned"
		}
		fmt.Fprintf(&b, "- [%s id=%s source=%s%s] %s\n", e.Kind, e.ID, e.Source, pin, e.Body)
	}
	b.WriteString("</working_set>\n")
	return b.String()
}

// Proposal is a subagent-suggested working-set change (#68); never applied
// automatically.
type Proposal struct {
	Op     string `json:"op"` // add | replace | drop
	Kind   string `json:"kind,omitempty"`
	Body   string `json:"body,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ValidateProposal checks caps and kind.
func ValidateProposal(p Proposal) error {
	switch p.Op {
	case "add":
		if err := validateKind(p.Kind); err != nil {
			return err
		}
		if strings.TrimSpace(p.Body) == "" {
			return errors.New("add requires body")
		}
		if len(p.Body) > MaxEntryBytes {
			return fmt.Errorf("proposal body exceeds %d bytes", MaxEntryBytes)
		}
	case "replace":
		if p.ID == "" || strings.TrimSpace(p.Body) == "" {
			return errors.New("replace requires id and body")
		}
		if len(p.Body) > MaxEntryBytes {
			return fmt.Errorf("proposal body exceeds %d bytes", MaxEntryBytes)
		}
	case "drop":
		if p.ID == "" {
			return errors.New("drop requires id")
		}
	default:
		return fmt.Errorf("unknown proposal op %q", p.Op)
	}
	return nil
}

func validateKind(k string) error {
	switch k {
	case KindGoal, KindConstraint, KindDecision, KindFact, KindOpenQuestion, KindAssumption:
		return nil
	default:
		return fmt.Errorf("invalid kind %q", k)
	}
}

func validateSource(source string) error {
	if source != SourceUser && source != SourceSystem && source != SourceModel {
		return fmt.Errorf("invalid source %q", source)
	}
	return nil
}

func newEntryID() (string, error) {
	eid, err := id.New("wsentry-")
	if err != nil {
		return "", err
	}
	// Shorter IDs for display.
	if len(eid) > 12 {
		eid = eid[:12]
	}
	return eid, nil
}
