// Package entity implements the routing chain from docs/plan.md: owner
// identity → channel group → session. waffle is single-owner by design:
// there is no guest tier. It answers two questions for every inbound
// message: "is this the owner?" (identity or pairing) and "which
// conversation does this belong to?" (channel group → session).
package entity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// ErrUnknownSender means the sender is not one of the owner's identities.
var ErrUnknownSender = errors.New("unknown sender")

// Identity is one of the owner's channel accounts.
type Identity struct {
	Channel    string
	ExternalID string
	Name       string
}

// Pairing is a pending request from a not-yet-recognized sender. Approving
// requires the host CLI — shell access to the machine is the ownership
// proof, so a stranger's code is useless to them.
type Pairing struct {
	Code       string
	Channel    string
	ExternalID string
	SenderName string
	ChatID     string
	CreatedAt  time.Time
}

// Group is one conversation bound to an agent group and session.
type Group struct {
	ID         int64
	Channel    string
	ChatID     string
	AgentGroup string
	SessionID  string
}

// Store persists the entity model.
type Store struct {
	db       *sql.DB
	sessions *session.Store
}

// New wraps an opened waffle store.
func New(st *store.Store, sessions *session.Store) *Store {
	return &Store{db: st.DB, sessions: sessions}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Identify resolves a channel sender to an owner identity, or
// ErrUnknownSender.
func (s *Store) Identify(ctx context.Context, channel, externalID string) (*Identity, error) {
	var id Identity
	err := s.db.QueryRowContext(ctx, `
		SELECT channel, external_id, name FROM identities
		WHERE channel = ? AND external_id = ?`, channel, externalID).
		Scan(&id.Channel, &id.ExternalID, &id.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownSender
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// Pair returns the sender's pairing code, creating one on first contact.
// Re-messaging returns the same code rather than minting endlessly.
func (s *Store) Pair(ctx context.Context, channel, externalID, senderName, chatID string) (*Pairing, error) {
	var p Pairing
	read := func() error {
		return s.db.QueryRowContext(ctx, `
		SELECT code, channel, external_id, sender_name, chat_id FROM pairings
		WHERE channel = ? AND external_id = ?`, channel, externalID).
			Scan(&p.Code, &p.Channel, &p.ExternalID, &p.SenderName, &p.ChatID)
	}
	err := read()
	if err == nil {
		return &p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	code, err := id.NewPairingCode()
	if err != nil {
		return nil, fmt.Errorf("new pairing code: %w", err)
	}
	p = Pairing{Code: code, Channel: channel, ExternalID: externalID, SenderName: senderName, ChatID: chatID}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO pairings (code, channel, external_id, sender_name, chat_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		p.Code, p.Channel, p.ExternalID, p.SenderName, p.ChatID, now()); err != nil {
		if err := read(); err == nil {
			return &p, nil
		}
		return nil, fmt.Errorf("create pairing: %w", err)
	}
	return &p, nil
}

// Pairings lists pending pairing requests.
func (s *Store) Pairings(ctx context.Context) (out []Pairing, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT code, channel, external_id, sender_name, chat_id, created_at
		FROM pairings ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		var p Pairing
		var created string
		if err := rows.Scan(&p.Code, &p.Channel, &p.ExternalID, &p.SenderName, &p.ChatID, &created); err != nil {
			return nil, err
		}
		if p.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil && created != "" {
			return nil, fmt.Errorf("parse pairing created_at: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Approve promotes a pairing to an owner identity. name may be empty to
// keep the sender's channel name.
func (s *Store) Approve(ctx context.Context, code, name string) (*Identity, error) {
	var p Pairing
	err := s.db.QueryRowContext(ctx, `
		SELECT code, channel, external_id, sender_name FROM pairings WHERE code = ?`, code).
		Scan(&p.Code, &p.Channel, &p.ExternalID, &p.SenderName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no pairing with code %q", code)
	}
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = p.SenderName
	}

	id := &Identity{Channel: p.Channel, ExternalID: p.ExternalID, Name: name}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO identities (channel, external_id, name, created_at) VALUES (?, ?, ?, ?)`,
		id.Channel, id.ExternalID, id.Name, now()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairings WHERE code = ?`, code); err != nil {
		return nil, err
	}
	return id, tx.Commit()
}

// GroupFor returns the channel group for a conversation, creating it (and
// its session) on first contact.
func (s *Store) GroupFor(ctx context.Context, channel, chatID string) (*Group, error) {
	g := &Group{Channel: channel, ChatID: chatID}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_group, session_id FROM channel_groups
		WHERE channel = ? AND chat_id = ?`, channel, chatID).
		Scan(&g.ID, &g.AgentGroup, &g.SessionID)
	if err == nil {
		return g, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	sess, err := s.sessions.Create(ctx, fmt.Sprintf("%s %s", channel, chatID))
	if err != nil {
		return nil, err
	}
	g.AgentGroup = "main"
	g.SessionID = sess.ID
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channel_groups (channel, chat_id, agent_group, session_id, created_at)
		VALUES (?, ?, ?, ?, ?)`, channel, chatID, g.AgentGroup, g.SessionID, now())
	if err != nil {
		return nil, fmt.Errorf("create channel group: %w", err)
	}
	g.ID, _ = res.LastInsertId()
	return g, nil
}

// TargetForSession returns the channel delivery target associated with a
// conversation session, if it has one.
func (s *Store) TargetForSession(ctx context.Context, sessionID string) (string, bool, error) {
	var channel, chatID string
	err := s.db.QueryRowContext(ctx, `SELECT channel, chat_id FROM channel_groups WHERE session_id = ?`, sessionID).Scan(&channel, &chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return channel + ":" + chatID, true, nil
}
