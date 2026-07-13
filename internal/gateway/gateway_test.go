package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/observability"
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
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
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
	go func() { _ = gw.Run(ctx) }()
	return gw, adapter, entities, sessions, cancel
}

func TestGatewayUsesProfileAgent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	if _, err := entities.GroupFor(ctx, "fake", "profile-chat", "main"); err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	if err := entities.SetProfile(ctx, "fake", "profile-chat", "reviewer"); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	// Re-load so Profile is populated.
	group, err := entities.GroupFor(ctx, "fake", "profile-chat", "main")
	if err != nil || group.Profile != "reviewer" {
		t.Fatalf("group = %+v err=%v", group, err)
	}
	_ = group

	defaultProv := &recordingProvider{}
	profileProv := &recordingProvider{}
	var logs bytes.Buffer
	gw := &Gateway{
		Agent: &agent.Agent{
			Provider: defaultProv,
			Tools:    tool.NewRegistry(gwNamedTool("bash")),
			System:   "main-system",
			Model:    "main-model",
		},
		Profiles: map[string]*agent.Agent{
			"reviewer": {
				Provider: profileProv,
				Tools:    tool.NewRegistry(gwNamedTool("read_file"), gwNamedTool("search")),
				System:   "reviewer-profile-system",
				Model:    "review-model",
			},
		},
		Entities: entities,
		Sessions: sessions,
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
	}

	reply, err := gw.converse(ctx, channel.Message{Channel: "fake", ChatID: "profile-chat", Text: "please review"})
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("reply = %q", reply)
	}
	if profileProv.last.System != "reviewer-profile-system" {
		t.Fatalf("system = %q, want reviewer-profile-system", profileProv.last.System)
	}
	if profileProv.last.Model != "review-model" {
		t.Fatalf("model = %q", profileProv.last.Model)
	}
	toolNames := make([]string, 0, len(profileProv.last.Tools))
	for _, d := range profileProv.last.Tools {
		toolNames = append(toolNames, d.Name)
	}
	if !strings.Contains(strings.Join(toolNames, ","), "read_file") || !strings.Contains(strings.Join(toolNames, ","), "search") {
		t.Fatalf("tools = %v, want profile toolbox", toolNames)
	}
	if strings.Contains(strings.Join(toolNames, ","), "bash") {
		t.Fatalf("tools include main bash: %v", toolNames)
	}
	if defaultProv.last.System != "" {
		t.Fatal("main agent should not have been used")
	}
	if !strings.Contains(logs.String(), "profile=reviewer") {
		t.Errorf("logs missing profile: %s", logs.String())
	}
}

func TestGatewayUnknownProfileErrors(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	if _, err := entities.GroupFor(ctx, "fake", "chat-x", "main"); err != nil {
		t.Fatal(err)
	}
	if err := entities.SetProfile(ctx, "fake", "chat-x", "missing"); err != nil {
		t.Fatal(err)
	}
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: scriptProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		Profiles: map[string]*agent.Agent{}, // non-nil: unknown must error
		Entities: entities,
		Sessions: sessions,
	}
	_, err = gw.converse(ctx, channel.Message{Channel: "fake", ChatID: "chat-x", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), `gateway: unknown profile "missing"`) {
		t.Fatalf("err = %v, want unknown profile", err)
	}
}

// recordingProvider captures the Complete request for profile-selection tests (#71).
type recordingProvider struct {
	last llm.Request
}

func (p *recordingProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.last = req
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

type gwNamedTool string

func (n gwNamedTool) Def() llm.Tool {
	return llm.Tool{Name: string(n), InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (n gwNamedTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	return string(n), nil
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
	if _, err := entities.GroupFor(ctx, "fake", "restricted-chat", ""); err != nil {
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
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
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

func TestGatewayRecordsSessionRunAndLogsSessionID(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	group, err := entities.GroupFor(ctx, "fake", "chat-1", "")
	if err != nil {
		t.Fatalf("GroupFor: %v", err)
	}
	var logs bytes.Buffer
	gw := &Gateway{
		Agent:         &agent.Agent{Provider: scriptProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		Entities:      entities,
		Sessions:      sessions,
		Observability: observability.New(st, nil),
		Log:           slog.New(slog.NewTextHandler(&logs, nil)),
	}

	if _, err := gw.converse(ctx, channel.Message{Channel: "fake", ChatID: "chat-1", Text: "hello"}); err != nil {
		t.Fatalf("converse: %v", err)
	}
	snapshot, err := gw.Observability.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].SessionID != group.SessionID || snapshot.Recent[0].Source != "gateway" || snapshot.Recent[0].Outcome != "ok" {
		t.Fatalf("recent runs = %+v", snapshot.Recent)
	}
	if !strings.Contains(logs.String(), "session_id="+group.SessionID) {
		t.Errorf("logs missing session_id: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "profile=main") || !strings.Contains(logs.String(), `msg="gateway run finished"`) {
		t.Errorf("logs missing profile/end dispatch: %s", logs.String())
	}
}

func TestGatewayRejectsUnavailablePersistedAgentGroup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	if _, err := entities.GroupFor(ctx, "fake", "restricted-chat", ""); err != nil {
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

// TestGroupUnknownSenderSilentIgnore is #34: group chats never mint pairing
// codes or reply to strangers.
func TestGroupUnknownSenderSilentIgnore(t *testing.T) {
	_, adapter, entities, _, cancel := newTestGateway(t)
	defer cancel()

	adapter.inbound <- channel.Message{
		Channel: "fake", ChatID: "-100", SenderID: "stranger", SenderName: "S",
		Text: "@waffle hi", IsGroup: true, ChatType: "supergroup",
	}
	// No reply should arrive.
	select {
	case <-adapter.wake:
		adapter.mu.Lock()
		replies := append([]string(nil), adapter.sent["-100"]...)
		adapter.mu.Unlock()
		t.Fatalf("unexpected group reply to stranger: %v", replies)
	case <-time.After(300 * time.Millisecond):
	}
	pending, err := entities.Pairings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("group contact created pairing: %v", pending)
	}
}

// TestGroupOwnerUsesRestrictedAgentTier verifies new group chats bind to the
// "group" agent tier rather than main.
func TestGroupOwnerUsesRestrictedAgentTier(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	// Pair owner identity without going through the gateway.
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO identities (channel, external_id, name, created_at) VALUES ('fake', 'owner', 'Matt', datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	adapter := newFakeAdapter()
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: namedProvider("main"), Tools: tool.NewRegistry(), Model: "m"},
		Agents:   map[string]*agent.Agent{"group": {Provider: namedProvider("group"), Tools: tool.NewRegistry(), Model: "m"}},
		Entities: entities,
		Sessions: sessions,
		Adapters: []channel.Adapter{adapter},
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = gw.Run(runCtx) }()

	adapter.inbound <- channel.Message{
		Channel: "fake", ChatID: "-99", SenderID: "owner", SenderName: "Matt",
		Text: "status?", IsGroup: true, ChatType: "group",
	}
	replies := adapter.waitForReply(t, "-99", 1)
	if replies[0] != "group: status?" {
		t.Fatalf("reply = %q, want group-tier provider", replies[0])
	}
	g, err := entities.GroupFor(ctx, "fake", "-99", "main")
	if err != nil {
		t.Fatal(err)
	}
	if g.AgentGroup != "group" {
		t.Errorf("agent_group = %q, want group", g.AgentGroup)
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

	group, err := entities.GroupFor(ctx, "fake", "c1", "")
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

	g1, _ := entities.GroupFor(ctx, "fake", "c1", "")
	g2, _ := entities.GroupFor(ctx, "fake", "c2", "")
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
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
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
	group, err := entities.GroupFor(bgCtx, "fake", "c1", "")
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

type drainBlockingProvider struct {
	started  chan struct{}
	canceled chan struct{}
}

type nonCooperativeAdapter struct {
	release chan struct{}
}

func (a *nonCooperativeAdapter) Name() string { return "stuck-adapter" }
func (a *nonCooperativeAdapter) Run(context.Context, chan<- channel.Message) error {
	<-a.release
	return nil
}
func (a *nonCooperativeAdapter) Send(context.Context, string, string) error { return nil }

func TestGatewayBoundsNonCooperativeAdapterShutdown(t *testing.T) {
	a := &nonCooperativeAdapter{release: make(chan struct{})}
	gw := &Gateway{Agent: &agent.Agent{Provider: scriptProvider{}, Tools: tool.NewRegistry(), Model: "m"}, Adapters: []channel.Adapter{a}, DrainTimeout: 20 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		close(a.release)
		t.Fatal("non-cooperative adapter blocked shutdown")
	}
	close(a.release)
}

type cancelCleanupProvider struct {
	started    chan struct{}
	canceled   chan struct{}
	finish     chan struct{}
	cleaned    chan struct{}
	cleanup    func() error
	cleanupErr chan error
}

func (p *cancelCleanupProvider) Complete(ctx context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	close(p.started)
	<-ctx.Done()
	close(p.canceled)
	<-p.finish
	if p.cleanup != nil {
		p.cleanupErr <- p.cleanup()
	}
	close(p.cleaned)
	return nil, ctx.Err()
}

func TestGatewayWaitsForContextAwareHandlerCleanupAfterCancel(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	adapter := newFakeAdapter()
	p := &cancelCleanupProvider{started: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan struct{}), cleaned: make(chan struct{}), cleanup: st.DB.Ping, cleanupErr: make(chan error, 1)}
	gw := &Gateway{Agent: &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}, Entities: entities, Sessions: sessions, Adapters: []channel.Adapter{adapter}, DrainTimeout: 20 * time.Millisecond, PostCancelGrace: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "cleanup", SenderID: "owner", Text: "pair"}
	adapter.waitForReply(t, "cleanup", 1)
	pending, _ := entities.Pairings(context.Background())
	if _, err := entities.Approve(context.Background(), pending[0].Code, "Matt"); err != nil {
		t.Fatal(err)
	}
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "cleanup", SenderID: "owner", Text: "run"}
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("provider not started")
	}
	cancel()
	select {
	case <-p.canceled:
	case <-time.After(time.Second):
		t.Fatal("handler context not canceled")
	}
	select {
	case err := <-done:
		t.Fatalf("gateway returned before handler cleanup: %v", err)
	default:
	}
	close(p.finish)
	select {
	case <-p.cleaned:
	case <-time.After(time.Second):
		t.Fatal("provider cleanup did not finish")
	}
	if err := <-p.cleanupErr; err != nil {
		t.Fatalf("handler cleanup observed closed store: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway did not return after cleanup")
	}
	// Closing shared storage after Run is now ordered after handler exit.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Ping(); err == nil {
		t.Fatal("test store remained open")
	}
}

func TestGatewayHoldsResourcesForTrulyNonCooperativeHandler(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	adapter := newFakeAdapter()
	p := &slowProvider{inFlight: make(chan struct{}), ready: make(chan struct{})}
	gw := &Gateway{Agent: &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}, Entities: entities, Sessions: sessions, Adapters: []channel.Adapter{adapter}, DrainTimeout: 10 * time.Millisecond, PostCancelGrace: 10 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "noncoop", SenderID: "owner", Text: "pair"}
	adapter.waitForReply(t, "noncoop", 1)
	pending, _ := entities.Pairings(context.Background())
	if _, err := entities.Approve(context.Background(), pending[0].Code, "Matt"); err != nil {
		t.Fatal(err)
	}
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "noncoop", SenderID: "owner", Text: "run"}
	<-p.inFlight
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned while non-cooperative handler could still use resources: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := st.DB.Ping(); err != nil {
		t.Fatalf("shared store was not held open: %v", err)
	}
	close(p.ready)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway did not return after non-cooperative handler exited")
	}
}

type cancelEmitAdapter struct {
	mu   sync.Mutex
	sent int
}

func (a *cancelEmitAdapter) Name() string { return "cancel-race" }
func (a *cancelEmitAdapter) Run(ctx context.Context, inbound chan<- channel.Message) error {
	<-ctx.Done()
	inbound <- channel.Message{Channel: a.Name(), ChatID: "buffered", SenderID: "stranger", Text: "too late"}
	return nil
}
func (a *cancelEmitAdapter) Send(context.Context, string, string) error {
	a.mu.Lock()
	a.sent++
	a.mu.Unlock()
	return nil
}

func TestGatewayCanceledContextDoesNotAcceptBufferedMessage(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	a := &cancelEmitAdapter{}
	gw := &Gateway{Agent: &agent.Agent{Provider: scriptProvider{}, Tools: tool.NewRegistry(), Model: "m"}, Entities: entity.New(st, session.New(st)), Sessions: session.New(st), Adapters: []channel.Adapter{a}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := gw.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	sent := a.sent
	a.mu.Unlock()
	if sent != 0 {
		t.Fatalf("accepted %d buffered messages after cancellation", sent)
	}
}

func (p *drainBlockingProvider) Complete(ctx context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	close(p.started)
	<-ctx.Done()
	close(p.canceled)
	return nil, ctx.Err()
}

func TestGracefulShutdownBoundsHungHandlerDrain(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	adapter := newFakeAdapter()
	provider := &drainBlockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: provider, Tools: tool.NewRegistry(), Model: "m"},
		Entities: entities, Sessions: sessions, Adapters: []channel.Adapter{adapter},
		DrainTimeout: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "hung", SenderID: "owner", Text: "pair"}
	adapter.waitForReply(t, "hung", 1)
	pending, err := entities.Pairings(context.Background())
	if err != nil || len(pending) == 0 {
		t.Fatalf("pairings=%v err=%v", pending, err)
	}
	if _, err := entities.Approve(context.Background(), pending[0].Code, "Matt"); err != nil {
		t.Fatal(err)
	}
	adapter.inbound <- channel.Message{Channel: "fake", ChatID: "hung", SenderID: "owner", Text: "hang"}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("drain context was not canceled")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway remained blocked after drain timeout")
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

// reflectProvider returns a fixed summary and counts Complete calls.
type reflectProvider struct {
	reply string
	err   error
	calls int
}

func (p *reflectProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	// Echo path for normal agent turns (last message is user text, not ReflectPrompt).
	last := ""
	if len(req.Messages) > 0 {
		last = req.Messages[len(req.Messages)-1].Text()
	}
	if strings.Contains(last, "Summarize it in 2-3 sentences") {
		return &llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: p.reply}}},
			StopReason: llm.StopEndTurn,
		}, nil
	}
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "echo: " + last}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func TestReflectEveryTurnsWritesSummary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	p := &reflectProvider{reply: "worked on every-N reflection"}
	gw := &Gateway{
		Agent:             &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"},
		Entities:          entities,
		Sessions:          sessions,
		ReflectEveryTurns: 2,
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Two turns total after one converse (user+assistant) — need 2 converses for turn count 4?
	// TurnCount is messages persisted: each converse adds user + assistant = 2 turns.
	// ReflectEveryTurns=2 fires when n%2==0 after first converse (n=2).
	if _, err := gw.converse(ctx, channel.Message{Channel: "fake", ChatID: "reflect-chat", Text: "hello"}); err != nil {
		t.Fatalf("converse: %v", err)
	}
	group, err := entities.GroupFor(ctx, "fake", "reflect-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.Get(ctx, group.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sess.Summary, "every-N") {
		t.Fatalf("summary = %q, want every-N reflection summary", sess.Summary)
	}
}

func TestReflectSessionFailureLoggedNoCrash(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	p := &reflectProvider{err: errors.New("provider down")}
	var logs bytes.Buffer
	gw := &Gateway{
		Agent:             &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"},
		Entities:          entities,
		Sessions:          sessions,
		ReflectEveryTurns: 2,
		Log:               slog.New(slog.NewTextHandler(&logs, nil)),
	}
	// Provider fails on every Complete including the main agent turn — use a
	// hybrid: first succeed for agent, fail for reflect.
	// Use converse with scriptProvider path by setting ReflectEveryTurns and
	// failing only on ReflectPrompt via reflectProvider with err always set.
	// That also fails agent turn. So call ReflectSession after seeding turns.
	group, err := entities.GroupFor(ctx, "fake", "fail-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = sessions.AppendTurn(ctx, group.SessionID, llm.UserText("a"))
	_ = sessions.AppendTurn(ctx, group.SessionID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "b"}},
	})
	wrote, err := gw.ReflectSession(ctx, group.SessionID)
	if wrote {
		t.Fatal("expected no write on failure")
	}
	if err == nil {
		t.Fatal("expected reflection error")
	}
	if !strings.Contains(logs.String(), "session reflection failed") {
		t.Fatalf("logs missing failure: %s", logs.String())
	}
	// Process continues; second call still safe.
	_, _ = gw.ReflectSession(ctx, group.SessionID)
}

func TestReflectSessionSkipsWhenGroupLocked(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	group, err := entities.GroupFor(ctx, "fake", "busy-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = sessions.AppendTurn(ctx, group.SessionID, llm.UserText("a"))
	_ = sessions.AppendTurn(ctx, group.SessionID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "b"}},
	})
	p := &reflectProvider{reply: "should not write while locked"}
	gw := &Gateway{
		Agent:    &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"},
		Entities: entities,
		Sessions: sessions,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Hold the same group lock message handling would hold.
	unlock := gw.lockGroup("fake\x00busy-chat")
	defer unlock()
	wrote, err := gw.ReflectSession(ctx, group.SessionID)
	if err != nil {
		t.Fatalf("ReflectSession: %v", err)
	}
	if wrote {
		t.Fatal("expected skip while locked")
	}
	if p.calls != 0 {
		t.Fatalf("provider calls=%d, want 0 while locked", p.calls)
	}
}
