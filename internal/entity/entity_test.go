package entity

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
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

	g1, err := s.GroupFor(ctx, "telegram", "chat-1", "")
	if err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	if g1.SessionID == "" || g1.AgentGroup != "main" {
		t.Errorf("group = %+v", g1)
	}
	g2, err := s.GroupFor(ctx, "telegram", "chat-1", "group")
	if err != nil || g2.SessionID != g1.SessionID || g2.ID != g1.ID || g2.AgentGroup != "main" {
		// Second call must not rewrite agent_group on an existing row.
		t.Errorf("second GroupFor = %+v, %v; want same as %+v", g2, err, g1)
	}

	other, err := s.GroupFor(ctx, "telegram", "chat-2", "")
	if err != nil || other.SessionID == g1.SessionID {
		t.Errorf("distinct chats share a session: %+v vs %+v (%v)", other, g1, err)
	}
}

func TestSetProfileOnChannelGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	g, err := s.GroupFor(ctx, "telegram", "42", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Profile != "" {
		t.Fatalf("default profile = %q", g.Profile)
	}
	if err := s.SetProfile(ctx, "telegram", "42", "reviewer"); err != nil {
		t.Fatal(err)
	}
	again, err := s.GroupFor(ctx, "telegram", "42", "")
	if err != nil || again.Profile != "reviewer" {
		t.Fatalf("after set = %+v %v", again, err)
	}
	if err := s.SetProfileByChat(ctx, "telegram:42", ""); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.GroupFor(ctx, "telegram", "42", "")
	if err != nil || cleared.Profile != "" {
		t.Fatalf("cleared = %+v %v", cleared, err)
	}
}

func TestProfileChangeAudit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.GroupFor(ctx, "telegram", "99", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProfileSource(ctx, "telegram", "99", "reviewer", "cli"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProfileSource(ctx, "telegram", "99", "researcher", "admin"); err != nil {
		t.Fatal(err)
	}
	// No-op same profile: no new audit row.
	if err := s.SetProfileSource(ctx, "telegram", "99", "researcher", "cli"); err != nil {
		t.Fatal(err)
	}
	audits, err := s.ProfileAudits(ctx, "telegram", "99", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 {
		t.Fatalf("audits = %d, want 2 (noop not recorded): %+v", len(audits), audits)
	}
	// Newest first.
	if audits[0].OldProfile != "reviewer" || audits[0].NewProfile != "researcher" || audits[0].Source != "admin" {
		t.Fatalf("latest = %+v", audits[0])
	}
	if audits[1].OldProfile != "" || audits[1].NewProfile != "reviewer" || audits[1].Source != "cli" {
		t.Fatalf("first = %+v", audits[1])
	}
	if audits[0].Channel != "telegram" || audits[0].ChatID != "99" {
		t.Fatalf("channel/chat = %+v", audits[0])
	}
	if audits[0].At.IsZero() {
		t.Fatal("timestamp missing")
	}
}

func TestGroupForCreatesGroupChatOnRestrictedTier(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	g, err := s.GroupFor(ctx, "telegram", "-1001", "group")
	if err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	if g.AgentGroup != "group" {
		t.Errorf("agent_group = %q, want group", g.AgentGroup)
	}
	// Re-fetch with a different requested tier keeps the stored binding.
	again, err := s.GroupFor(ctx, "telegram", "-1001", "main")
	if err != nil || again.AgentGroup != "group" || again.SessionID != g.SessionID {
		t.Errorf("re-fetch = %+v, %v; want agent_group=group session=%s", again, err, g.SessionID)
	}
}

func TestPairIsIdempotentUnderConcurrentFirstContact(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.db.SetMaxOpenConns(2)

	for attempt := 0; attempt < 200; attempt++ {
		start := make(chan struct{})
		results := make(chan string, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p, err := s.Pair(ctx, "telegram", fmt.Sprintf("race-%d", attempt), "Matt", "chat-race")
				if err != nil {
					errs <- err
					return
				}
				results <- p.Code
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		var gotErrs []error
		for err := range errs {
			gotErrs = append(gotErrs, err)
		}
		if len(gotErrs) != 0 {
			t.Fatalf("Pair returned errors under concurrent first contact: %v", gotErrs)
		}

		var codes []string
		for code := range results {
			codes = append(codes, code)
		}
		if len(codes) != 2 {
			t.Fatalf("got %d pairing codes, want 2", len(codes))
		}
		if codes[0] != codes[1] {
			t.Fatalf("concurrent Pair returned different codes: %q vs %q", codes[0], codes[1])
		}
	}
}

// TestGroupForIsAtomicUnderConcurrentFirstTouch asserts concurrent first
// contact for the same conversation never orphans a session row: the session
// and channel group are created in one transaction, and the loser of the
// UNIQUE(channel, chat_id) race returns the winner's group (#290).
func TestGroupForIsAtomicUnderConcurrentFirstTouch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.db.SetMaxOpenConns(2)

	for attempt := 0; attempt < 100; attempt++ {
		chat := fmt.Sprintf("race-%d", attempt)
		start := make(chan struct{})
		results := make(chan *Group, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				g, err := s.GroupFor(ctx, "telegram", chat, "")
				if err != nil {
					errs <- err
					return
				}
				results <- g
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		for err := range errs {
			t.Fatalf("GroupFor error under concurrent first contact: %v", err)
		}
		var groups []*Group
		for g := range results {
			groups = append(groups, g)
		}
		if len(groups) != 2 {
			t.Fatalf("got %d groups, want 2", len(groups))
		}
		if groups[0].ID != groups[1].ID || groups[0].SessionID != groups[1].SessionID {
			t.Fatalf("concurrent GroupFor returned different groups: %+v vs %+v", groups[0], groups[1])
		}

		// Exactly one session row exists for this conversation — the loser's
		// session must not survive as an orphan.
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sessions WHERE title = ?`, "telegram "+chat).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("sessions with title %q = %d, want 1 (no orphan)", "telegram "+chat, n)
		}
	}
}
