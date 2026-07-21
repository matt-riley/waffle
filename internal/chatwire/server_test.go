package chatwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
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

func TestServerReportsClientProtocolVersionMismatch(t *testing.T) {
	t.Parallel()
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return newWireFake("unused"), nil
	}, nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintln(conn, `{"version":7,"type":"open","id":"mismatch"}`); err != nil {
		t.Fatal(err)
	}
	frame, err := NewClientCodec(conn, nil).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeError {
		t.Fatalf("frame = %+v", frame)
	}
	var payload ErrorPayload
	if err := decodePayload(frame, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != "protocol_version_mismatch" {
		t.Fatalf("payload = %+v", payload)
	}
	for _, want := range []string{"client version 7", "service version 1", "deploy", "binary", "service"} {
		if !strings.Contains(strings.ToLower(payload.Message), want) {
			t.Fatalf("message = %q, want %q", payload.Message, want)
		}
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
	if err == nil || len(events) != 2 || events[1].Kind != chat.EventTurnDone || !events[1].IsError {
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

func TestServerEncodedFrameRedactsNestedMapKeys(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		canaries[0]: map[string]any{
			canaries[1]: map[string]any{
				canaries[2]: canaries[3],
			},
		},
	}
	var wire bytes.Buffer
	writer := &serverWriter{codec: NewServerCodec(nil, &wire)}
	if err := writer.send(TypeNotice, "safe-id", payload); err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, wire.String())

	frame, err := NewClientCodec(&wire, nil).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if frame.ID != "safe-id" {
		t.Fatalf("frame id = %q", frame.ID)
	}
	assertNoCanaries(t, string(frame.Payload))
}

func TestServerRedactedMapKeysPreserveCollidingEntriesDeterministically(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"AGE-SECRET-KEY-ONE": "first",
		"AGE-SECRET-KEY-TWO": "second",
	}
	var previous string
	for range 20 {
		var wire bytes.Buffer
		writer := &serverWriter{codec: NewServerCodec(nil, &wire)}
		if err := writer.send(TypeNotice, "safe", payload); err != nil {
			t.Fatal(err)
		}
		assertNoCanaries(t, wire.String())
		if previous != "" && previous != wire.String() {
			t.Fatalf("redacted wire output was nondeterministic:\n%s\n%s", previous, wire.String())
		}
		previous = wire.String()
		frame, err := NewClientCodec(&wire, nil).Decode()
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(frame.Payload, &decoded); err != nil {
			t.Fatal(err)
		}
		if len(decoded) != 2 {
			t.Fatalf("decoded map = %#v, want both entries", decoded)
		}
	}
}

func TestServerRawClientFrameRedactsCanaryIDAndPayloadKeys(t *testing.T) {
	t.Parallel()

	backend := newWireFake("raw-redaction")
	var factoryCalls atomic.Int32
	nested, err := json.Marshal(map[string]any{
		canaries[0]: map[string]any{canaries[1]: map[string]any{canaries[2]: canaries[3]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.state.History = []llm.Message{{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{{
			Type:    llm.BlockToolUse,
			ToolUse: &llm.ToolUse{ID: "tool", Name: "read", Input: nested},
		}},
	}}
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		factoryCalls.Add(1)
		return backend, nil
	}, nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	codec := NewClientCodec(conn, conn)
	if err := encodeTestFrame(codec, TypeOpen, strings.Join(canaries, "-"), chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	assertNoCanaries(t, string(line))
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatal(err)
	}
	assertWireError(t, frame, "invalid_request", "invalid chat request id")
	if frame.ID != "" {
		t.Fatalf("invalid ID was echoed as %q", frame.ID)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("factory calls = %d", got)
	}
}

func TestServerAcceptsOpaqueSafeIDsExactlyAndRejectsUnsafeCollisions(t *testing.T) {
	t.Parallel()

	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return newWireFake("ids"), nil }, nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	codec := NewClientCodec(conn, conn)
	if err := encodeTestFrame(codec, TypeOpen, "opaque id:雪/one", chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	frame, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeReady || frame.ID != "opaque id:雪/one" {
		t.Fatalf("ready frame = %+v", frame)
	}
	for _, unsafeID := range []string{"AGE-SECRET-KEY-ONE", "AGE-SECRET-KEY-TWO", strings.Repeat("x", maxRequestIDBytes+1)} {
		if err := encodeTestFrame(codec, TypeCommand, unsafeID, chat.ParsedCommand{Name: chat.CommandStatus}); err != nil {
			t.Fatal(err)
		}
		frame, err = codec.Decode()
		if err != nil {
			t.Fatal(err)
		}
		assertWireError(t, frame, "invalid_request", "invalid chat request id")
		if frame.ID != "" {
			t.Fatalf("unsafe ID was echoed as %q", frame.ID)
		}
	}
	if err := encodeTestFrame(codec, TypeCommand, "opaque command:雪/two", chat.ParsedCommand{Name: chat.CommandStatus}); err != nil {
		t.Fatal(err)
	}
	frame, err = codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeCommandResult || frame.ID != "opaque command:雪/two" {
		t.Fatalf("command frame = %+v", frame)
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

func TestServerFailedTurnPreservesFinalEventAndReturnsStableError(t *testing.T) {
	t.Parallel()

	backend := newWireFake("failed")
	backend.turnEvent = chat.Event{
		Kind:    chat.EventTurnDone,
		Text:    "partial output persisted",
		IsError: true,
		Usage:   llm.Usage{InputTokens: 7, OutputTokens: 11},
		State:   &chat.State{SessionID: "failed", ModelAlias: "gpt"},
	}
	backend.turnErr = errors.New(strings.Join(canaries, " "))
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
	var events []chat.Event
	err = client.Turn(ctx, "fail", func(event chat.Event) { events = append(events, event) })
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "turn_failed" || remote.Message != "chat turn failed" {
		t.Fatalf("Turn error = %#v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	done := events[0]
	if done.Kind != chat.EventTurnDone || !done.IsError || done.Text != "partial output persisted" ||
		done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 11 || done.State == nil || done.State.SessionID != "failed" {
		t.Fatalf("turn_done fidelity = %+v", done)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerOversizedTurnEventReturnsStableError(t *testing.T) {
	t.Parallel()

	backend := newWireFake("oversized-turn")
	backend.turnEvent = chat.Event{Kind: chat.EventTextDelta, Text: strings.Repeat("x", MaxFrameBytes)}
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
	err = client.Turn(ctx, "large", nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "turn_failed" {
		t.Fatalf("Turn error = %#v", err)
	}
}

func TestServerOversizedTurnEventCancelsTurnContextWhenCancelIsStubborn(t *testing.T) {
	t.Parallel()

	backend := newWireFake("oversized-stubborn-turn")
	backend.turnEvent = chat.Event{Kind: chat.EventTextDelta, Text: strings.Repeat("x", MaxFrameBytes)}
	backend.blockTurn = true
	backend.ignoreCancel = true
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
	done := make(chan error, 1)
	go func() { done <- client.Turn(ctx, "large", nil) }()
	select {
	case err := <-done:
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Code != "turn_failed" {
			t.Fatalf("Turn error = %#v", err)
		}
	case <-ctx.Done():
		t.Fatalf("oversized turn event did not cancel the turn context: %v", ctx.Err())
	}
}

func TestServerOversizedCommandEventReturnsStableError(t *testing.T) {
	t.Parallel()

	backend := newWireFake("oversized-command")
	backend.commandEvent = chat.Event{Kind: chat.EventNotice, Text: strings.Repeat("x", MaxFrameBytes)}
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
	_, err = client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus}, nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "command_failed" {
		t.Fatalf("Command error = %#v", err)
	}
}

func TestServerOversizedCommandDoesNotCancelConcurrentTurn(t *testing.T) {
	t.Parallel()

	backend := newWireFake("oversized-command-with-turn")
	backend.blockTurn = true
	backend.commandEvent = chat.Event{Kind: chat.EventNotice, Text: strings.Repeat("x", MaxFrameBytes)}
	backend.blockCommand = true
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
	go func() { turnDone <- client.Turn(ctx, "active", nil) }()
	backend.waitStarted(t)
	commandDone := make(chan error, 1)
	go func() {
		_, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus}, nil)
		commandDone <- err
	}()
	select {
	case err := <-commandDone:
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Code != "command_failed" {
			t.Fatalf("Command error = %#v", err)
		}
	case <-ctx.Done():
		t.Fatalf("oversized command event did not cancel its command context: %v", ctx.Err())
	}
	if got := backend.cancelCount(); got != 0 {
		t.Fatalf("oversized command canceled backend %d times", got)
	}
	select {
	case err := <-turnDone:
		t.Fatalf("active turn ended during command: %v", err)
	default:
	}
	client.Cancel()
	if err := <-turnDone; err != nil {
		t.Fatalf("Turn after explicit cancel: %v", err)
	}
}

func TestServerBlockingCommandRemainsCancelableAndBounded(t *testing.T) {
	t.Parallel()
	backend := newWireFake("blocking-command")
	backend.blockCommand = true
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
	go func() {
		_, commandErr := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandSkill, Args: "slow"}, nil)
		firstDone <- commandErr
	}()
	backend.waitCommandStarted(t)
	_, err = client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus}, nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "command_active" {
		t.Fatalf("second command = %#v", err)
	}
	client.Cancel()
	select {
	case err := <-firstDone:
		if !errors.As(err, &remote) || remote.Code != "command_failed" {
			t.Fatalf("canceled command = %#v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock command")
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	backend.waitClosed(t)
}

func TestServerCloseAndEOFReapBlockingCommand(t *testing.T) {
	for _, mode := range []string{"close", "eof"} {
		t.Run(mode, func(t *testing.T) {
			backend := newWireFake(mode)
			backend.blockCommand = true
			path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
			go func() {
				_, _ = client.Command(ctx, chat.ParsedCommand{Name: chat.CommandRepo, Args: "owner/repo"}, nil)
			}()
			backend.waitCommandStarted(t)
			if mode == "close" {
				if err := client.Close(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err := client.conn.Close(); err != nil {
				t.Fatal(err)
			}
			backend.waitClosed(t)
		})
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

func TestClientGracefulCloseReturnsSafeWarningAndCleansOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		closeErr    error
		wantMessage string
	}{
		{name: "redacted", closeErr: wireSafeCloseError{"reflection failed: [redacted:test]"}, wantMessage: "reflection failed: [redacted:test]"},
		{name: "unredacted", closeErr: errors.New("opaque-close-secret-9912"), wantMessage: "chat closed with a cleanup warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newWireFake(tc.name)
			backend.closeErr = tc.closeErr
			path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
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
			err = client.Close(ctx)
			var remote *RemoteError
			if !errors.As(err, &remote) || remote.Code != "close_warning" || remote.Message != tc.wantMessage {
				t.Fatalf("Close error = %#v", err)
			}
			if strings.Contains(err.Error(), "opaque-close-secret-9912") {
				t.Fatalf("Close leaked backend error: %v", err)
			}
			if secondErr := client.Close(ctx); secondErr == nil || secondErr.Error() != err.Error() {
				t.Fatalf("second Close = %v, want %v", secondErr, err)
			}
			backend.waitClosed(t)
			if backend.closeCount() != 1 {
				t.Fatalf("backend closes = %d", backend.closeCount())
			}
		})
	}
}

func TestServerGracefulCloseWarningPrecedesTerminalGoodbye(t *testing.T) {
	backend := newWireFake("close-order")
	backend.closeErr = wireSafeCloseError{"cleanup warning"}
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	codec := NewClientCodec(conn, conn)
	if err := encodeTestFrame(codec, TypeOpen, "open", chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(); err != nil {
		t.Fatal(err)
	}
	if err := encodeTestFrame(codec, TypeClose, "close", nil); err != nil {
		t.Fatal(err)
	}
	warning, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	assertWireError(t, warning, "close_warning", "cleanup warning")
	goodbye, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if goodbye.Type != TypeGoodbye || goodbye.ID != "close" {
		t.Fatalf("goodbye = %+v", goodbye)
	}
	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if frame, err := codec.Decode(); err == nil {
		t.Fatalf("post-goodbye frame = %+v", frame)
	}
}

func TestServerCloseCancelsConnectionContextWhenBackendCancelIsStubborn(t *testing.T) {
	t.Parallel()

	backend := newWireFake("stubborn")
	backend.blockTurn = true
	backend.ignoreCancel = true
	audits := make(chan string, 2)
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return backend, nil
	}, func(_ context.Context, _ net.Conn, event string) { audits <- event })
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
	go func() { turnDone <- client.Turn(ctx, "wait for context", nil) }()
	backend.waitStarted(t)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer closeCancel()
	if err := client.Close(closeCtx); err != nil {
		t.Fatalf("Close with stubborn backend: %v", err)
	}
	backend.waitClosed(t)
	if backend.cancelCount() != 1 || backend.closeCount() != 1 {
		t.Fatalf("lifecycle cancel=%d close=%d", backend.cancelCount(), backend.closeCount())
	}
	select {
	case <-turnDone:
	case <-ctx.Done():
		t.Fatal("context-aware turn did not exit")
	}
	if got := <-audits; got != "connected" {
		t.Fatalf("first audit = %q", got)
	}
	if got := <-audits; got != "disconnected" {
		t.Fatalf("second audit = %q", got)
	}
}

func TestServerEventWriteFailureTerminatesConnectionAndCleansBackend(t *testing.T) {
	t.Parallel()

	serverSide, clientSide := net.Pipe()
	failing := &toggleWriteConn{Conn: serverSide}
	backend := newWireFake("write-failure")
	backend.turnEvent = chat.Event{Kind: chat.EventTextDelta, Text: "cannot write"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		serveConn(ctx, failing, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	codec := NewClientCodec(clientSide, clientSide)
	if err := encodeTestFrame(codec, TypeOpen, "open", chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(); err != nil {
		t.Fatal(err)
	}
	failing.fail.Store(true)
	if err := encodeTestFrame(codec, TypeTurn, "turn", TurnPayload{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("event write failure did not terminate the connection")
	}
	backend.waitClosed(t)
}

func TestServerCloseInterruptsBlockedEventWriteBeforeWaiting(t *testing.T) {
	t.Parallel()

	serverSide, clientSide := net.Pipe()
	backend := newWireFake("blocked-write")
	backend.turnEvent = chat.Event{Kind: chat.EventTextDelta, Text: strings.Repeat("x", 64*1024)}
	audits := make(chan string, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serveDone := make(chan struct{})
	go func() {
		serveConn(ctx, serverSide, func(context.Context) (chat.Backend, error) { return backend, nil }, func(_ context.Context, _ net.Conn, event string) {
			audits <- event
		})
		close(serveDone)
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	codec := NewClientCodec(clientSide, clientSide)
	if err := encodeTestFrame(codec, TypeOpen, "open", chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(); err != nil {
		t.Fatal(err)
	}
	if err := encodeTestFrame(codec, TypeTurn, "turn", TurnPayload{Text: "block"}); err != nil {
		t.Fatal(err)
	}
	backend.waitStarted(t)
	time.Sleep(20 * time.Millisecond)
	if err := encodeTestFrame(codec, TypeClose, "close", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-backend.closed:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cleanup waited behind a blocked server write")
	}
	if got := backend.cancelCount(); got != 1 {
		t.Fatalf("blocked turn cancel calls = %d, want 1", got)
	}
	terminal, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Type != TypeTurnDone || terminal.ID != "turn" {
		t.Fatalf("terminal frame = %+v", terminal)
	}
	frame, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != TypeGoodbye || frame.ID != "close" {
		t.Fatalf("goodbye frame = %+v", frame)
	}
	select {
	case <-serveDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("serveConn did not exit after blocked write cleanup")
	}
	if got := <-audits; got != "connected" {
		t.Fatalf("first audit = %q", got)
	}
	if got := <-audits; got != "disconnected" {
		t.Fatalf("second audit = %q", got)
	}
}

func TestServerWriterBoundsNormalWriteWithoutPeerRead(t *testing.T) {
	t.Parallel()

	serverSide, peer := net.Pipe()
	defer func() { _ = serverSide.Close() }()
	defer func() { _ = peer.Close() }()
	writer := &serverWriter{
		conn:         serverSide,
		codec:        NewServerCodec(nil, serverSide),
		writeTimeout: 40 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		done <- writer.send(TypeNotice, "notice", chat.Event{Kind: chat.EventNotice, Text: "blocked"})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write unexpectedly succeeded")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("normal server write had no finite deadline")
	}
}

func TestServerWriterNeverResumesAfterPartialFrameWrite(t *testing.T) {
	t.Parallel()

	serverSide, peer := net.Pipe()
	defer func() { _ = serverSide.Close() }()
	defer func() { _ = peer.Close() }()
	partial := &partialWriteConn{Conn: serverSide}
	tracked := &serverTrackedWriter{conn: partial}
	writer := &serverWriter{conn: partial, codec: NewServerCodec(nil, tracked), trackedWriter: tracked}
	err := writer.send(TypeNotice, "notice", chat.Event{Kind: chat.EventNotice, Text: "partial"})
	if !errors.Is(err, errServerWriteCorrupted) {
		t.Fatalf("send error = %v, want partial-write corruption", err)
	}
	if err := writer.resume(); !errors.Is(err, errServerWriteCorrupted) {
		t.Fatalf("resume error = %v, want partial-write corruption", err)
	}
}

func TestServerImmediateSequentialTurnsNeverSeeTurnActive(t *testing.T) {
	t.Parallel()

	backend := newWireFake("sequential")
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		if err := client.Turn(ctx, fmt.Sprintf("turn-%d", i), nil); err != nil {
			t.Fatalf("Turn %d: %v", i, err)
		}
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerClearsTurnActiveBeforeTerminalFrameIsObservable(t *testing.T) {
	t.Parallel()

	serverSide, clientSide := net.Pipe()
	blocking := &blockingReturnConn{
		Conn:     serverSide,
		observed: make(chan struct{}),
		release:  make(chan struct{}),
	}
	backend := newWireFake("terminal-order")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() {
		serveConn(ctx, blocking, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
		close(serverDone)
	}()
	client := &Client{
		conn:      clientSide,
		codec:     NewClientCodec(clientSide, clientSide),
		pending:   make(map[string]chan Frame),
		routeDone: make(map[string]chan struct{}),
		closed:    make(chan struct{}),
	}
	go client.readLoop()
	t.Cleanup(func() {
		cancel()
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	blocking.armed.Store(true)
	firstDone := make(chan error, 1)
	go func() { firstDone <- client.Turn(ctx, "first", nil) }()
	select {
	case <-blocking.observed:
	case <-ctx.Done():
		t.Fatal("first terminal frame was not observed")
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Turn: %v", err)
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- client.Turn(ctx, "second", nil) }()
	time.Sleep(20 * time.Millisecond)
	close(blocking.release)
	if err := <-secondDone; err != nil {
		t.Fatalf("second Turn: %v", err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server connection did not close")
	}
}

func TestServerAdmitsNextCommandWhileResultFrameIsObservable(t *testing.T) {
	t.Parallel()
	testServerAdmitsNextCommandWhileTerminalFrameIsObservable(t, TypeCommandResult, nil)
}

func TestServerAdmitsNextCommandWhileBackendErrorFrameIsObservable(t *testing.T) {
	t.Parallel()
	testServerAdmitsNextCommandWhileTerminalFrameIsObservable(t, TypeError, errors.New("backend command failed"))
}

func TestServerFailedCommandWriteNeverRunsPipelinedCommand(t *testing.T) {
	for _, tc := range []struct {
		name         string
		terminalType string
		firstErr     error
		failEvent    bool
	}{
		{name: "result", terminalType: TypeCommandResult},
		{name: "backend_error", terminalType: TypeError, firstErr: errors.New("backend command failed")},
		{name: "event_then_stable_error", terminalType: TypeError, failEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testServerFailedCommandWriteNeverRunsPipelinedCommand(t, tc.terminalType, tc.firstErr, tc.failEvent)
		})
	}
}

func TestServerCanceledSettlingWaiterPreservesPredecessorBarrier(t *testing.T) {
	t.Parallel()

	serverSide, clientSide := net.Pipe()
	blocking := &blockingReturnConn{
		Conn:      serverSide,
		frameType: TypeCommandResult,
		observed:  make(chan struct{}),
		release:   make(chan struct{}),
	}
	backend := &terminalOrderBackend{
		wireFakeBackend: newWireFake("canceled-waiter-order"),
		calls:           make(chan chat.ParsedCommand, 3),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() {
		serveConn(ctx, blocking, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
		close(serverDone)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	t.Cleanup(func() {
		release()
		cancel()
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	codec := NewClientCodec(clientSide, clientSide)
	if err := encodeTestFrame(codec, TypeOpen, "open", chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(); err != nil {
		t.Fatal(err)
	}
	blocking.armed.Store(true)
	if err := encodeTestFrame(codec, TypeCommand, "first", chat.ParsedCommand{Name: chat.CommandSkill, Args: "first"}); err != nil {
		t.Fatal(err)
	}
	assertTerminalOrderCommandCall(t, ctx, backend.calls, chat.CommandSkill)
	firstTerminal, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if firstTerminal.Type != TypeCommandResult || firstTerminal.ID != "first" {
		t.Fatalf("first terminal frame = %+v", firstTerminal)
	}
	select {
	case <-blocking.observed:
	case <-ctx.Done():
		t.Fatal("first terminal frame write did not remain blocked")
	}
	if err := encodeTestFrame(codec, TypeCommand, "second", chat.ParsedCommand{Name: chat.CommandStatus, Args: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := encodeTestFrame(codec, TypeCancel, "cancel", nil); err != nil {
		t.Fatal(err)
	}
	if err := encodeTestFrame(codec, TypeCommand, "third", chat.ParsedCommand{Name: chat.CommandUsage, Args: "third"}); err != nil {
		t.Fatal(err)
	}
	assertNoTerminalOrderCommandCall(t, backend.calls)
	release()
	secondTerminal, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if secondTerminal.Type != TypeError || secondTerminal.ID != "second" {
		t.Fatalf("second terminal frame = %+v", secondTerminal)
	}
	assertTerminalOrderCommandCall(t, ctx, backend.calls, chat.CommandUsage)
	thirdTerminal, err := codec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if thirdTerminal.Type != TypeCommandResult || thirdTerminal.ID != "third" {
		t.Fatalf("third terminal frame = %+v", thirdTerminal)
	}
	_ = clientSide.Close()
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server connection did not close")
	}
}

func testServerAdmitsNextCommandWhileTerminalFrameIsObservable(t *testing.T, terminalType string, firstErr error) {
	t.Helper()

	serverSide, clientSide := net.Pipe()
	blocking := &blockingReturnConn{
		Conn:      serverSide,
		frameType: terminalType,
		observed:  make(chan struct{}),
		release:   make(chan struct{}),
	}
	backend := &terminalOrderBackend{
		wireFakeBackend: newWireFake("command-terminal-order"),
		calls:           make(chan chat.ParsedCommand, 2),
		firstErr:        firstErr,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() {
		serveConn(ctx, blocking, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
		close(serverDone)
	}()
	observedClient := &observedRequestConn{
		Conn:     clientSide,
		needle:   []byte(`"args":"second"`),
		observed: make(chan struct{}),
	}
	client := &Client{
		conn:      observedClient,
		codec:     NewClientCodec(observedClient, observedClient),
		pending:   make(map[string]chan Frame),
		routeDone: make(map[string]chan struct{}),
		closed:    make(chan struct{}),
	}
	go client.readLoop()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	t.Cleanup(func() {
		release()
		cancel()
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	blocking.armed.Store(true)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandSkill, Args: "first"}, nil)
		firstDone <- err
	}()
	assertTerminalOrderCommandCall(t, ctx, backend.calls, chat.CommandSkill)
	select {
	case <-blocking.observed:
	case <-ctx.Done():
		t.Fatal("first command terminal frame was not observed")
	}
	if err := <-firstDone; !errors.Is(err, firstErr) {
		var remote *RemoteError
		if firstErr == nil || !errors.As(err, &remote) || remote.Code != "command_failed" {
			t.Fatalf("first Command error = %#v, want %v", err, firstErr)
		}
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus, Args: "second"}, nil)
		secondDone <- err
	}()
	select {
	case <-observedClient.observed:
	case <-ctx.Done():
		t.Fatal("second command request was not written")
	}
	assertNoTerminalOrderCommandCall(t, backend.calls)
	release()
	assertTerminalOrderCommandCall(t, ctx, backend.calls, chat.CommandStatus)
	if err := <-secondDone; err != nil {
		t.Fatalf("second Command: %v", err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server connection did not close")
	}
}

func testServerFailedCommandWriteNeverRunsPipelinedCommand(t *testing.T, terminalType string, firstErr error, failEvent bool) {
	t.Helper()

	serverSide, clientSide := net.Pipe()
	failing := &terminalWriteFailureConn{
		Conn:         serverSide,
		terminalType: terminalType,
		attempted:    make(chan struct{}),
		release:      make(chan struct{}),
	}
	backend := &terminalOrderBackend{
		wireFakeBackend: newWireFake("failed-command-write"),
		calls:           make(chan chat.ParsedCommand, 2),
		firstErr:        firstErr,
		firstEvent:      failEvent,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	go func() {
		serveConn(ctx, failing, func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
		close(serverDone)
	}()
	observedClient := &observedRequestConn{
		Conn:     clientSide,
		needle:   []byte(`"args":"second"`),
		observed: make(chan struct{}),
	}
	client := &Client{
		conn:      observedClient,
		codec:     NewClientCodec(observedClient, observedClient),
		pending:   make(map[string]chan Frame),
		routeDone: make(map[string]chan struct{}),
		closed:    make(chan struct{}),
	}
	go client.readLoop()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(failing.release) }) }
	t.Cleanup(func() {
		release()
		cancel()
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	if _, err := client.Open(ctx, chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	failing.armed.Store(true)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandSkill, Args: "first"}, nil)
		firstDone <- err
	}()
	assertTerminalOrderCommandCall(t, ctx, backend.calls, chat.CommandSkill)
	select {
	case <-failing.attempted:
	case <-ctx.Done():
		t.Fatal("first command terminal write was not attempted")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := client.Command(ctx, chat.ParsedCommand{Name: chat.CommandStatus, Args: "second"}, nil)
		secondDone <- err
	}()
	select {
	case <-observedClient.observed:
	case <-ctx.Done():
		t.Fatal("pipelined command request was not written")
	}
	assertNoTerminalOrderCommandCall(t, backend.calls)
	release()
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server connection did not close after terminal write failure")
	}
	if err := <-firstDone; err == nil {
		t.Fatal("first Command unexpectedly succeeded")
	}
	if err := <-secondDone; err == nil {
		t.Fatal("pipelined Command unexpectedly succeeded")
	}
	select {
	case parsed := <-backend.calls:
		t.Fatalf("pipelined command reached backend after terminal write failure: %q", parsed.Name)
	default:
	}
}

func assertTerminalOrderCommandCall(t *testing.T, ctx context.Context, calls <-chan chat.ParsedCommand, want chat.Name) {
	t.Helper()
	select {
	case parsed := <-calls:
		if parsed.Name != want {
			t.Fatalf("backend command = %q, want %q", parsed.Name, want)
		}
	case <-ctx.Done():
		t.Fatalf("backend command %q was not called: %v", want, ctx.Err())
	}
}

func assertNoTerminalOrderCommandCall(t *testing.T, calls <-chan chat.ParsedCommand) {
	t.Helper()
	select {
	case parsed := <-calls:
		t.Fatalf("backend command %q started before predecessor terminal write settled", parsed.Name)
	case <-time.After(100 * time.Millisecond):
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

func TestServerDisconnectedAuditUsesDetachedBoundedContext(t *testing.T) {
	t.Parallel()

	type auditObservation struct {
		event       string
		err         error
		hasDeadline bool
	}
	observations := make(chan auditObservation, 2)
	path, stop := startChatwireServer(t, func(context.Context) (chat.Backend, error) {
		return newWireFake("audit-context"), nil
	}, func(ctx context.Context, _ net.Conn, event string) {
		_, hasDeadline := ctx.Deadline()
		observations <- auditObservation{event: event, err: ctx.Err(), hasDeadline: hasDeadline}
	})
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
	connected := <-observations
	disconnected := <-observations
	if connected.event != "connected" || disconnected.event != "disconnected" {
		t.Fatalf("audit events = %+v, %+v", connected, disconnected)
	}
	if disconnected.err != nil || !disconnected.hasDeadline {
		t.Fatalf("disconnected audit context = %+v, want live bounded context", disconnected)
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
	mu             sync.Mutex
	state          chat.State
	turnStarted    chan struct{}
	turnReleased   chan struct{}
	closed         chan struct{}
	startedOnce    sync.Once
	releasedOnce   sync.Once
	closedOnce     sync.Once
	blockTurn      bool
	ignoreCancel   bool
	turnEvent      chat.Event
	turnErr        error
	commandEvent   chat.Event
	blockCommand   bool
	commandStarted chan struct{}
	commandOnce    sync.Once
	commandResult  chat.Result
	cancels        int
	closes         int
	closeErr       error
}

type wireSafeCloseError struct{ message string }

func (e wireSafeCloseError) Error() string       { return e.message }
func (e wireSafeCloseError) SafeMessage() string { return e.message }

type errorListener struct{ err error }

func (l errorListener) Accept() (net.Conn, error) { return nil, l.err }
func (errorListener) Close() error                { return nil }
func (errorListener) Addr() net.Addr              { return &net.UnixAddr{Name: "test", Net: "unix"} }

type toggleWriteConn struct {
	net.Conn
	fail atomic.Bool
}

type partialWriteConn struct{ net.Conn }

func (c *partialWriteConn) Write([]byte) (int, error) {
	return 1, errors.New("injected partial write")
}

func (c *toggleWriteConn) Write(payload []byte) (int, error) {
	if c.fail.Load() {
		return 0, errors.New("injected write failure")
	}
	return c.Conn.Write(payload)
}

type blockingReturnConn struct {
	net.Conn
	armed     atomic.Bool
	once      sync.Once
	frameType string
	observed  chan struct{}
	release   chan struct{}
}

type observedRequestConn struct {
	net.Conn
	once     sync.Once
	needle   []byte
	observed chan struct{}
}

func (c *observedRequestConn) Write(payload []byte) (int, error) {
	written, err := c.Conn.Write(payload)
	if err == nil && bytes.Contains(payload, c.needle) {
		c.once.Do(func() { close(c.observed) })
	}
	return written, err
}

type terminalWriteFailureConn struct {
	net.Conn
	armed        atomic.Bool
	once         sync.Once
	terminalType string
	attempted    chan struct{}
	release      chan struct{}
}

func (c *terminalWriteFailureConn) Write(payload []byte) (int, error) {
	if bytes.Contains(payload, []byte(`"type":"`+c.terminalType+`"`)) && c.armed.CompareAndSwap(true, false) {
		c.once.Do(func() { close(c.attempted) })
		<-c.release
		return 0, errors.New("injected terminal write failure")
	}
	return c.Conn.Write(payload)
}

func (c *blockingReturnConn) Write(payload []byte) (int, error) {
	written, err := c.Conn.Write(payload)
	frameType := c.frameType
	if frameType == "" {
		frameType = TypeTurnDone
	}
	if err == nil && bytes.Contains(payload, []byte(`"type":"`+frameType+`"`)) && c.armed.CompareAndSwap(true, false) {
		c.once.Do(func() { close(c.observed) })
		<-c.release
	}
	return written, err
}

type terminalOrderBackend struct {
	*wireFakeBackend
	calls      chan chat.ParsedCommand
	firstErr   error
	firstEvent bool
}

func (b *terminalOrderBackend) Command(_ context.Context, parsed chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	b.calls <- parsed
	if parsed.Name == chat.CommandSkill && b.firstEvent {
		emit(chat.Event{Kind: chat.EventNotice, Text: strings.Repeat("x", MaxFrameBytes)})
	}
	if parsed.Name == chat.CommandSkill && b.firstErr != nil {
		return chat.Result{}, b.firstErr
	}
	return b.commandResult, nil
}

func newWireFake(id string) *wireFakeBackend {
	return &wireFakeBackend{
		state:          chat.State{SessionID: id, Title: id, ConnectionMode: "unix"},
		turnStarted:    make(chan struct{}),
		turnReleased:   make(chan struct{}),
		closed:         make(chan struct{}),
		commandStarted: make(chan struct{}),
		commandResult:  chat.Result{Title: "status", Text: id},
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

func (b *wireFakeBackend) Command(ctx context.Context, _ chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	b.commandOnce.Do(func() { close(b.commandStarted) })
	if b.commandEvent.Kind != "" && emit != nil {
		emit(b.commandEvent)
	}
	if b.blockCommand {
		<-ctx.Done()
		return chat.Result{}, ctx.Err()
	}
	return b.commandResult, nil
}

func (b *wireFakeBackend) waitCommandStarted(t *testing.T) {
	t.Helper()
	select {
	case <-b.commandStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("backend command did not start")
	}
}

func (b *wireFakeBackend) Cancel() {
	b.mu.Lock()
	b.cancels++
	ignore := b.ignoreCancel
	b.mu.Unlock()
	if ignore {
		return
	}
	b.releasedOnce.Do(func() { close(b.turnReleased) })
}

func (b *wireFakeBackend) Close(context.Context) error {
	b.mu.Lock()
	b.closes++
	b.mu.Unlock()
	b.closedOnce.Do(func() { close(b.closed) })
	return b.closeErr
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
