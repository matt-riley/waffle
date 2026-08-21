package chatwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

func TestRemoteErrorReportsStableConnectionSemantics(t *testing.T) {
	err := &RemoteError{Code: "turn_failed", Message: "chat turn failed"}
	semantic, ok := any(err).(interface {
		ConnectionUsable() bool
		ErrorCode() string
	})
	if !ok {
		t.Fatal("RemoteError does not expose backend error semantics")
	}
	if !semantic.ConnectionUsable() || semantic.ErrorCode() != "turn_failed" {
		t.Fatalf("RemoteError semantics usable=%v code=%q", semantic.ConnectionUsable(), semantic.ErrorCode())
	}
}

func TestDialRequiresAbsoluteNULFreeUnixPath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"relative.sock", "/tmp/waffle\x00.sock"} {
		if _, err := Dial(context.Background(), path); err == nil {
			t.Fatalf("Dial(%q) succeeded", path)
		}
	}
}

func TestDialAllowsAbsoluteNonCleanUnixPath(t *testing.T) {
	t.Parallel()

	listener, path := unixListener(t)
	subdir := filepath.Join(filepath.Dir(path), "sub")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	nonCleanPath := filepath.Join(subdir, "..", filepath.Base(path))
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			close(accepted)
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, nonCleanPath)
	if err != nil {
		t.Fatalf("Dial(%q): %v", nonCleanPath, err)
	}
	_ = client.conn.Close()
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("listener did not accept non-clean absolute path")
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
	for _, want := range []string{"client version 1", "service version 2", "deploy", "binary", "service"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Fatalf("Open error = %q, want actionable %q guidance", err, want)
		}
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

func TestClientCanceledRequestDoesNotDrainHighVolumeBufferedRoute(t *testing.T) {
	t.Parallel()

	client := &Client{closed: make(chan struct{})}
	responses := make(chan Frame, 16)
	for range cap(responses) {
		responses <- Frame{Version: ProtocolVersion, Type: TypeNotice, ID: "abandoned"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	frame, err := client.nextResponse(ctx, responses)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("nextResponse = %+v, %v; want context cancellation", frame, err)
	}
}

func TestClientAbandonedFullRouteDoesNotBlockLaterResponses(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	client := &Client{
		conn:      clientConn,
		codec:     NewClientCodec(clientConn, clientConn),
		pending:   make(map[string]chan Frame),
		routeDone: make(map[string]chan struct{}),
		closed:    make(chan struct{}),
	}
	abandoned := make(chan Frame, 16)
	for range cap(abandoned) {
		abandoned <- Frame{Version: ProtocolVersion, Type: TypeNotice, ID: "abandoned"}
	}
	later := make(chan Frame, 1)
	client.pending["abandoned"] = abandoned
	client.routeDone["abandoned"] = make(chan struct{})
	client.pending["later"] = later
	client.routeDone["later"] = make(chan struct{})
	go client.readLoop()
	t.Cleanup(func() {
		client.finishRead(net.ErrClosed)
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	serverCodec := NewServerCodec(serverConn, serverConn)
	firstWrite := make(chan error, 1)
	go func() {
		firstWrite <- encodeTestFrame(serverCodec, TypeNotice, "abandoned", chat.Event{Kind: chat.EventNotice})
	}()
	select {
	case err := <-firstWrite:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server could not write abandoned response")
	}
	time.Sleep(20 * time.Millisecond)
	client.finishRequest("abandoned")

	secondWrite := make(chan error, 1)
	go func() {
		secondWrite <- encodeTestFrame(serverCodec, TypeNotice, "later", chat.Event{Kind: chat.EventNotice})
	}()
	select {
	case frame := <-later:
		if frame.ID != "later" {
			t.Fatalf("later frame = %+v", frame)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("abandoned full route blocked the client read loop")
	}
	if err := <-secondWrite; err != nil {
		t.Fatal(err)
	}
	client.finishRequest("later")

	serverCloseDone := make(chan error, 1)
	go func() {
		closeFrame, err := serverCodec.Decode()
		if err == nil {
			err = encodeTestFrame(serverCodec, TypeGoodbye, closeFrame.ID, nil)
		}
		serverCloseDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close after abandoned route: %v", err)
	}
	if err := <-serverCloseDone; err != nil {
		t.Fatalf("server close exchange: %v", err)
	}
	select {
	case <-client.closed:
	case <-ctx.Done():
		t.Fatal("client read loop did not exit after close")
	}
}

// maxUnixSocketPath is the practical sockaddr_un.sun_path budget (Darwin
// allows 104 bytes including the NUL terminator).
const maxUnixSocketPath = 100

func unixListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "waffle-chatwire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "chat.sock")
	listener, err := net.Listen("unix", path)
	if err != nil && len(path) >= maxUnixSocketPath {
		// A deep ambient TMPDIR (sandboxed CI, agent harnesses) overflows
		// Darwin's sun_path cap and bind fails with "invalid argument".
		// Retry from a short root so the suite does not depend on how deep
		// the host's temp directory happens to be.
		shortDir, shortErr := os.MkdirTemp("/tmp", "waffle-chatwire-")
		if shortErr != nil {
			t.Fatalf("listen unix %s: %v (short-path retry failed: %v)", path, err, shortErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
		path = filepath.Join(shortDir, "chat.sock")
		listener, err = net.Listen("unix", path)
	}
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

func TestClientCancelAndCloseAreBoundedBehindBlockedWrite(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		client, peer, entered, writeDone := blockedWriteClient(t)
		defer func() { _ = peer.Close() }()
		<-entered
		cancelDone := make(chan struct{})
		go func() {
			client.Cancel()
			close(cancelDone)
		}()
		select {
		case <-cancelDone:
		case <-time.After(350 * time.Millisecond):
			_ = client.conn.Close()
			<-writeDone
			t.Fatal("Cancel waited indefinitely behind an earlier write")
		}
		_ = client.conn.Close()
		<-writeDone
	})

	t.Run("close", func(t *testing.T) {
		client, peer, entered, writeDone := blockedWriteClient(t)
		defer func() { _ = peer.Close() }()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		started := time.Now()
		err := client.Close(ctx)
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("Close took %s behind an earlier write", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close error = %v, want deadline exceeded", err)
		}
		<-writeDone
	})
}

type writeObservedConn struct {
	net.Conn
	once    sync.Once
	entered chan struct{}
}

func (c *writeObservedConn) Write(payload []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(payload)
}

func blockedWriteClient(t *testing.T) (*Client, net.Conn, <-chan struct{}, <-chan error) {
	t.Helper()
	clientSide, peer := net.Pipe()
	observed := &writeObservedConn{Conn: clientSide, entered: make(chan struct{})}
	client := &Client{
		conn:      observed,
		codec:     NewClientCodec(observed, observed),
		pending:   make(map[string]chan Frame),
		routeDone: make(map[string]chan struct{}),
		closed:    make(chan struct{}),
	}
	frame, err := newFrame(TypeTurn, "blocked", TurnPayload{Text: strings.Repeat("x", MaxFrameBytes/2)})
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.writeFrame(context.Background(), frame) }()
	return client, peer, observed.entered, writeDone
}
