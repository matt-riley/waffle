// Package artifact persists files intentionally produced inside an
// authorized session/workspace (#480). Artifacts are declared explicitly by
// tools (write_artifact) using opaque IDs; the payload lives in the SQLite
// store so no host path ever enters metadata, the Desk, or the transcript.
// Serving re-verifies session ownership, the stored digest, and the size cap
// before any payload leaves the process.
package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
)

// States describe an artifact's serveability lifecycle.
const (
	StateAvailable = "available"
	StateStale     = "stale"
	StateMissing   = "missing"
)

// States lists the valid lifecycle states.
var States = []string{StateAvailable, StateStale, StateMissing}

// Caps bound artifact size and count so the single-connection SQLite store
// stays cheap and a hostile model cannot bloat the database.
const (
	// MaxBytes caps one artifact payload. Images and documents the model
	// produces fit well under this; it matches the media cap's spirit.
	MaxBytes = 10 << 20
	// MaxNameBytes caps the safe display name.
	MaxNameBytes = 200
)

// ErrTooLarge is returned when an artifact payload exceeds MaxBytes.
var ErrTooLarge = errors.New("artifact payload exceeds the size limit")

// ErrNotFound is returned when an artifact row is missing.
var ErrNotFound = errors.New("artifact not found")

// ErrNotOwned is returned when an artifact belongs to another session.
var ErrNotOwned = errors.New("artifact belongs to another session")

// Artifact is one persisted artifact row (metadata + payload).
type Artifact struct {
	ID        string
	SessionID string
	TurnSeq   int64
	ToolName  string
	Name      string
	MediaType string
	Size      int64
	Digest    string
	State     string
	Payload   []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists artifacts for sessions.
type Store struct {
	DB *sql.DB
	// Now, when set, freezes the clock (tests).
	Now func() time.Time
	// MaxPerSession caps artifacts per session; zero uses the default.
	MaxPerSession int
}

// New wraps an opened waffle store.
func New(db *sql.DB) *Store { return &Store{DB: db} }

func (s *Store) clock() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) nowStr() string { return s.clock().Format(time.RFC3339Nano) }

func (s *Store) maxPerSession() int {
	if s.MaxPerSession > 0 {
		return s.MaxPerSession
	}
	return 128
}

// ValidName reports whether name is a safe display name: no path separators,
// no control characters, bounded length. Names are metadata only — the
// payload never lives at a host path.
func ValidName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxNameBytes {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// previewableMediaTypes lists payload types the Desk may render inline.
// Everything else is download-only or blocked.
var previewableMediaTypes = map[string]bool{
	"text/plain": true, "text/markdown": true, "text/csv": true,
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// Previewable reports whether a media type may be previewed inline.
func Previewable(mediaType string) bool {
	return previewableMediaTypes[strings.ToLower(strings.TrimSpace(mediaType))]
}

// Write validates and persists one artifact. The session and tool name are
// recorded for provenance; the opaque ID is returned via the Artifact. An
// empty or oversized payload is rejected; the digest is computed here so the
// stored row always matches the payload.
func (s *Store) Write(ctx context.Context, sessionID, toolName, name, mediaType string, payload []byte) (*Artifact, error) {
	sessionID = strings.TrimSpace(sessionID)
	name = strings.TrimSpace(name)
	if sessionID == "" {
		return nil, errors.New("session id required")
	}
	if !ValidName(name) {
		return nil, fmt.Errorf("artifact name %q is not a safe display name", name)
	}
	if len(payload) > MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(payload))
	}
	if len(payload) == 0 {
		return nil, errors.New("artifact payload is empty")
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType == "" {
		return nil, errors.New("artifact media type is required")
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		return nil, fmt.Errorf("count session artifacts: %w", err)
	}
	if count >= s.maxPerSession() {
		return nil, fmt.Errorf("session artifact limit reached (%d)", s.maxPerSession())
	}
	idstr, err := id.NewBytes(8)
	if err != nil {
		return nil, fmt.Errorf("new artifact id: %w", err)
	}
	digest := sha256.Sum256(payload)
	ts := s.nowStr()
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO artifacts (id, session_id, tool_name, name, media_type, size_bytes, digest, state, payload, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		idstr, sessionID, strings.TrimSpace(toolName), name, mediaType, len(payload), hex.EncodeToString(digest[:]), StateAvailable, payload, ts, ts); err != nil {
		return nil, fmt.Errorf("insert artifact: %w", err)
	}
	return &Artifact{
		ID: idstr, SessionID: sessionID, ToolName: toolName, Name: name, MediaType: mediaType,
		Size: int64(len(payload)), Digest: hex.EncodeToString(digest[:]), State: StateAvailable,
		Payload: payload, CreatedAt: s.clock(), UpdatedAt: s.clock(),
	}, nil
}

// List returns a session's artifacts, newest first.
func (s *Store) List(ctx context.Context, sessionID string) ([]Artifact, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, session_id, tool_name, name, media_type, size_bytes, digest, state, created_at, updated_at
		FROM artifacts WHERE session_id = ? ORDER BY created_at DESC, id DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		var created, updated string
		if err := rows.Scan(&a.ID, &a.SessionID, &a.ToolName, &a.Name, &a.MediaType, &a.Size, &a.Digest, &a.State, &created, &updated); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		a.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get loads one artifact's metadata and payload. Ownership is enforced:
// requesting an artifact from another session returns ErrNotOwned, never the
// row or payload.
func (s *Store) Get(ctx context.Context, sessionID, id string) (*Artifact, error) {
	var a Artifact
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, session_id, tool_name, name, media_type, size_bytes, digest, state, payload, created_at, updated_at
		FROM artifacts WHERE id = ?`, id).Scan(&a.ID, &a.SessionID, &a.ToolName, &a.Name, &a.MediaType, &a.Size, &a.Digest, &a.State, &a.Payload, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if sessionID != "" && a.SessionID != sessionID {
		return nil, ErrNotOwned
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	a.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &a, nil
}

// Ref returns the safe metadata projection without the payload.
func (a *Artifact) Ref() llm.ArtifactRef {
	return llm.ArtifactRef{
		ID: a.ID, Name: a.Name, MediaType: a.MediaType,
		Size: a.Size, Digest: a.Digest, State: a.State,
	}
}

// VerifyDigest recomputes the payload digest and reports whether it matches
// the stored row. A mismatch marks the artifact stale and returns an error so
// callers never serve tampered bytes.
func (s *Store) VerifyDigest(ctx context.Context, id string) error {
	a, err := s.Get(ctx, "", id)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(a.Payload)
	if hex.EncodeToString(sum[:]) != a.Digest {
		_ = s.SetState(ctx, id, StateStale)
		return fmt.Errorf("artifact %s digest mismatch", id)
	}
	return nil
}

// SetState records a lifecycle transition (available/stale/missing) without
// touching the payload.
func (s *Store) SetState(ctx context.Context, id, state string) error {
	if !validState(state) {
		return fmt.Errorf("invalid artifact state %q", state)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE artifacts SET state = ?, updated_at = ? WHERE id = ?`, state, s.nowStr(), id); err != nil {
		return fmt.Errorf("set artifact state: %w", err)
	}
	return nil
}

func validState(state string) bool {
	for _, candidate := range States {
		if candidate == state {
			return true
		}
	}
	return false
}
