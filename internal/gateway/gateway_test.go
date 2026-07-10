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

// namedProvider identifies the agent group that produced a response.
type namedProvider string

func (p namedProvider) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].Text()
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: string(p) + ": " + last}}},
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

func TestGatewayUsesPersistedAgentGroup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")

	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	if _, err := entities.GroupFor(ctx, "fake", "restricted-chat"); err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE channel_groups SET agent_group = 'restricted' WHERE channel = 'fake' AND chat_id = 'restricted-chat'`); err != nil {
		t.Fatalf("bind restricted group: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	sessions = session.New(st)
	entities = entity.New(st, sessions)
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: namedProvider("main"), Tools: tool.NewRegistry(), Model: "m"},
		Agents:   map[string]*agent.Agent{"restricted": {Provider: namedProvider("restricted"), Tools: tool.NewRegistry(), Model: "m"}},
		Entities: entities,
		Sessions: sessions,
	}

	reply, err := gw.converse(ctx, channel.Message{Channel: "fake", ChatID: "restricted-chat", Text: "hello"})
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if reply != "restricted: hello" {
		t.Fatalf("reply = %q, want restricted provider reply", reply)
	}
}

func TestGatewayRejectsUnavailablePersistedAgentGroup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	if _, err := entities.GroupFor(ctx, "fake", "restricted-chat"); err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE channel_groups SET agent_group = 'restricted' WHERE channel = 'fake' AND chat_id = 'restricted-chat'`); err != nil {
		t.Fatalf("bind restricted group: %v", err)
	}
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: namedProvider("main"), Tools: tool.NewRegistry(), Model: "m"},
		Entities: entities,
		Sessions: sessions,
	}

	_, err = gw.converse(ctx, channel.Message{Channel: "fake", ChatID: "restricted-chat", Text: "hello"})
	if err == nil || err.Error() != "gateway: no agent configured for group restricted" {
		t.Fatalf("converse error = %v, want unavailable restricted group error", err)
	}
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

// slowProvider blocks until ready is closed, then replies like scriptProvider.
// inFlight is closed when Complete is entered so callers can synchronize.
type slowProvider struct {
	inFlight chan struct{}
	ready    chan struct{}
}

func (p *slowProvider) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	close(p.inFlight)
	<-p.ready
	last := req.Messages[len(req.Messages)-1].Text()
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "echo: " + last}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

// TestGracefulShutdownPersistsTurn verifies that canceling the gateway context
// while a turn is in-flight still results in that turn being fully executed and
// persisted (i.e. the drain path uses a detached context, not the canceled one).
func TestGracefulShutdownPersistsTurn(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	adapter := newFakeAdapter()

	provider := &slowProvider{
		inFlight: make(chan struct{}),
		ready:    make(chan struct{}),
	}
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: provider, Tools: tool.NewRegistry(), Model: "m"},
		Entities: entities,
		Sessions: sessions,
		Adapters: []channel.Adapter{adapter},
	}

	ctx, cancel := context.WithCancel(context.Background())
	gwDone := make(chan error, 1)
	go func() { gwDone <- gw.Run(ctx) }()

	bgCtx := context.Background()

	// Pair the owner.
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner", SenderName: "O", Text: "hi"}
	adapter.waitForReply(t, "c1", 1)
	pending, err := entities.Pairings(bgCtx)
	if err != nil || len(pending) == 0 {
		t.Fatalf("Pairings: %v %v", pending, err)
	}
	if _, err := entities.Approve(bgCtx, pending[0].Code, "Matt"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Send a message that will block inside the LLM provider.
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "c1", SenderID: "owner", SenderName: "O", Text: "in-flight message"}

	// Wait until the handler is truly in-flight (inside Complete).
	select {
	case <-provider.inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handler to enter Complete")
	}

	// Simulate SIGTERM: cancel the gateway context.
	cancel()

	// Unblock the LLM provider so the turn can complete.
	close(provider.ready)

	// The gateway must drain and return.
	select {
	case <-gwDone:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not drain after shutdown")
	}

	// The in-flight turn must have been persisted.
	group, err := entities.GroupFor(bgCtx, "fake", "c1")
	if err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	turns, err := sessions.Turns(bgCtx, group.SessionID)
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	// Expect: user + assistant = 2 turns (the pairing message has no session).
	if len(turns) != 2 {
		t.Fatalf("expected 2 persisted turns, got %d", len(turns))
	}
	if turns[0].Text() != "in-flight message" {
		t.Errorf("turns[0] = %q, want %q", turns[0].Text(), "in-flight message")
	}
	if turns[1].Text() != "echo: in-flight message" {
		t.Errorf("turns[1] = %q, want %q", turns[1].Text(), "echo: in-flight message")
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
