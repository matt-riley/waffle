package chatwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

const (
	clientCloseTimeout  = 5 * time.Second
	clientWriteTimeout  = 5 * time.Second
	clientCancelTimeout = 250 * time.Millisecond
)

// RemoteError is a stable, redacted error returned by the chat service.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	if e == nil {
		return "chat service error"
	}
	return fmt.Sprintf("chat service %s: %s", e.Code, e.Message)
}

// ConnectionUsable reports that the service returned a complete error frame
// and the underlying chat connection remains available for later requests.
func (e *RemoteError) ConnectionUsable() bool { return true }

// ErrorCode returns the stable service error code.
func (e *RemoteError) ErrorCode() string {
	if e == nil {
		return ""
	}
	return e.Code
}

// Client implements chat.Backend over a local Unix connection.
type Client struct {
	conn       net.Conn
	codec      *Codec
	writeInit  sync.Once
	writeToken chan struct{}

	pendingMu sync.Mutex
	pending   map[string]chan Frame
	routeDone map[string]chan struct{}
	closed    chan struct{}
	readErr   error
	closeOnce sync.Once
	nextID    atomic.Uint64

	closeMu   sync.Mutex
	closeDone chan struct{}
	closeErr  error
}

var _ chat.Backend = (*Client)(nil)

// Dial connects to an absolute, NUL-free Unix socket path.
func Dial(ctx context.Context, path string) (*Client, error) {
	if !filepath.IsAbs(path) || strings.IndexByte(path, 0) >= 0 {
		return nil, fmt.Errorf("chat socket path must be absolute and NUL-free")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial chat socket %q: %w", path, err)
	}
	client := &Client{
		conn:      conn,
		codec:     NewClientCodec(conn, conn),
		pending:   make(map[string]chan Frame),
		routeDone: make(map[string]chan struct{}),
		closed:    make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

// Open performs the versioned protocol handshake and returns initial state.
func (c *Client) Open(ctx context.Context, options chat.OpenOptions) (chat.State, error) {
	id, responses, err := c.startRequest(ctx, TypeOpen, options)
	if err != nil {
		return chat.State{}, err
	}
	defer c.finishRequest(id)
	frame, err := c.nextResponse(ctx, responses)
	if err != nil {
		var mismatch *ProtocolVersionError
		if errors.As(err, &mismatch) {
			return chat.State{}, fmt.Errorf("chat protocol mismatch: client version %d, service version %d; deploy the matching Waffle binary and waffle service together: %w", ProtocolVersion, mismatch.Got, err)
		}
		return chat.State{}, err
	}
	if frame.Type == TypeError {
		return chat.State{}, remoteError(frame)
	}
	if frame.Type != TypeReady {
		return chat.State{}, unexpectedResponse(frame, TypeReady)
	}
	var state chat.State
	if err := decodePayload(frame, &state); err != nil {
		return chat.State{}, err
	}
	return state, nil
}

// Turn streams events until the matching turn_done response.
func (c *Client) Turn(ctx context.Context, input string, emit func(chat.Event)) error {
	id, responses, err := c.startRequest(ctx, TypeTurn, TurnPayload{Text: input})
	if err != nil {
		return err
	}
	defer c.finishRequest(id)
	for {
		frame, err := c.nextResponse(ctx, responses)
		if err != nil {
			return err
		}
		if frame.Type == TypeError {
			return remoteError(frame)
		}
		event, err := eventFromFrame(frame)
		if err != nil {
			return err
		}
		if emit != nil {
			emit(event)
		}
		if frame.Type == TypeTurnDone {
			if event.IsError {
				return &RemoteError{Code: "turn_failed", Message: "chat turn failed"}
			}
			return nil
		}
	}
}

// Command executes one parsed command and streams any intermediate events.
func (c *Client) Command(ctx context.Context, command chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	id, responses, err := c.startRequest(ctx, TypeCommand, command)
	if err != nil {
		return chat.Result{}, err
	}
	defer c.finishRequest(id)
	for {
		frame, err := c.nextResponse(ctx, responses)
		if err != nil {
			return chat.Result{}, err
		}
		switch frame.Type {
		case TypeError:
			return chat.Result{}, remoteError(frame)
		case TypeCommandResult:
			var result chat.Result
			if err := decodePayload(frame, &result); err != nil {
				return chat.Result{}, err
			}
			return result, nil
		default:
			event, err := eventFromFrame(frame)
			if err != nil {
				return chat.Result{}, err
			}
			if emit != nil {
				emit(event)
			}
		}
	}
}

// Cancel asks the service to cancel only this connection's active turn.
func (c *Client) Cancel() {
	ctx, cancel := context.WithTimeout(context.Background(), clientCancelTimeout)
	defer cancel()
	frame, err := newFrame(TypeCancel, "", nil)
	if err == nil {
		_ = c.writeFrame(ctx, frame)
	}
}

// Close gracefully closes the backend once, awaiting goodbye for at most five seconds.
func (c *Client) Close(ctx context.Context) error {
	c.closeMu.Lock()
	if c.closeDone != nil {
		done := c.closeDone
		c.closeMu.Unlock()
		select {
		case <-done:
			c.closeMu.Lock()
			err := c.closeErr
			c.closeMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.closeDone = make(chan struct{})
	done := c.closeDone
	c.closeMu.Unlock()

	err := c.close(ctx)
	c.closeMu.Lock()
	c.closeErr = err
	close(done)
	c.closeMu.Unlock()
	return err
}

func (c *Client) close(ctx context.Context) error {
	closeCtx, cancel := boundedContext(ctx, clientCloseTimeout)
	defer cancel()
	id, responses, err := c.startRequest(closeCtx, TypeClose, nil)
	if err == nil {
		var warning error
		for {
			frame, responseErr := c.nextResponse(closeCtx, responses)
			if responseErr != nil {
				err = errors.Join(warning, responseErr)
				break
			}
			switch frame.Type {
			case TypeError:
				warning = errors.Join(warning, remoteError(frame))
			case TypeGoodbye:
				err = warning
			default:
				err = errors.Join(warning, unexpectedResponse(frame, TypeGoodbye))
			}
			if frame.Type == TypeGoodbye || frame.Type != TypeError {
				break
			}
		}
		c.finishRequest(id)
	}
	closeErr := c.conn.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(err, closeErr)
}

func (c *Client) startRequest(ctx context.Context, frameType string, payload any) (string, chan Frame, error) {
	id := fmt.Sprintf("%d", c.nextID.Add(1))
	frame, err := newFrame(frameType, id, payload)
	if err != nil {
		return "", nil, err
	}
	responses := make(chan Frame, 16)
	done := make(chan struct{})
	c.pendingMu.Lock()
	select {
	case <-c.closed:
		err := c.readErrorLocked()
		c.pendingMu.Unlock()
		return "", nil, err
	default:
	}
	c.pending[id] = responses
	if c.routeDone == nil {
		c.routeDone = make(map[string]chan struct{})
	}
	c.routeDone[id] = done
	c.pendingMu.Unlock()
	if err := c.writeFrame(ctx, frame); err != nil {
		c.finishRequest(id)
		return "", nil, err
	}
	return id, responses, nil
}

func (c *Client) finishRequest(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	done, exists := c.routeDone[id]
	delete(c.routeDone, id)
	c.pendingMu.Unlock()
	if exists {
		close(done)
	}
}

func (c *Client) nextResponse(ctx context.Context, responses <-chan Frame) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	default:
	}
	select {
	case frame := <-responses:
		return frame, nil
	default:
	}
	select {
	case frame := <-responses:
		return frame, nil
	case <-c.closed:
		select {
		case frame := <-responses:
			return frame, nil
		default:
		}
		c.pendingMu.Lock()
		err := c.readErrorLocked()
		c.pendingMu.Unlock()
		return Frame{}, err
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

func (c *Client) writeFrame(ctx context.Context, frame Frame) error {
	c.writeInit.Do(func() { c.writeToken = make(chan struct{}, 1) })
	select {
	case c.writeToken <- struct{}{}:
		defer func() { <-c.writeToken }()
	case <-ctx.Done():
		return ctx.Err()
	}
	writeCtx, cancel := boundedContext(ctx, clientWriteTimeout)
	defer cancel()
	deadline, _ := writeCtx.Deadline()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set chat write deadline: %w", err)
	}
	defer c.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // best-effort reset on a closing connection
	if err := c.codec.Encode(frame); err != nil {
		return fmt.Errorf("send chat %s: %w", frame.Type, err)
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		frame, err := c.codec.Decode()
		if err != nil {
			c.finishRead(err)
			return
		}
		c.pendingMu.Lock()
		responses := c.pending[frame.ID]
		done := c.routeDone[frame.ID]
		c.pendingMu.Unlock()
		if responses == nil {
			continue
		}
		select {
		case responses <- frame:
		case <-done:
		case <-c.closed:
			return
		}
	}
}

func (c *Client) finishRead(err error) {
	c.pendingMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.closeOnce.Do(func() { close(c.closed) })
	c.pendingMu.Unlock()
}

func (c *Client) readErrorLocked() error {
	if c.readErr == nil || errors.Is(c.readErr, io.EOF) || errors.Is(c.readErr, net.ErrClosed) {
		return errors.New("chat service disconnected")
	}
	return c.readErr
}

func remoteError(frame Frame) error {
	var payload ErrorPayload
	if err := decodePayload(frame, &payload); err != nil {
		return err
	}
	if payload.Code == "" || payload.Message == "" {
		return fmt.Errorf("%w: incomplete error payload", ErrMalformedFrame)
	}
	return &RemoteError{Code: payload.Code, Message: payload.Message}
}

func unexpectedResponse(frame Frame, expected string) error {
	return fmt.Errorf("%w: got %s response, want %s", ErrMalformedFrame, frame.Type, expected)
}

func eventFromFrame(frame Frame) (chat.Event, error) {
	expected := map[string]chat.EventKind{
		TypeState:        chat.EventState,
		TypeTextDelta:    chat.EventTextDelta,
		TypeToolStarted:  chat.EventToolStarted,
		TypeToolFinished: chat.EventToolFinished,
		TypeNotice:       chat.EventNotice,
		TypeTurnDone:     chat.EventTurnDone,
	}[frame.Type]
	if expected == "" {
		return chat.Event{}, unexpectedResponse(frame, "event")
	}
	var event chat.Event
	if err := decodePayload(frame, &event); err != nil {
		return chat.Event{}, err
	}
	if event.Kind != expected {
		return chat.Event{}, fmt.Errorf("%w: %s payload kind is %q", ErrMalformedFrame, frame.Type, event.Kind)
	}
	return event, nil
}

func boundedContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if existing, ok := parent.Deadline(); ok && existing.Before(deadline) {
		return context.WithCancel(parent)
	}
	return context.WithDeadline(parent, deadline)
}
