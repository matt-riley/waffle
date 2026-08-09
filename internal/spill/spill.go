// Package spill stores full tool outputs before truncation (#69) so mid-run
// expansion and FTS can recover dropped bytes.
package spill

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/textcut"
	"github.com/matt-riley/waffle/internal/tool"
)

// SpillCap is the maximum bytes stored per spill.
const SpillCap = 512 * 1024

// Store persists spills.
type Store struct {
	DB *sql.DB
}

// Save writes content (already redacted by the caller) and returns an id.
// If content fits under tool.OutputLimit, Save is a no-op and returns "".
func (s *Store) Save(ctx context.Context, sessionID, toolName, content string) (spillID string, partial bool, err error) {
	if utf8.RuneCountInString(content) <= tool.OutputLimit {
		return "", false, nil
	}
	stored := content
	if len(stored) > SpillCap {
		stored = textcut.Cut(stored, SpillCap)
		partial = true
	}
	sid, err := id.New("spill-")
	if err != nil {
		return "", false, err
	}
	if len(sid) > 16 {
		sid = sid[:16]
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO tool_spills (id, session_id, tool_name, content, created_at)
		VALUES (?, ?, ?, ?, ?)`, sid, sessionID, toolName, stored, now); err != nil {
		return "", false, err
	}
	return sid, partial, nil
}

// Marker returns the truncation notice embedded in the tool result.
func Marker(spillID string, partial bool) string {
	if partial {
		return fmt.Sprintf("\n... [output truncated; full content (partial spill) id=%s — use expand_output]", spillID)
	}
	return fmt.Sprintf("\n... [output truncated; full content id=%s — use expand_output]", spillID)
}

// Get returns spill content.
func (s *Store) Get(ctx context.Context, id string) (content string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT content FROM tool_spills WHERE id = ?`, id).Scan(&content)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("unknown spill id %q", id)
	}
	return content, err
}

// Expand returns a byte range or grep matches, then Truncate's the response.
func (s *Store) Expand(ctx context.Context, spillID string, offset, limit int, pattern string) (string, error) {
	content, err := s.Get(ctx, spillID)
	if err != nil {
		return "", err
	}
	if pattern != "" {
		var hits []string
		// Prefer line-oriented matches; for single huge lines, return a
		// bounded window around each match index.
		if strings.Contains(content, "\n") {
			for i, line := range strings.Split(content, "\n") {
				if strings.Contains(line, pattern) {
					hits = append(hits, fmt.Sprintf("%d: %s", i+1, tool.Truncate(line, 200)))
				}
				if len(hits) >= 50 {
					break
				}
			}
		} else {
			from := 0
			for len(hits) < 50 {
				idx := strings.Index(content[from:], pattern)
				if idx < 0 {
					break
				}
				abs := from + idx
				start := abs - 40
				if start < 0 {
					start = 0
				}
				end := abs + len(pattern) + 40
				if end > len(content) {
					end = len(content)
				}
				start, end = snapToRunes(content, start, end)
				hits = append(hits, fmt.Sprintf("@%d: %s", abs, content[start:end]))
				from = abs + len(pattern)
			}
		}
		if len(hits) == 0 {
			return fmt.Sprintf("no matches for %q in spill %s", pattern, spillID), nil
		}
		return tool.Truncate(strings.Join(hits, "\n"), tool.OutputLimit), nil
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(content) {
		return "", fmt.Errorf("offset %d out of range (len=%d)", offset, len(content))
	}
	if limit <= 0 {
		limit = tool.OutputLimit
	}
	end := offset + limit
	if end > len(content) {
		end = len(content)
	}
	start, end := snapToRunes(content, offset, end)
	return tool.Truncate(content[start:end], tool.OutputLimit), nil
}

// snapToRunes moves start forward and end backward until both land on UTF-8
// rune boundaries, so a byte-window slice never splits a multi-byte rune
// (#280). start/end may already be boundary-aligned; end never moves below
// start.
func snapToRunes(s string, start, end int) (int, int) {
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	for end > start && end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	if end < start {
		end = start
	}
	return start, end
}

// DeleteSession removes all spills for a session (retention/forget).
func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM tool_spills WHERE session_id = ?`, sessionID)
	return err
}

// SearchFTS finds spill content matching query terms.
func (s *Store) SearchFTS(ctx context.Context, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 8
	}
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return nil, nil
	}
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT s.id, s.session_id, snippet(tool_spills_fts, 0, '[', ']', ' … ', 16)
		FROM tool_spills_fts
		JOIN tool_spills s ON s.rowid = tool_spills_fts.rowid
		WHERE tool_spills_fts MATCH ?
		LIMIT ?`, strings.Join(terms, " "), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Snippet); err != nil {
			return nil, err
		}
		h.Source = "spill"
		out = append(out, h)
	}
	return out, rows.Err()
}

// Hit is one FTS result.
type Hit struct {
	ID, SessionID, Snippet, Source string
}
