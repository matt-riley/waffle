package dashboard

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

func TestChatClientDoesNotRetryTurnAfterDisconnect(t *testing.T) {
	backend := &fakeChatBackend{}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := clients.Turn(context.Background(), client, "ship it"); err != nil {
		t.Fatal(err)
	}
	if err := clients.Close(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if backend.turnCalls != 1 {
		t.Fatalf("turn calls = %d, want 1", backend.turnCalls)
	}
}

func TestChatClientCloseWaitsForActiveTurn(t *testing.T) {
	backend := &fakeChatBackend{turnStarted: make(chan struct{}), releaseTurn: make(chan struct{}), closeCalled: make(chan struct{})}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turnDone := make(chan error, 1)
	go func() { turnDone <- clients.Turn(context.Background(), client, "hold") }()
	<-backend.turnStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- clients.Close(context.Background(), client) }()
	select {
	case <-backend.closeCalled:
		t.Fatal("Close ran before the active turn finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.releaseTurn)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
}

func TestChatClientTurnReapsIdleClient(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	backend := &fakeChatBackend{}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	clients.now = func() time.Time { return now }
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30*time.Minute + time.Nanosecond)
	err = clients.Turn(context.Background(), client, "too late")
	if !errors.Is(err, errChatClientNotFound) {
		t.Fatalf("Turn error = %v, want client not found", err)
	}
	if backend.closeCount() != 1 {
		t.Fatalf("close calls = %d, want 1", backend.closeCount())
	}
	if backend.turnCount() != 0 {
		t.Fatalf("turn calls = %d, want 0", backend.turnCount())
	}
}

func TestChatClientPublishesSanitizedEvents(t *testing.T) {
	canary := "sk-browser-event-secret"
	backend := &fakeChatBackend{turnEvent: chat.Event{Kind: chat.EventNotice, Text: "safe " + canary, ToolName: "/var/lib/waffle/private"}}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{4}, 32)))
	hub := NewEventHub(4)
	clients.events = hub
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := clients.Turn(context.Background(), client, "not an event payload"); err != nil {
		t.Fatal(err)
	}
	events, resync := hub.Subscribe(0)
	if resync {
		t.Fatal("unexpected event resync")
	}
	defer hub.Unsubscribe(events)
	event := <-events
	if event.Resource != "chat" || event.ResourceID != client || event.Type != string(chat.EventNotice) {
		t.Fatalf("event = %+v, want chat event for client", event)
	}
	data := string(event.Data)
	if !strings.Contains(data, "safe") {
		t.Fatalf("event data = %q, want safe text", data)
	}
	for _, leaked := range []string{canary, "/var/lib/waffle", "not an event payload"} {
		if strings.Contains(data, leaked) {
			t.Fatalf("event data leaked %q: %q", leaked, data)
		}
	}
}

func TestChatClientPublishesSafeErrorAndStateEvents(t *testing.T) {
	canary := "provider failed sk-event-secret /var/lib/waffle/private prompt: ship it"
	backend := &fakeChatBackend{turnEvents: []chat.Event{
		{Kind: chat.EventNotice, IsError: true, Text: canary},
		{Kind: chat.EventState, State: &chat.State{Title: "ready", Workspace: "/var/lib/waffle/work", History: []llm.Message{{}}}},
	}}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	hub := NewEventHub(4)
	clients.SetEventHub(hub)
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := clients.Turn(context.Background(), client, "ship it"); err != nil {
		t.Fatal(err)
	}
	events, _ := hub.Subscribe(0)
	defer hub.Unsubscribe(events)
	first, second := <-events, <-events
	if strings.Contains(string(first.Data), canary) || !strings.Contains(string(first.Data), "chat operation failed") {
		t.Fatalf("error event = %s", first.Data)
	}
	if !strings.Contains(string(second.Data), `"state"`) || strings.Contains(string(second.Data), "History") || strings.Contains(string(second.Data), "/var/lib/waffle") {
		t.Fatalf("state event = %s", second.Data)
	}
}

func TestChatClientsShutdownWaitsForActiveTurn(t *testing.T) {
	backend := &fakeChatBackend{turnStarted: make(chan struct{}), releaseTurn: make(chan struct{}), closeCalled: make(chan struct{})}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{6}, 32)))
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turnDone := make(chan error, 1)
	go func() { turnDone <- clients.Turn(context.Background(), client, "hold") }()
	<-backend.turnStarted
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- clients.Shutdown(context.Background()) }()
	select {
	case <-backend.closeCalled:
		t.Fatal("Shutdown closed a backend before its active turn finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.releaseTurn)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err == nil {
		t.Fatal("Open succeeded after shutdown")
	}
}

func TestChatClientRetriesIDCollisionWithoutReplacingExistingBackend(t *testing.T) {
	first, second := &fakeChatBackend{}, &fakeChatBackend{}
	backends := []chat.Backend{first, second, &fakeChatBackend{}}
	index := 0
	ids := append(bytes.Repeat([]byte{0}, 16), append(bytes.Repeat([]byte{0}, 16), bytes.Repeat([]byte{1}, 16)...)...)
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { backend := backends[index]; index++; return backend, nil }, bytes.NewReader(ids))
	firstID, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatalf("client ID collision was reused: %q", firstID)
	}
	if err := clients.Close(context.Background(), firstID); err != nil {
		t.Fatal(err)
	}
	if first.closeCount() != 1 || second.closeCount() != 0 {
		t.Fatalf("close counts = %d, %d", first.closeCount(), second.closeCount())
	}
}

func TestChatClientReapClosesEveryExpiredBackendAfterErrors(t *testing.T) {
	now := time.Now()
	first, second := &fakeChatBackend{closeErr: errors.New("first close")}, &fakeChatBackend{closeErr: errors.New("second close")}
	backends := []chat.Backend{first, second, &fakeChatBackend{}}
	i := 0
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { b := backends[i]; i++; return b, nil }, bytes.NewReader(append(bytes.Repeat([]byte{2}, 16), bytes.Repeat([]byte{3}, 16)...)))
	clients.now = func() time.Time { return now }
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err == nil {
		t.Fatal("Open succeeded despite reap close error")
	}
	if first.closeCount() != 1 || second.closeCount() != 1 {
		t.Fatalf("reap close counts = %d, %d", first.closeCount(), second.closeCount())
	}
}

func TestChatClientCloseFinalizesAfterCallerCancellation(t *testing.T) {
	backend := &fakeChatBackend{turnStarted: make(chan struct{}), releaseTurn: make(chan struct{}), closeCalled: make(chan struct{})}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{8}, 32)))
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = clients.Turn(context.Background(), client, "hold") }()
	<-backend.turnStarted
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clients.Close(ctx, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v", err)
	}
	close(backend.releaseTurn)
	select {
	case <-backend.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("cancelled Close did not finalize backend")
	}
	if err := clients.Cancel(client); !errors.Is(err, errChatClientNotFound) {
		t.Fatalf("Cancel after close = %v", err)
	}
}

func TestChatClientRejectsConcurrentCommandAndPropagatesCancel(t *testing.T) {
	backend := &fakeChatBackend{turnStarted: make(chan struct{}), releaseTurn: make(chan struct{})}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{10}, 32)))
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = clients.Turn(context.Background(), client, "hold") }()
	<-backend.turnStarted
	if _, err := clients.Command(context.Background(), client, chat.ParsedCommand{Name: chat.CommandStatus}); !errors.Is(err, errChatTurnActive) {
		t.Fatalf("Command error = %v", err)
	}
	if err := clients.Cancel(client); err != nil {
		t.Fatal(err)
	}
	if backend.cancelCount() != 1 {
		t.Fatalf("cancel calls = %d", backend.cancelCount())
	}
	close(backend.releaseTurn)
}

func TestChatClientEnforces64ClientCap(t *testing.T) {
	created := 0
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { created++; return &fakeChatBackend{}, nil }, nil)
	for i := 0; i < defaultChatClientLimit; i++ {
		if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); !errors.Is(err, errChatUnavailable) {
		t.Fatalf("65th open = %v", err)
	}
	if created != defaultChatClientLimit {
		t.Fatalf("backends = %d", created)
	}
}

type fakeChatBackend struct {
	mu          sync.Mutex
	openCalls   int
	turnCalls   int
	closeCalls  int
	turnStarted chan struct{}
	releaseTurn chan struct{}
	closeCalled chan struct{}
	turnEvent   chat.Event
	turnEvents  []chat.Event
	closeErr    error
	cancels     int
}

func (f *fakeChatBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	f.mu.Lock()
	f.openCalls++
	f.mu.Unlock()
	return chat.State{SessionID: "session"}, nil
}

func (f *fakeChatBackend) Turn(_ context.Context, _ string, emit func(chat.Event)) error {
	f.mu.Lock()
	f.turnCalls++
	f.mu.Unlock()
	if f.turnStarted != nil {
		close(f.turnStarted)
		<-f.releaseTurn
	}
	if f.turnEvent.Kind != "" {
		emit(f.turnEvent)
	}
	for _, event := range f.turnEvents {
		emit(event)
	}
	emit(chat.Event{Kind: chat.EventTurnDone})
	return f.closeErr
}

func (f *fakeChatBackend) Command(context.Context, chat.ParsedCommand, func(chat.Event)) (chat.Result, error) {
	return chat.Result{}, nil
}

func (f *fakeChatBackend) Cancel() { f.mu.Lock(); f.cancels++; f.mu.Unlock() }

func (f *fakeChatBackend) Close(context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	if f.closeCalled != nil {
		close(f.closeCalled)
	}
	return f.closeErr
}

func (f *fakeChatBackend) turnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.turnCalls
}

func (f *fakeChatBackend) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openCalls
}

func (f *fakeChatBackend) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

func (f *fakeChatBackend) cancelCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.cancels }
