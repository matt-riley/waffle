package chatwire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

var canaries = []string{
	"AGE-SECRET-KEY-1TEST",
	"sk-provider-secret",
	"/var/lib/waffle/config.toml",
	"WAFFLE_AGE_IDENTITY",
}

func TestServerRequiresOpenBeforeCreatingBackend(t *testing.T) {
	t.Parallel()

	var factoryCalls atomic.Int32
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		factoryCalls.Add(1)
		return newWireFake("unused"), nil
	}, nil)
	defer stop()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	codec := NewClientCodec(conn, conn)
	if err := encodeTestFrame(codec, TypeTurn, "early", TurnPayload{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	frame, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	assertWireError(t, frame, "open_required", "open must be the first chat request")
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("factory calls = %d", got)
	}
}

func TestServerRedactsStateEventsResultsAndErrors(t *testing.T) {
	t.Parallel()

	secretText := strings.Join(canaries, " ")
	backend := newWireFake("redacted")
	backend.state.Title = secretText
	backend.state.ModelError = secretText
	backend.turnEvent = chat.Event{Kind: chat.EventNotice, Text: secretText}
	backend.turnErr = errors.New(secretText)
	backend.commandResult = chat.Result{Title: "status", Text: secretText}
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return backend, nil
	}, nil)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.Open(ctx, chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, fmt.Sprintf("%+v", state))

	var events []chat.Event
	err = client.Turn(ctx, "secret test", func(event chat.Event) { events = append(events, event) })
	if err == nil || len(events) != 1 {
		t.Fatalf("Turn events=%+v err=%v", events, err)
	}
	assertNoCanaries(t, err.Error())
	assertNoCanaries(t, fmt.Sprintf("%+v", events))
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "turn_failed" || remote.Message != "chat turn failed" {
		t.Fatalf("Turn error = %#v", err)
	}

	result, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, fmt.Sprintf("%+v", result))
	if result.Text == secretText {
		t.Fatal("command result was not redacted")
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerEncodedRedactionPreservesIntegerFields(t *testing.T) {
	t.Parallel()

	const byteCount = int(1<<54 + 1)
	var wire bytes.Buffer
	writer := &serverWriter{codec: NewServerCodec(nil, &wire)}
	if err := writer.send(TypeToolFinished, "request-1", chat.Event{
		Kind:      chat.EventToolFinished,
		Text:      strings.Join(canaries, " "),
		ToolName:  "read",
		ByteCount: byteCount,
	}); err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, wire.String())
	frame, err := NewClientCodec(&wire, nil).Decode()
	if err != nil {
		t.Fatal(err)
	}
	var event chat.Event
	if err := decodePayload(frame, &event); err != nil {
		t.Fatal(err)
	}
	if event.ByteCount != byteCount {
		t.Fatalf("byte count = %d, want %d", event.ByteCount, byteCount)
	}
}

func TestServerRejectsSecondTurnAndCancelRemainsResponsive(t *testing.T) {
	t.Parallel()

	backend := newWireFake("one")
	backend.blockTurn = true
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return backend, nil
	}, nil)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- client.Turn(ctx, "first", nil) }()
	backend.waitStarted(t)

	err = client.Turn(ctx, "second", nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "turn_active" || remote.Message != "a chat turn is already active" {
		t.Fatalf("second Turn error = %#v", err)
	}
	client.Cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first Turn: %v", err)
	}
	if got := backend.cancelCount(); got != 1 {
		t.Fatalf("cancel calls = %d", got)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerDisconnectCancelsAndClosesOnlyThatBackend(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var backends []*wireFakeBackend
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		mu.Lock()
		defer mu.Unlock()
		backend := newWireFake(fmt.Sprintf("client-%d", len(backends)+1))
		backend.blockTurn = true
		backends = append(backends, backend)
		return backend, nil
	}, nil)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	firstState, err := first.Open(ctx, chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := second.Open(ctx, chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if firstState.SessionID == secondState.SessionID {
		t.Fatalf("client state was shared: %+v %+v", firstState, secondState)
	}

	mu.Lock()
	firstBackend, secondBackend := backends[0], backends[1]
	mu.Unlock()
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Turn(ctx, "disconnect", nil) }()
	firstBackend.waitStarted(t)
	if err := first.conn.Close(); err != nil {
		t.Fatal(err)
	}
	firstBackend.waitClosed(t)
	if firstBackend.cancelCount() != 1 || firstBackend.closeCount() != 1 {
		t.Fatalf("first lifecycle cancel=%d close=%d", firstBackend.cancelCount(), firstBackend.closeCount())
	}

	result, err := second.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus}, nil)
	if err != nil || result.Text != "client-2" {
		t.Fatalf("second Command = %+v, %v", result, err)
	}
	if secondBackend.cancelCount() != 0 || secondBackend.closeCount() != 0 {
		t.Fatalf("second affected cancel=%d close=%d", secondBackend.cancelCount(), secondBackend.closeCount())
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	secondBackend.waitClosed(t)
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("disconnected turn did not exit")
	}
}

func TestServerCloseCancelsActiveTurnBeforeGoodbye(t *testing.T) {
	t.Parallel()

	backend := newWireFake("close")
	backend.blockTurn = true
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return backend, nil
	}, nil)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	turnDone := make(chan error, 1)
	go func() { turnDone <- client.Turn(ctx, "wait", nil) }()
	backend.waitStarted(t)
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	backend.waitClosed(t)
	if backend.cancelCount() != 1 || backend.closeCount() != 1 {
		t.Fatalf("lifecycle cancel=%d close=%d", backend.cancelCount(), backend.closeCount())
	}
	select {
	case err := <-turnDone:
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("active turn goroutine was not released")
	}
}

func TestServerAuditsConnectionLifecycle(t *testing.T) {
	t.Parallel()

	events := make(chan string, 2)
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return newWireFake("audit"), nil
	}, func(_ context.Context, _ net.Conn, event string) { events <- event })
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := <-events, "connected"; got != want {
		t.Fatalf("first audit event = %q, want %q", got, want)
	}
	if got, want := <-events, "disconnected"; got != want {
		t.Fatalf("second audit event = %q, want %q", got, want)
	}
}

func TestServeReturnsUnexpectedListenerError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("listener failed")
	err := Serve(context.Background(), errorListener{err: sentinel}, func(context.Context) (chat.Backend, error) {
		return newWireFake("unused"), nil
	}, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Serve error = %v, want %v", err, sentinel)
	}
}

type wireFakeBackend struct {
	mu            sync.Mutex
	state         chat.State
	turnStarted   chan struct{}
	turnReleased  chan struct{}
	closed        chan struct{}
	startedOnce   sync.Once
	releasedOnce  sync.Once
	closedOnce    sync.Once
	blockTurn     bool
	turnEvent     chat.Event
	turnErr       error
	commandResult chat.Result
	cancels       int
	closes        int
}

type errorListener struct{ err error }

func (l errorListener) Accept() (net.Conn, error) { return nil, l.err }
func (errorListener) Close() error                { return nil }
func (errorListener) Addr() net.Addr              { return &net.UnixAddr{Name: "test", Net: "unix"} }

func newWireFake(id string) *wireFakeBackend {
	return &wireFakeBackend{
		state:         chat.State{SessionID: id, Title: id, ConnectionMode: "unix"},
		turnStarted:   make(chan struct{}),
		turnReleased:  make(chan struct{}),
		closed:        make(chan struct{}),
		commandResult: chat.Result{Title: "status", Text: id},
	}
}

func (b *wireFakeBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	return b.state, nil
}

func (b *wireFakeBackend) Turn(ctx context.Context, _ string, emit func(chat.Event)) error {
	b.startedOnce.Do(func() { close(b.turnStarted) })
	if b.turnEvent.Kind != "" && emit != nil {
		emit(b.turnEvent)
	}
	if b.blockTurn {
		select {
		case <-b.turnReleased:
		case <-ctx.Done():
		}
	}
	if b.turnErr != nil {
		return b.turnErr
	}
	if emit != nil {
		emit(chat.Event{Kind: chat.EventTurnDone})
	}
	return nil
}

func (b *wireFakeBackend) Command(_ context.Context, _ chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	return b.commandResult, nil
}

func (b *wireFakeBackend) Cancel() {
	b.mu.Lock()
	b.cancels++
	b.mu.Unlock()
	b.releasedOnce.Do(func() { close(b.turnReleased) })
}

func (b *wireFakeBackend) Close(context.Context) error {
	b.mu.Lock()
	b.closes++
	b.mu.Unlock()
	b.closedOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *wireFakeBackend) cancelCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancels
}

func (b *wireFakeBackend) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes
}

func (b *wireFakeBackend) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-b.turnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("backend turn did not start")
	}
}

func (b *wireFakeBackend) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-b.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not close")
	}
}

func startChatwireServer(t *testing.T, factory Factory, audit AuditFunc) (string, func()) {
	t.Helper()
	listener, path := unixListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, factory, audit) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = listener.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("Serve did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return path, stop
}

func assertWireError(t *testing.T, frame Frame, code, message string) {
	t.Helper()
	if frame.Type != TypeError {
		t.Fatalf("frame type = %q, want error", frame.Type)
	}
	var payload ErrorPayload
	if err := decodePayload(frame, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != code || payload.Message != message {
		t.Fatalf("error payload = %+v", payload)
	}
}

func assertNoCanaries(t *testing.T, value string) {
	t.Helper()
	for _, canary := range canaries {
		if strings.Contains(value, canary) {
			t.Fatalf("value contains canary %q: %s", canary, value)
		}
	}
}
