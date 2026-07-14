package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// NotesIndex keeps MEMORY.md / MEMORY.archive.md searchable via SQLite FTS5 (#60).
// File remains the source of truth for injection; this table is the recall index.
type NotesIndex struct {
	DB  *sql.DB
	Now func() time.Time
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (n *NotesIndex) now() time.Time {
	if n != nil && n.Now != nil {
		return n.Now().UTC()
	}
	return time.Now().UTC()
}

// NoteHit is one FTS hit over curated notes (live or archived).
type NoteHit struct {
	ID       string
	Agent    string
	Body     string
	RawLine  string
	Archived bool
	Snippet  string
	NoteDate time.Time
}

// Upsert inserts or replaces a note row and keeps FTS in sync via triggers.
func (n *NotesIndex) Upsert(ctx context.Context, agent string, note note, archived bool) error {
	if n == nil || n.DB == nil {
		return nil
	}
	return n.upsert(ctx, n.DB, agent, note, archived)
}

func (n *NotesIndex) upsert(ctx context.Context, db sqlExecer, agent string, note note, archived bool) error {
	if agent == "" {
		agent = DefaultAgent
	}
	id := note.id
	if id == "" {
		// Legacy un-ID'd lines: stable key from agent + body.
		id = "legacy-" + shortHash(agent+":"+bodyKey(note.body))
	}
	ts := n.now().Format(time.RFC3339Nano)
	date := ""
	if !note.date.IsZero() {
		date = note.date.Format("2006-01-02")
	}
	arch := 0
	if archived {
		arch = 1
	}
	pin := 0
	if note.pinned {
		pin = 1
	}
	body := note.body
	if body == "" {
		body = extractBody(note.raw)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO memory_notes (id, agent, body, raw_line, archived, pinned, note_date, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			agent = excluded.agent,
			body = excluded.body,
			raw_line = excluded.raw_line,
			archived = excluded.archived,
			pinned = excluded.pinned,
			note_date = excluded.note_date,
			updated_at = excluded.updated_at`,
		id, agent, body, note.raw, arch, pin, date, ts, ts)
	return err
}

// MarkArchived flips archived=1 for a note id (forget/supersede).
func (n *NotesIndex) MarkArchived(ctx context.Context, noteID string) error {
	if n == nil || n.DB == nil || noteID == "" {
		return nil
	}
	ts := n.now().Format(time.RFC3339Nano)
	_, err := n.DB.ExecContext(ctx, `
		UPDATE memory_notes SET archived = 1, updated_at = ? WHERE id = ?`, ts, noteID)
	return err
}

// Delete removes a note from the index entirely.
func (n *NotesIndex) Delete(ctx context.Context, noteID string) error {
	if n == nil || n.DB == nil || noteID == "" {
		return nil
	}
	_, err := n.DB.ExecContext(ctx, `DELETE FROM memory_notes WHERE id = ?`, noteID)
	return err
}

// SyncWorkspace rebuilds the index for one agent from MEMORY.md + archive.
// Safe to call after migration of a pre-change DB that has notes only on disk.
func (n *NotesIndex) SyncWorkspace(ctx context.Context, agent string, ws Workspace) error {
	if n == nil || n.DB == nil {
		return nil
	}
	if agent == "" {
		agent = DefaultAgent
	}
	notes, err := n.notesFromFile(ws.MemoryPath(), false)
	if err != nil {
		return err
	}
	archived, err := n.notesFromFile(ws.ArchivePath(), true)
	if err != nil {
		return err
	}
	notes = append(notes, archived...)

	// Rebuilding one workspace index used to commit each note separately. Keep
	// the index replacement atomic and hold the single SQLite writer briefly.
	tx, err := n.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_notes WHERE agent = ?`, agent); err != nil {
		return err
	}
	for _, entry := range notes {
		if err := n.upsert(ctx, tx, agent, entry.note, entry.archived); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type indexedNote struct {
	note     note
	archived bool
}

func (n *NotesIndex) notesFromFile(path string, archived bool) ([]indexedNote, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	notes := make([]indexedNote, 0, strings.Count(string(body), "\n")+1)
	for i, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		notes = append(notes, indexedNote{note: parseNote(line, i), archived: archived})
	}
	return notes, nil
}

// Search runs FTS over live and archived notes, blending relevance with recency.
// now is used for the recency blend (injectable for tests).
func (n *NotesIndex) Search(ctx context.Context, query string, limit int) ([]NoteHit, error) {
	if n == nil || n.DB == nil {
		return nil, nil
	}
	terms := strings.Fields(strings.TrimSpace(query))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 6
	}
	ftsTerms := make([]string, len(terms))
	for i, t := range terms {
		ftsTerms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	now := n.now().Format(time.RFC3339)
	// FTS5 rank is more negative for better matches; age days added so older
	// equal-relevance hits sort after newer ones (#60).
	rows, err := n.DB.QueryContext(ctx, `
		SELECT n.id, n.agent, n.body, n.raw_line, n.archived, n.note_date,
		       snippet(memory_notes_fts, 1, '[', ']', ' … ', 24)
		FROM memory_notes_fts
		JOIN memory_notes n ON n.rowid = memory_notes_fts.rowid
		WHERE memory_notes_fts MATCH ?
		ORDER BY (rank + (strftime('%s', ?) - strftime('%s', COALESCE(NULLIF(n.note_date, ''), n.created_at))) / 86400.0 * 0.1)
		LIMIT ?`, strings.Join(ftsTerms, " "), now, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []NoteHit
	for rows.Next() {
		var h NoteHit
		var arch int
		var date string
		if err := rows.Scan(&h.ID, &h.Agent, &h.Body, &h.RawLine, &arch, &date, &h.Snippet); err != nil {
			return nil, err
		}
		h.Archived = arch != 0
		if date != "" {
			if t, err := time.Parse("2006-01-02", date); err == nil {
				h.NoteDate = t
			}
		}
		if h.Snippet == "" {
			h.Snippet = h.RawLine
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func shortHash(s string) string {
	// Non-crypto stable id for legacy lines; keep short and filesystem-safe.
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}
