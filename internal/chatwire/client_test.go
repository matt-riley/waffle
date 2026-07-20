package chatwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

func TestDialRequiresAbsoluteCleanUnixPath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"relative.sock", "/tmp/../tmp/waffle.sock"} {
		if _, err := Dial(context.Background(), path); err == nil {
			t.Fatalf("Dial(%q) succeeded", path)
		}
	}
}

func TestClientImplementsBackendLifecycle(t *testing.T) {
	t.Parallel()

	listener, path := unixListener(t)
	serverDone := make(chan error, 1)
	cancelRead := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = conn.Close() }()
		codec := NewServerCodec(conn, conn)

		open, err := codec.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		var options chat.OpenOptions
		if err := decodePayload(open, &options); err != nil {
			serverDone <- err
			return
		}
		if !options.Continue || options.Profile != "host" {
			serverDone <- fmt.Errorf("open options = %+v", options)
			return
		}
		if err := encodeTestFrame(codec, TypeReady, open.ID, chat.State{SessionID: "01TEST", ModelAlias: "gpt"}); err != nil {
			serverDone <- err
			return
		}

		turn, err := codec.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		var input TurnPayload
		if err := decodePayload(turn, &input); err != nil || input.Text != "hello" {
			serverDone <- fmt.Errorf("turn payload = %+v, %v", input, err)
			return
		}
		if err := encodeTestFrame(codec, TypeTextDelta, turn.ID, chat.Event{Kind: chat.EventTextDelta, Text: "hel"}); err != nil {
			serverDone <- err
			return
		}
		cancel, err := codec.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		if cancel.Type != TypeCancel {
			serverDone <- fmt.Errorf("got %s, want cancel", cancel.Type)
			return
		}
		close(cancelRead)
		if err := encodeTestFrame(codec, TypeTurnDone, turn.ID, chat.Event{Kind: chat.EventTurnDone}); err != nil {
			serverDone <- err
			return
		}

		command, err := codec.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		var parsed chat.ParsedCommand
		if err := decodePayload(command, &parsed); err != nil || parsed.Name != chat.CommandStatus {
			serverDone <- fmt.Errorf("command payload = %+v, %v", parsed, err)
			return
		}
		if err := encodeTestFrame(codec, TypeNotice, command.ID, chat.Event{Kind: chat.EventNotice, Text: "checking"}); err != nil {
			serverDone <- err
			return
		}
		if err := encodeTestFrame(codec, TypeCommandResult, command.ID, chat.Result{Title: "status", Text: "ready"}); err != nil {
			serverDone <- err
			return
		}

		closeFrame, err := codec.Decode()
		if err != nil {
			serverDone <- err
			return
		}
		if closeFrame.Type != TypeClose {
			serverDone <- fmt.Errorf("got %s, want close", closeFrame.Type)
			return
		}
		serverDone <- encodeTestFrame(codec, TypeGoodbye, closeFrame.ID, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.Open(ctx, chat.OpenOptions{Continue: true, Profile: "host"})
	if err != nil || state.SessionID != "01TEST" || state.ModelAlias != "gpt" {
		t.Fatalf("Open = %+v, %v", state, err)
	}

	var eventMu sync.Mutex
	var events []chat.Event
	turnDone := make(chan error, 1)
	go func() {
		turnDone <- client.Turn(ctx, "hello", func(event chat.Event) {
			eventMu.Lock()
			events = append(events, event)
			eventMu.Unlock()
		})
	}()
	waitForClientEvent(t, &eventMu, &events, chat.EventTextDelta)
	client.Cancel()
	select {
	case <-cancelRead:
	case <-ctx.Done():
		t.Fatal("server did not receive cancel")
	}
	if err := <-turnDone; err != nil {
		t.Fatalf("Turn: %v", err)
	}
	eventMu.Lock()
	if len(events) != 2 || events[0].Text != "hel" || events[1].Kind != chat.EventTurnDone {
		t.Fatalf("turn events = %+v", events)
	}
	eventMu.Unlock()

	events = nil
	result, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus}, func(event chat.Event) {
		events = append(events, event)
	})
	if err != nil || result.Title != "status" || result.Text != "ready" {
		t.Fatalf("Command = %+v, %v", result, err)
	}
	if len(events) != 1 || events[0].Kind != chat.EventNotice {
		t.Fatalf("command events = %+v", events)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestClientRejectsServerVersionMismatch(t *testing.T) {
	t.Parallel()

	listener, path := unixListener(t)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		codec := NewServerCodec(conn, conn)
		open, err := codec.Decode()
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(conn, "{\"version\":2,\"type\":\"ready\",\"id\":%q}\n", open.ID)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.conn.Close() }()
	_, err = client.Open(ctx, chat.OpenOptions{})
	if !errors.Is(err, ErrProtocolVersion) {
		t.Fatalf("Open error = %v, want %v", err, ErrProtocolVersion)
	}
}

func TestClientReturnsStableRemoteError(t *testing.T) {
	t.Parallel()

	listener, path := unixListener(t)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		codec := NewServerCodec(conn, conn)
		open, err := codec.Decode()
		if err != nil {
			return
		}
		_ = encodeTestFrame(codec, TypeError, open.ID, ErrorPayload{Code: "open_failed", Message: "could not open chat"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.conn.Close() }()
	_, err = client.Open(ctx, chat.OpenOptions{})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "open_failed" || remote.Message != "could not open chat" {
		t.Fatalf("Open error = %#v", err)
	}
}

func TestClientDeliversBufferedResponseBeforeDisconnect(t *testing.T) {
	t.Parallel()

	client := &Client{closed: make(chan struct{}), readErr: io.EOF}
	close(client.closed)
	for range 100 {
		responses := make(chan Frame, 1)
		responses <- Frame{Version: ProtocolVersion, Type: TypeGoodbye, ID: "close"}
		frame, err := client.nextResponse(context.Background(), responses)
		if err != nil || frame.Type != TypeGoodbye {
			t.Fatalf("nextResponse = %+v, %v", frame, err)
		}
	}
}

func unixListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "waffle-chatwire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "chat.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, path
}

func encodeTestFrame(codec *Codec, frameType, id string, payload any) error {
	frame, err := newFrame(frameType, id, payload)
	if err != nil {
		return err
	}
	return codec.Encode(frame)
}

func waitForClientEvent(t *testing.T, mu *sync.Mutex, events *[]chat.Event, kind chat.EventKind) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := len(*events) > 0 && (*events)[len(*events)-1].Kind == kind
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not receive %s", kind)
}
