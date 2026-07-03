package entity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	return New(st, session.New(st))
}

func TestPairingFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Unknown sender.
	if _, err := s.Identify(ctx, "telegram", "12345"); !errors.Is(err, ErrUnknownSender) {
		t.Fatalf("Identify unknown = %v, want ErrUnknownSender", err)
	}

	// First contact creates a pairing; re-contact reuses it.
	p1, err := s.Pair(ctx, "telegram", "12345", "Matt", "chat-1")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if len(p1.Code) != 6 {
		t.Errorf("code = %q", p1.Code)
	}
	p2, err := s.Pair(ctx, "telegram", "12345", "Matt", "chat-1")
	if err != nil || p2.Code != p1.Code {
		t.Errorf("re-pair minted new code: %v vs %v (%v)", p2, p1, err)
	}

	pending, err := s.Pairings(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pairings = %v, %v", pending, err)
	}

	// Approve promotes to identity and consumes the pairing.
	id, err := s.Approve(ctx, p1.Code, "")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if id.Name != "Matt" || id.Channel != "telegram" || id.ExternalID != "12345" {
		t.Errorf("identity = %+v", id)
	}
	got, err := s.Identify(ctx, "telegram", "12345")
	if err != nil || got.Name != "Matt" {
		t.Errorf("Identify after approve = %+v, %v", got, err)
	}
	if pending, _ := s.Pairings(ctx); len(pending) != 0 {
		t.Errorf("pairing not consumed: %v", pending)
	}

	// Unknown code errors.
	if _, err := s.Approve(ctx, "ZZZZZZ", ""); err == nil {
		t.Error("approve of unknown code succeeded")
	}
}

func TestGroupForCreatesOnceAndBindsSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	g1, err := s.GroupFor(ctx, "telegram", "chat-1")
	if err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	if g1.SessionID == "" || g1.AgentGroup != "main" {
		t.Errorf("group = %+v", g1)
	}
	g2, err := s.GroupFor(ctx, "telegram", "chat-1")
	if err != nil || g2.SessionID != g1.SessionID || g2.ID != g1.ID {
		t.Errorf("second GroupFor = %+v, %v; want same as %+v", g2, err, g1)
	}

	other, err := s.GroupFor(ctx, "telegram", "chat-2")
	if err != nil || other.SessionID == g1.SessionID {
		t.Errorf("distinct chats share a session: %+v vs %+v (%v)", other, g1, err)
	}
}
