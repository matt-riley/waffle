package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/dashboard/ui"
	"github.com/matt-riley/waffle/internal/llm"
)

func TestTodayClientStaticContract(t *testing.T) {
	handler := newTestShellHandler(t, ui.ShellView{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/assets/today.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Today client status = %d, want %d", rec.Code, http.StatusOK)
	}
	source := rec.Body.String()

	for _, required := range []string{
		`const phase = Object.freeze({`,
		`opening: "opening"`,
		`idle: "idle"`,
		`sending: "sending"`,
		`streaming: "streaming"`,
		`cancelling: "cancelling"`,
		`disconnected: "disconnected"`,
		`document.createTextNode`,
		`.textContent`,
		`"/api/v1/desk/bootstrap"`,
		`"/api/v1/desk/chat/open"`,
		`"/api/v1/desk/chat/turn"`,
		`"/api/v1/desk/chat/cancel"`,
		`"/api/v1/desk/chat/command"`,
		`"/api/v1/desk/chat/close"`,
		`resource_id !== state.clientID`,
		`command: { name: "model", args: alias }`,
		`crypto.randomUUID()`,
		`eventSource.close()`,
		`activeOperation: null`,
		`activeTurn: null`,
		`state.activeTurn !== turn`,
		`bootstrap.request_token`,
		`bootstrap.event_cursor`,
		`/api/v1/desk/events?after=`,
		`const staleClientID = state.clientID`,
		`generation: 0`,
		`generation !== state.generation`,
		`function scheduleReconnect()`,
		`resync_required`,
		`error.network`,
		`idempotencyKey`,
		`state.eventCursor`,
	} {
		if !strings.Contains(source, required) {
			t.Errorf("Today client missing static contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"innerHTML",
		"localStorage",
		"sessionStorage",
		"retryTurn",
		"retryMutation",
		"console.",
		"turnDoneSeen",
		"turnRequestPending",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("Today client contains forbidden behavior %q", forbidden)
		}
	}
	// Bounded reconnect uses setTimeout; unbounded polling must not appear.
	if !strings.Contains(source, "setTimeout(") {
		t.Error("Today client missing bounded reconnect timer")
	}
	if strings.Contains(source, "setInterval(") {
		t.Error("Today client must not use setInterval")
	}
	if got := strings.Count(source, `postMutation("/api/v1/desk/chat/turn"`); got != 1 {
		t.Errorf("turn mutation call sites = %d, want exactly 1", got)
	}
	if got := strings.Count(source, `postMutation("/api/v1/desk/chat/cancel"`); got != 1 {
		t.Errorf("cancel mutation call sites = %d, want exactly 1", got)
	}
	cancelStart := strings.Index(source, "async function cancelTurn")
	modelStart := strings.Index(source, "async function selectModel")
	if cancelStart == -1 || modelStart <= cancelStart {
		t.Fatal("Today client must define bounded cancel and model handlers")
	}
	if strings.Contains(source[cancelStart:modelStart], "setPhase(phase.idle)") {
		t.Fatal("cancel must wait for turn_done instead of restoring idle itself")
	}
}

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
	// Exact-value redaction is the chat secret boundary; format regex is not (#153).
	clients.SetRedactor(func(s string) string { return strings.ReplaceAll(s, canary, "[redacted]") })
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
	if !strings.Contains(data, `"tool_name":"[redacted]"`) {
		t.Fatalf("tool name was not structurally rejected: %s", data)
	}
}

func TestChatClientPublishesSafeErrorAndStateEvents(t *testing.T) {
	canary := "provider failed sk-event-secret /var/lib/waffle/private prompt: ship it"
	backend := &fakeChatBackend{turnEvents: []chat.Event{
		{Kind: chat.EventNotice, IsError: true, Text: canary},
		{Kind: chat.EventState, State: &chat.State{Title: "ready", Workspace: "/var/lib/waffle/work", History: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Text: canary}}}}}},
	}}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	clients.SetRedactor(func(s string) string { return strings.ReplaceAll(s, canary, "[redacted]") })
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
	var projected struct {
		State map[string]json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(second.Data, &projected); err != nil {
		t.Fatal(err)
	}
	if _, exists := projected.State["history"]; exists {
		t.Fatalf("state event contains history field: %s", second.Data)
	}
	if strings.Contains(string(second.Data), canary) || strings.Contains(string(second.Data), "/var/lib/waffle") {
		t.Fatalf("state event = %s", second.Data)
	}
	if workspace, exists := projected.State["workspace"]; !exists || string(workspace) != `""` {
		t.Fatalf("absolute workspace path was not dropped: %s", second.Data)
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

func TestChatClientCloseBoundsActiveWorkForBackgroundCaller(t *testing.T) {
	backend := &fakeChatBackend{
		turnStarted: make(chan struct{}),
		releaseTurn: make(chan struct{}),
		closeCalled: make(chan struct{}),
	}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = 30 * time.Millisecond
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turnDone := make(chan error, 1)
	go func() { turnDone <- clients.Turn(context.Background(), id, "hold") }()
	<-backend.turnStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- clients.Close(context.Background(), id) }()
	select {
	case err := <-closeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error = %v, want deadline exceeded", err)
		}
	case <-time.After(150 * time.Millisecond):
		close(backend.releaseTurn)
		<-turnDone
		<-closeDone
		t.Fatal("Close with background caller waited forever for active work")
	}
	select {
	case <-backend.closeCalled:
		t.Fatal("Close called backend while active work was still running")
	default:
	}
	close(backend.releaseTurn)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
}

func TestChatClientsShutdownWaitsForCallerCancelledCloseFinalization(t *testing.T) {
	backend := &fakeChatBackend{
		turnStarted: make(chan struct{}),
		releaseTurn: make(chan struct{}),
		closeCalled: make(chan struct{}),
	}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = time.Second
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turnDone := make(chan error, 1)
	go func() { turnDone <- clients.Turn(context.Background(), id, "hold") }()
	<-backend.turnStarted

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clients.Close(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context canceled", err)
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- clients.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before pending Close finalized: %v", err)
	case <-backend.closeCalled:
		t.Fatal("pending Close called backend before active work drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.releaseTurn)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-backend.closeCalled:
	default:
		t.Fatal("pending Close did not finalize backend before Shutdown returned")
	}
	if backend.closeCount() != 1 {
		t.Fatalf("close calls = %d, want 1", backend.closeCount())
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

func TestChatClientCancellationResetsForSecondTurn(t *testing.T) {
	firstStarted, firstRelease := make(chan struct{}), make(chan struct{})
	secondStarted, secondRelease := make(chan struct{}), make(chan struct{})
	backend := &fakeChatBackend{
		turnStarts:   []chan struct{}{firstStarted, secondStarted},
		releaseTurns: []chan struct{}{firstRelease, secondRelease},
	}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- clients.Turn(context.Background(), id, "first") }()
	<-firstStarted
	if err := clients.Cancel(id); err != nil {
		t.Fatal(err)
	}
	close(firstRelease)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- clients.Turn(context.Background(), id, "second") }()
	<-secondStarted
	if err := clients.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if backend.cancelCount() != 2 {
		t.Fatalf("cancel calls after second turn = %d, want 2", backend.cancelCount())
	}
	close(secondRelease)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
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

func TestChatClientCancelRacingCloseWaitsAndRunsExactlyOnce(t *testing.T) {
	backend := &fakeChatBackend{cancelStarted: make(chan struct{}), releaseCancel: make(chan struct{}), closeCalled: make(chan struct{})}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{12}, 32)))
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = clients.Cancel(id) }()
	<-backend.cancelStarted
	done := make(chan error, 1)
	go func() { done <- clients.Close(context.Background(), id) }()
	select {
	case <-backend.closeCalled:
		t.Fatal("close ran before cancel returned")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.releaseCancel)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if backend.cancelCount() != 1 || backend.closeCount() != 1 {
		t.Fatalf("counts cancel=%d close=%d", backend.cancelCount(), backend.closeCount())
	}
}

func TestChatClientCancelRacingShutdownWaitsAndRunsExactlyOnce(t *testing.T) {
	backend := &fakeChatBackend{cancelStarted: make(chan struct{}), releaseCancel: make(chan struct{}), closeCalled: make(chan struct{})}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{13}, 32)))
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = clients.Cancel(id) }()
	<-backend.cancelStarted
	done := make(chan error, 1)
	go func() { done <- clients.Shutdown(context.Background()) }()
	select {
	case <-backend.closeCalled:
		t.Fatal("shutdown close ran before cancel returned")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.releaseCancel)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if backend.cancelCount() != 1 || backend.closeCount() != 1 {
		t.Fatalf("counts cancel=%d close=%d", backend.cancelCount(), backend.closeCount())
	}
}

func TestChatClientsShutdownUsesOneGlobalDeadline(t *testing.T) {
	backends := []chat.Backend{&fakeChatBackend{closeWaitContext: true}, &fakeChatBackend{closeWaitContext: true}}
	i := 0
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { b := backends[i]; i++; return b, nil }, nil)
	clients.shutdownTTL = 30 * time.Millisecond
	for range 2 {
		if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	_ = clients.Shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("shutdown used per-backend deadlines: %s", elapsed)
	}
}

func TestChatClientsShutdownWithCancelledCallerDrainsActiveWork(t *testing.T) {
	backend := &fakeChatBackend{
		turnStarted: make(chan struct{}),
		releaseTurn: make(chan struct{}),
		closeCalled: make(chan struct{}),
	}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = time.Second
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turnDone := make(chan error, 1)
	go func() { turnDone <- clients.Turn(context.Background(), id, "hold") }()
	<-backend.turnStarted

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- clients.Shutdown(ctx) }()
	select {
	case <-backend.closeCalled:
		t.Fatal("Shutdown closed backend before active work drained")
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before active work drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.releaseTurn)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown error = %v", err)
	}
	select {
	case <-backend.closeCalled:
	default:
		t.Fatal("Shutdown did not close backend after active work drained")
	}
}

func TestChatClientsShutdownNeverStartsUntrackedBackendCancel(t *testing.T) {
	backend := &cleanupContractBackend{
		cancelStarted:  make(chan struct{}),
		cancelReturned: make(chan struct{}),
		releaseCancel:  make(chan struct{}),
		closeStarted:   make(chan struct{}),
		closeReturned:  make(chan struct{}),
	}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = 30 * time.Millisecond
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- clients.Shutdown(context.Background()) }()
	select {
	case <-backend.cancelStarted:
		var shutdownErr error
		select {
		case shutdownErr = <-shutdownDone:
		case <-time.After(150 * time.Millisecond):
			close(backend.releaseCancel)
			<-backend.cancelReturned
			t.Fatal("Shutdown remained joined to an uninterruptible Backend.Cancel")
		}
		select {
		case <-backend.cancelReturned:
			t.Fatal("blocking Backend.Cancel unexpectedly returned")
		default:
		}
		close(backend.releaseCancel)
		<-backend.cancelReturned
		t.Fatalf("Shutdown launched Backend.Cancel and returned with it outstanding: %v", shutdownErr)
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown error = %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Shutdown neither completed nor exposed a backend cancellation")
	}
	if backend.cancelCount() != 0 {
		t.Fatalf("cancel calls = %d, want 0; Close owns cleanup cancellation", backend.cancelCount())
	}
	if backend.closeCount() != 1 {
		t.Fatalf("close calls = %d, want 1", backend.closeCount())
	}
	select {
	case <-backend.closeReturned:
	default:
		t.Fatal("backend Close remained outstanding after Shutdown")
	}
}

func TestChatClientsShutdownCancelsAndDrainsActiveOperationBeforeClose(t *testing.T) {
	for _, operation := range []string{"turn", "command"} {
		t.Run(operation, func(t *testing.T) {
			backend := &cleanupContractBackend{
				operation:        operation,
				operationStarted: make(chan struct{}),
				operationExited:  make(chan struct{}),
				rescueOperation:  make(chan struct{}),
				closeStarted:     make(chan struct{}),
				closeReturned:    make(chan struct{}),
			}
			clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
			clients.shutdownTTL = 80 * time.Millisecond
			id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			operationDone := make(chan error, 1)
			if operation == "turn" {
				go func() { operationDone <- clients.Turn(context.Background(), id, "hold") }()
			} else {
				go func() {
					_, commandErr := clients.Command(context.Background(), id, chat.ParsedCommand{Name: chat.CommandStatus})
					operationDone <- commandErr
				}()
			}
			<-backend.operationStarted

			shutdownErr := clients.Shutdown(context.Background())
			backend.releaseOperation()
			operationErr := <-operationDone
			if shutdownErr != nil {
				t.Fatalf("Shutdown error = %v", shutdownErr)
			}
			if !errors.Is(operationErr, context.Canceled) {
				t.Fatalf("%s error = %v, want context canceled", operation, operationErr)
			}
			select {
			case <-backend.operationExited:
			default:
				t.Fatalf("%s remained outstanding after Shutdown", operation)
			}
			select {
			case <-backend.closeReturned:
			default:
				t.Fatal("backend Close remained outstanding after Shutdown")
			}
			if backend.cancelCount() != 0 || backend.closeCount() != 1 {
				t.Fatalf("cleanup calls cancel=%d close=%d, want 0 and 1", backend.cancelCount(), backend.closeCount())
			}
		})
	}
}

func TestChatClientsShutdownPreservesEarlierCallerDeadlineAcrossClients(t *testing.T) {
	first := &cleanupContractBackend{
		closeStarted:        make(chan struct{}),
		closeReturned:       make(chan struct{}),
		closeWaitForContext: true,
	}
	second := &cleanupContractBackend{
		closeStarted:        make(chan struct{}),
		closeReturned:       make(chan struct{}),
		closeWaitForContext: true,
	}
	backends := []chat.Backend{first, second}
	index := 0
	clients := NewChatClients(func(context.Context) (chat.Backend, error) {
		backend := backends[index]
		index++
		return backend, nil
	}, nil)
	clients.shutdownTTL = 300 * time.Millisecond
	for range backends {
		if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	started := time.Now()
	err := clients.Shutdown(ctx)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Shutdown discarded earlier caller deadline: %v", elapsed)
	}
	for i, backend := range []*cleanupContractBackend{first, second} {
		select {
		case <-backend.closeReturned:
		default:
			t.Fatalf("backend %d Close remained outstanding after Shutdown", i+1)
		}
		if calls := backend.closeCount(); calls != 1 {
			t.Fatalf("backend %d close calls = %d, want 1", i+1, calls)
		}
		if delta := backend.observedCloseDeadline().Sub(callerDeadline); delta < -10*time.Millisecond || delta > 10*time.Millisecond {
			t.Fatalf("backend %d close deadline differs from caller by %v", i+1, delta)
		}
	}
	clients.mu.Lock()
	pending := len(clients.pending)
	clients.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending cleanups after Shutdown = %d, want 0", pending)
	}
}

func TestChatClientsShutdownShortensPendingCloseToGlobalDeadlineAndJoins(t *testing.T) {
	backend := &cleanupContractBackend{
		closeStarted:        make(chan struct{}),
		closeReturned:       make(chan struct{}),
		closeWaitForContext: true,
	}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = 300 * time.Millisecond
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	closeCtx, closeCancel := context.WithCancel(context.Background())
	closeCancel()
	if err := clients.Close(closeCtx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context canceled", err)
	}
	<-backend.closeStarted

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer shutdownCancel()
	started := time.Now()
	err = clients.Shutdown(shutdownCtx)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Shutdown waited for pending Close's later deadline: %v", elapsed)
	}
	select {
	case <-backend.closeReturned:
	default:
		t.Fatal("pending backend Close remained outstanding after Shutdown")
	}
	if calls := backend.closeCount(); calls != 1 {
		t.Fatalf("close calls = %d, want 1", calls)
	}
	clients.mu.Lock()
	pending := len(clients.pending)
	clients.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending cleanups after Shutdown = %d, want 0", pending)
	}
}

func TestChatClientCloseTimeoutRemainsRetryableUntilExplicitCloseSucceeds(t *testing.T) {
	backend := newRetryableCleanupBackend()
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = time.Second
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer closeCancel()
	if err := clients.Close(closeCtx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want deadline exceeded", err)
	}
	clients.mu.Lock()
	retained := clients.clients[id]
	cleanup := clients.pending[retained]
	clients.mu.Unlock()
	if cleanup != nil {
		<-cleanup.done
	}
	<-backend.attemptDone(0)

	clients.mu.Lock()
	retained = clients.clients[id]
	pending := len(clients.pending)
	clients.mu.Unlock()
	if retained == nil || retained.backend != backend {
		t.Fatal("timed-out Close lost or replaced the retryable client handle")
	}
	if pending != 0 {
		t.Fatalf("pending cleanups after timed-out Close = %d, want 0", pending)
	}
	if !backend.ownerHeld() || !backend.resourcesHeld() {
		t.Fatal("timed-out Close released runtime ownership or resources")
	}
	if err := clients.Turn(context.Background(), id, "must not run"); !errors.Is(err, errChatClientNotFound) {
		t.Fatalf("Turn during retryable cleanup error = %v, want client not found", err)
	}
	if _, err := clients.Command(context.Background(), id, chat.ParsedCommand{Name: chat.CommandStatus}); !errors.Is(err, errChatClientNotFound) {
		t.Fatalf("Command during retryable cleanup error = %v, want client not found", err)
	}
	if err := clients.Cancel(id); !errors.Is(err, errChatClientNotFound) {
		t.Fatalf("Cancel during retryable cleanup error = %v, want client not found", err)
	}

	backend.allowSuccess()
	if err := clients.Close(context.Background(), id); err != nil {
		t.Fatalf("explicit Close retry: %v", err)
	}
	if calls := backend.closeCount(); calls != 2 {
		t.Fatalf("backend Close calls = %d, want 2", calls)
	}
	if backend.ownerHeld() || backend.resourcesHeld() {
		t.Fatal("successful Close retry retained runtime ownership or resources")
	}
	clients.mu.Lock()
	retained = clients.clients[id]
	pending = len(clients.pending)
	clients.mu.Unlock()
	if retained != nil || pending != 0 {
		t.Fatalf("successful Close retry retained client=%t pending=%d", retained != nil, pending)
	}
}

func TestChatClientCloseTimeoutRemainsRetryableUntilShutdownSucceeds(t *testing.T) {
	backend := newRetryableCleanupBackend()
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	clients.shutdownTTL = time.Second
	id, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer closeCancel()
	if err := clients.Close(closeCtx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want deadline exceeded", err)
	}
	clients.mu.Lock()
	retained := clients.clients[id]
	cleanup := clients.pending[retained]
	clients.mu.Unlock()
	if cleanup != nil {
		<-cleanup.done
	}
	<-backend.attemptDone(0)
	backend.allowSuccess()

	if err := clients.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown retry: %v", err)
	}
	if calls := backend.closeCount(); calls != 2 {
		t.Fatalf("backend Close calls = %d, want 2", calls)
	}
	if backend.ownerHeld() || backend.resourcesHeld() {
		t.Fatal("successful Shutdown retry retained runtime ownership or resources")
	}
	clients.mu.Lock()
	retainedCount := len(clients.clients)
	pending := len(clients.pending)
	clients.mu.Unlock()
	if retainedCount != 0 || pending != 0 {
		t.Fatalf("successful Shutdown retry retained clients=%d pending=%d", retainedCount, pending)
	}
}

type retryableCleanupBackend struct {
	mu          sync.Mutex
	allow       chan struct{}
	attempts    []chan struct{}
	closeCalls  int
	ownsSession bool
	hasResource bool
}

func newRetryableCleanupBackend() *retryableCleanupBackend {
	return &retryableCleanupBackend{
		allow:       make(chan struct{}),
		attempts:    []chan struct{}{make(chan struct{}), make(chan struct{})},
		ownsSession: true,
		hasResource: true,
	}
}

func (b *retryableCleanupBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	return chat.State{SessionID: "retryable-session"}, nil
}

func (b *retryableCleanupBackend) Turn(context.Context, string, func(chat.Event)) error {
	return nil
}

func (b *retryableCleanupBackend) Command(context.Context, chat.ParsedCommand, func(chat.Event)) (chat.Result, error) {
	return chat.Result{}, nil
}

func (b *retryableCleanupBackend) Cancel() {}

func (b *retryableCleanupBackend) Close(ctx context.Context) error {
	b.mu.Lock()
	call := b.closeCalls
	b.closeCalls++
	done := b.attempts[call]
	b.mu.Unlock()
	defer close(done)
	select {
	case <-b.allow:
		b.mu.Lock()
		b.ownsSession = false
		b.hasResource = false
		b.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *retryableCleanupBackend) allowSuccess() {
	close(b.allow)
}

func (b *retryableCleanupBackend) attemptDone(call int) <-chan struct{} {
	return b.attempts[call]
}

func (b *retryableCleanupBackend) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeCalls
}

func (b *retryableCleanupBackend) ownerHeld() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ownsSession
}

func (b *retryableCleanupBackend) resourcesHeld() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hasResource
}

type cleanupContractBackend struct {
	mu                  sync.Mutex
	operation           string
	operationStarted    chan struct{}
	operationExited     chan struct{}
	rescueOperation     chan struct{}
	rescueOnce          sync.Once
	cancelStarted       chan struct{}
	cancelReturned      chan struct{}
	releaseCancel       chan struct{}
	closeStarted        chan struct{}
	closeReturned       chan struct{}
	closeWaitForContext bool
	cancelCalls         int
	closeCalls          int
	closeDeadline       time.Time
}

func (b *cleanupContractBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	return chat.State{SessionID: "session"}, nil
}

func (b *cleanupContractBackend) Turn(ctx context.Context, _ string, _ func(chat.Event)) error {
	if b.operation != "turn" {
		return nil
	}
	return b.runOperation(ctx)
}

func (b *cleanupContractBackend) Command(ctx context.Context, _ chat.ParsedCommand, _ func(chat.Event)) (chat.Result, error) {
	if b.operation != "command" {
		return chat.Result{}, nil
	}
	return chat.Result{}, b.runOperation(ctx)
}

func (b *cleanupContractBackend) runOperation(ctx context.Context) error {
	close(b.operationStarted)
	select {
	case <-ctx.Done():
		close(b.operationExited)
		return ctx.Err()
	case <-b.rescueOperation:
		close(b.operationExited)
		return context.Canceled
	}
}

func (b *cleanupContractBackend) releaseOperation() {
	b.rescueOnce.Do(func() {
		if b.rescueOperation != nil {
			close(b.rescueOperation)
		}
	})
}

func (b *cleanupContractBackend) Cancel() {
	b.mu.Lock()
	b.cancelCalls++
	b.mu.Unlock()
	if b.cancelStarted != nil {
		close(b.cancelStarted)
		<-b.releaseCancel
		close(b.cancelReturned)
	}
}

func (b *cleanupContractBackend) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closeCalls++
	b.closeDeadline, _ = ctx.Deadline()
	b.mu.Unlock()
	if b.closeStarted != nil {
		close(b.closeStarted)
	}
	var err error
	if b.closeWaitForContext {
		<-ctx.Done()
		err = ctx.Err()
	}
	if b.closeReturned != nil {
		close(b.closeReturned)
	}
	return err
}

func (b *cleanupContractBackend) cancelCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelCalls
}

func (b *cleanupContractBackend) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeCalls
}

func (b *cleanupContractBackend) observedCloseDeadline() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeDeadline
}

type fakeChatBackend struct {
	mu               sync.Mutex
	openCalls        int
	openState        chat.State
	openErr          error
	turnCalls        int
	commandCalls     int
	closeCalls       int
	turnStarted      chan struct{}
	releaseTurn      chan struct{}
	closeCalled      chan struct{}
	turnStarts       []chan struct{}
	releaseTurns     []chan struct{}
	turnEvent        chat.Event
	turnEvents       []chat.Event
	closeErr         error
	cancels          int
	cancelStarted    chan struct{}
	releaseCancel    chan struct{}
	closeWaitContext bool
}

func (f *fakeChatBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	f.mu.Lock()
	f.openCalls++
	f.mu.Unlock()
	if f.openState.SessionID != "" || len(f.openState.History) > 0 {
		return f.openState, f.openErr
	}
	return chat.State{SessionID: "session"}, f.openErr
}

func (f *fakeChatBackend) Turn(_ context.Context, _ string, emit func(chat.Event)) error {
	f.mu.Lock()
	call := f.turnCalls
	f.turnCalls++
	f.mu.Unlock()
	if call < len(f.turnStarts) {
		close(f.turnStarts[call])
		<-f.releaseTurns[call]
	}
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
	return nil
}

func (f *fakeChatBackend) Command(context.Context, chat.ParsedCommand, func(chat.Event)) (chat.Result, error) {
	f.mu.Lock()
	f.commandCalls++
	f.mu.Unlock()
	return chat.Result{}, nil
}

func (f *fakeChatBackend) Cancel() {
	f.mu.Lock()
	f.cancels++
	f.mu.Unlock()
	if f.cancelStarted != nil {
		close(f.cancelStarted)
		<-f.releaseCancel
	}
}

func (f *fakeChatBackend) Close(ctx context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	if f.closeCalled != nil {
		close(f.closeCalled)
	}
	if f.closeWaitContext {
		<-ctx.Done()
		return ctx.Err()
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

func (f *fakeChatBackend) cancelCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.cancels }
func (f *fakeChatBackend) commandCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.commandCalls }
