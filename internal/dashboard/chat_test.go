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

type fakeChatBackend struct {
	mu          sync.Mutex
	openCalls   int
	turnCalls   int
	closeCalls  int
	turnStarted chan struct{}
	releaseTurn chan struct{}
	closeCalled chan struct{}
	turnEvent   chat.Event
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
	emit(chat.Event{Kind: chat.EventTurnDone})
	return nil
}

func (f *fakeChatBackend) Command(context.Context, chat.ParsedCommand, func(chat.Event)) (chat.Result, error) {
	return chat.Result{}, nil
}

func (f *fakeChatBackend) Cancel() {}

func (f *fakeChatBackend) Close(context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	if f.closeCalled != nil {
		close(f.closeCalled)
	}
	return nil
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
