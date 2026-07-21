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

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/schedule"
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
	// Profile is an optional named agent profile (#71). Empty uses default.
	Profile   string
	SessionID string
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
// its session) on first contact. agentGroup is used only when creating a new
// row; empty defaults to "main". Multi-party channel chats should pass
// "group" so sessions inherit the restricted group-chat policy (#34).
// Existing rows keep their stored agent_group.
func (s *Store) GroupFor(ctx context.Context, channel, chatID, agentGroup string) (*Group, error) {
	g := &Group{Channel: channel, ChatID: chatID}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_group, session_id, profile FROM channel_groups
		WHERE channel = ? AND chat_id = ?`, channel, chatID).
		Scan(&g.ID, &g.AgentGroup, &g.SessionID, &g.Profile)
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
	if agentGroup == "" {
		agentGroup = config.GroupMain
	}
	g.AgentGroup = agentGroup
	g.SessionID = sess.ID
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channel_groups (channel, chat_id, agent_group, session_id, created_at, profile)
		VALUES (?, ?, ?, ?, ?, '')`, channel, chatID, g.AgentGroup, g.SessionID, now())
	if err != nil {
		return nil, fmt.Errorf("create channel group: %w", err)
	}
	g.ID, _ = res.LastInsertId()
	return g, nil
}

// SetProfile binds a named agent profile to a channel group (#71).
// profile may be empty to clear. chat may be "channel:chat_id" or just chat_id
// when unique. Each change is written to profile_audit (old, new, channel, chat).
func (s *Store) SetProfile(ctx context.Context, channel, chatID, profile string) error {
	return s.SetProfileSource(ctx, channel, chatID, profile, "cli")
}

// SetProfileSource is SetProfile with an explicit audit source (e.g. "cli", "admin").
func (s *Store) SetProfileSource(ctx context.Context, channel, chatID, profile, source string) error {
	var old string
	err := s.db.QueryRowContext(ctx, `
		SELECT profile FROM channel_groups WHERE channel = ? AND chat_id = ?`,
		channel, chatID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no channel group for %s:%s", channel, chatID)
	}
	if err != nil {
		return err
	}
	if old == profile {
		return nil
	}
	if source == "" {
		source = "cli"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE channel_groups SET profile = ? WHERE channel = ? AND chat_id = ?`,
		profile, channel, chatID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no channel group for %s:%s", channel, chatID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO profile_audit (at, channel, chat_id, old_profile, new_profile, source)
		VALUES (?, ?, ?, ?, ?, ?)`,
		now(), channel, chatID, old, profile, source); err != nil {
		return fmt.Errorf("profile audit: %w", err)
	}
	return tx.Commit()
}

// ProfileAudits lists recent profile change rows for a channel:chat (newest first).
func (s *Store) ProfileAudits(ctx context.Context, channel, chatID string, limit int) (out []ProfileAudit, err error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, at, channel, chat_id, old_profile, new_profile, source
		FROM profile_audit
		WHERE channel = ? AND chat_id = ?
		ORDER BY id DESC
		LIMIT ?`, channel, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		var a ProfileAudit
		var at string
		if err := rows.Scan(&a.ID, &at, &a.Channel, &a.ChatID, &a.OldProfile, &a.NewProfile, &a.Source); err != nil {
			return nil, err
		}
		if a.At, err = time.Parse(time.RFC3339Nano, at); err != nil && at != "" {
			return nil, fmt.Errorf("parse profile_audit at: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProfileAudit is one channel-group profile rebinding (#71).
type ProfileAudit struct {
	ID         int64
	At         time.Time
	Channel    string
	ChatID     string
	OldProfile string
	NewProfile string
	Source     string
}

// SetProfileByChat sets profile when chat_id uniquely identifies a row.
func (s *Store) SetProfileByChat(ctx context.Context, chatRef, profile string) error {
	if channel, chatID, ok := schedule.ParseTarget(chatRef); ok {
		return s.SetProfile(ctx, channel, chatID, profile)
	}
	// chat-only: must be unique
	var ch, id string
	err := s.db.QueryRowContext(ctx, `
		SELECT channel, chat_id FROM channel_groups WHERE chat_id = ?`, chatRef).
		Scan(&ch, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no channel group for chat %q", chatRef)
	}
	if err != nil {
		return err
	}
	return s.SetProfile(ctx, ch, id, profile)
}

// ChannelChatForSession returns the channel and chat id bound to a session,
// if any. Used by gateway reflection to take the same group lock as message
// handling (#59).
func (s *Store) ChannelChatForSession(ctx context.Context, sessionID string) (channel, chatID string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT channel, chat_id FROM channel_groups WHERE session_id = ?`, sessionID).Scan(&channel, &chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return channel, chatID, true, nil
}

// TargetForSession returns the channel delivery target associated with a
// conversation session, if it has one.
func (s *Store) TargetForSession(ctx context.Context, sessionID string) (string, bool, error) {
	channel, chatID, ok, err := s.ChannelChatForSession(ctx, sessionID)
	if err != nil || !ok {
		return "", ok, err
	}
	return channel + ":" + chatID, true, nil
}
