package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

// fakeAdapter is an in-memory channel.
type fakeAdapter struct {
	inbound chan channel.Message

	mu   sync.Mutex
	sent map[string][]string // chatID → replies
	wake chan struct{}
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{
		inbound: make(chan channel.Message, 8),
		sent:    map[string][]string{},
		wake:    make(chan struct{}, 64),
	}
}

func (f *fakeAdapter) Name() string { return "fake" }

func (f *fakeAdapter) Run(ctx context.Context, inbound chan<- channel.Message) error {
	for {
		select {
		case m := <-f.inbound:
			inbound <- m
		case <-ctx.Done():
			return nil
		}
	}
}

func (f *fakeAdapter) Send(ctx context.Context, chatID, text string) error {
	f.mu.Lock()
	f.sent[chatID] = append(f.sent[chatID], text)
	f.mu.Unlock()
	f.wake <- struct{}{}
	return nil
}

func (f *fakeAdapter) waitForReply(t *testing.T, chatID string, n int) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		replies := append([]string(nil), f.sent[chatID]...)
		f.mu.Unlock()
		if len(replies) >= n {
			return replies
		}
		select {
		case <-f.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for reply %d on %s (have %v)", n, chatID, replies)
		}
	}
}

// scriptProvider replies with canned text incorporating the last user text.
type scriptProvider struct{}

func (scriptProvider) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].Text()
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "echo: " + last}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func newTestGateway(t *testing.T) (*Gateway, *fakeAdapter, *entity.Store, *session.Store, context.CancelFunc) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	adapter := newFakeAdapter()
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: scriptProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		Entities: entities,
		Sessions: sessions,
		Adapters: []channel.Adapter{adapter},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go gw.Run(ctx) //nolint:errcheck // stopped via cancel
	return gw, adapter, entities, sessions, cancel
}

func TestUnknownSenderGetsPairingCodeOnly(t *testing.T) {
	_, adapter, entities, _, cancel := newTestGateway(t)
	defer cancel()

	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "stranger", SenderName: "S", Text: "hi"}
	replies := adapter.waitForReply(t, "c1", 1)
	if !strings.Contains(replies[0], "Pairing code:") {
		t.Fatalf("reply = %q", replies[0])
	}
	// The agent never ran: no echo.
	if strings.Contains(replies[0], "echo:") {
		t.Fatalf("agent answered a stranger: %q", replies[0])
	}
	pending, err := entities.Pairings(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pairings = %v, %v", pending, err)
	}
}

func TestOwnerConversationPersistsAndReplies(t *testing.T) {
	_, adapter, entities, sessions, cancel := newTestGateway(t)
	defer cancel()
	ctx := context.Background()

	// Pair the owner via the normal flow.
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner-1", SenderName: "Matt", Text: "hello?"}
	adapter.waitForReply(t, "c1", 1)
	pending, _ := entities.Pairings(ctx)
	if _, err := entities.Approve(ctx, pending[0].Code, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Now the same sender is the owner: messages reach the agent.
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner-1", SenderName: "Matt", Text: "first message"}
	replies := adapter.waitForReply(t, "c1", 2)
	if replies[1] != "echo: first message" {
		t.Fatalf("reply = %q", replies[1])
	}

	// Second message continues the same persisted session.
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner-1", SenderName: "Matt", Text: "second message"}
	adapter.waitForReply(t, "c1", 3)

	group, err := entities.GroupFor(ctx, "fake", "c1")
	if err != nil {
		t.Fatal(err)
	}
	turns, err := sessions.Turns(ctx, group.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant, user, assistant
	if len(turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(turns))
	}
	if turns[2].Text() != "second message" || turns[3].Text() != "echo: second message" {
		t.Errorf("history = %q / %q", turns[2].Text(), turns[3].Text())
	}
}

func TestDistinctChatsGetDistinctSessions(t *testing.T) {
	_, adapter, entities, _, cancel := newTestGateway(t)
	defer cancel()
	ctx := context.Background()

	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "o", Text: "x"}
	adapter.waitForReply(t, "c1", 1)
	pending, _ := entities.Pairings(ctx)
	if _, err := entities.Approve(ctx, pending[0].Code, "Matt"); err != nil {
		t.Fatal(err)
	}

	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "o", Text: "in chat one"}
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c2", SenderID: "o", Text: "in chat two"}
	adapter.waitForReply(t, "c1", 2)
	adapter.waitForReply(t, "c2", 1)

	g1, _ := entities.GroupFor(ctx, "fake", "c1")
	g2, _ := entities.GroupFor(ctx, "fake", "c2")
	if g1.SessionID == g2.SessionID {
		t.Error("chats share a session")
	}
}

func TestCompletedConversationReleasesGroupLock(t *testing.T) {
	gw, adapter, entities, _, cancel := newTestGateway(t)
	defer cancel()
	ctx := context.Background()

	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner", Text: "pair"}
	adapter.waitForReply(t, "c1", 1)
	pending, err := entities.Pairings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entities.Approve(ctx, pending[0].Code, "Matt"); err != nil {
		t.Fatal(err)
	}

	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner", Text: "hello"}
	adapter.waitForReply(t, "c1", 2)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		remaining := len(gw.groups)
		gw.mu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	gw.mu.Lock()
	remaining := len(gw.groups)
	gw.mu.Unlock()
	t.Fatalf("completed conversation left %d group lock entries", remaining)
}
