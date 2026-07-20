package chatwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
)

const serverCloseTimeout = 5 * time.Second

// Factory constructs one isolated backend for one accepted connection.
type Factory func(context.Context) (chat.Backend, error)

// AuditFunc records a connection lifecycle event without protocol payloads.
type AuditFunc func(context.Context, net.Conn, string)

// Serve accepts local chat connections until ctx is canceled or the listener fails.
func Serve(ctx context.Context, listener net.Listener, factory Factory, audit AuditFunc) error {
	if listener == nil {
		return errors.New("chat wire listener is nil")
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	listenerDone := make(chan struct{})
	go func() {
		select {
		case <-serveCtx.Done():
			_ = listener.Close()
		case <-listenerDone:
		}
	}()
	defer close(listenerDone)

	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept chat connection: %w", err)
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			serveConn(serveCtx, conn, factory, audit)
		}()
	}
}

func serveConn(parent context.Context, conn net.Conn, factory Factory, audit AuditFunc) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-connDone:
		}
	}()
	defer close(connDone)
	defer func() { _ = conn.Close() }()
	if audit != nil {
		audit(ctx, conn, "connected")
		defer audit(ctx, conn, "disconnected")
	}

	codec := NewServerCodec(conn, conn)
	writer := &serverWriter{codec: codec}
	openFrame, err := codec.Decode()
	if err != nil {
		_ = writer.stableError("", "protocol_error", "invalid chat protocol frame")
		return
	}
	if openFrame.Type != TypeOpen {
		_ = writer.stableError(openFrame.ID, "open_required", "open must be the first chat request")
		return
	}
	if openFrame.ID == "" {
		_ = writer.stableError("", "invalid_request", "chat request id is required")
		return
	}
	var options chat.OpenOptions
	if err := decodePayload(openFrame, &options); err != nil {
		_ = writer.stableError(openFrame.ID, "invalid_request", "invalid open request")
		return
	}
	if factory == nil {
		_ = writer.stableError(openFrame.ID, "backend_unavailable", "chat service is unavailable")
		return
	}
	backend, err := factory(ctx)
	if err != nil || backend == nil {
		_ = writer.stableError(openFrame.ID, "backend_unavailable", "chat service is unavailable")
		return
	}

	var activeMu sync.Mutex
	active := false
	var turnGroup sync.WaitGroup
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			activeMu.Lock()
			turnActive := active
			activeMu.Unlock()
			if turnActive {
				backend.Cancel()
			}
			turnGroup.Wait()
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), serverCloseTimeout)
			defer closeCancel()
			_ = backend.Close(closeCtx)
		})
	}
	defer cleanup()

	state, err := backend.Open(ctx, options)
	if err != nil {
		_ = writer.stableError(openFrame.ID, "open_failed", "could not open chat")
		return
	}
	if err := writer.send(TypeReady, openFrame.ID, state); err != nil {
		return
	}

	for {
		frame, err := codec.Decode()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
				_ = writer.stableError("", "protocol_error", "invalid chat protocol frame")
			}
			return
		}
		switch frame.Type {
		case TypeOpen:
			_ = writer.stableError(frame.ID, "already_open", "chat connection is already open")
		case TypeTurn:
			if frame.ID == "" {
				_ = writer.stableError("", "invalid_request", "chat request id is required")
				continue
			}
			var turn TurnPayload
			if err := decodePayload(frame, &turn); err != nil {
				_ = writer.stableError(frame.ID, "invalid_request", "invalid turn request")
				continue
			}
			activeMu.Lock()
			if active {
				activeMu.Unlock()
				_ = writer.stableError(frame.ID, "turn_active", "a chat turn is already active")
				continue
			}
			active = true
			turnGroup.Add(1)
			activeMu.Unlock()
			go func(id, input string) {
				defer turnGroup.Done()
				defer func() {
					activeMu.Lock()
					active = false
					activeMu.Unlock()
				}()
				var doneEvent chat.Event
				err := backend.Turn(ctx, input, func(event chat.Event) {
					if event.Kind == chat.EventTurnDone {
						doneEvent = event
						return
					}
					if frameType, ok := eventFrameType(event.Kind); ok {
						_ = writer.send(frameType, id, event)
					}
				})
				if err != nil {
					if ctx.Err() == nil {
						_ = writer.stableError(id, "turn_failed", "chat turn failed")
					}
					return
				}
				if doneEvent.Kind == "" {
					doneEvent.Kind = chat.EventTurnDone
				}
				_ = writer.send(TypeTurnDone, id, doneEvent)
			}(frame.ID, turn.Text)
		case TypeCommand:
			if frame.ID == "" {
				_ = writer.stableError("", "invalid_request", "chat request id is required")
				continue
			}
			var command chat.ParsedCommand
			if err := decodePayload(frame, &command); err != nil {
				_ = writer.stableError(frame.ID, "invalid_request", "invalid command request")
				continue
			}
			result, err := backend.Command(ctx, command, func(event chat.Event) {
				if frameType, ok := eventFrameType(event.Kind); ok {
					_ = writer.send(frameType, frame.ID, event)
				}
			})
			if err != nil {
				_ = writer.stableError(frame.ID, "command_failed", "chat command failed")
				continue
			}
			if err := writer.send(TypeCommandResult, frame.ID, result); err != nil {
				return
			}
		case TypeCancel:
			activeMu.Lock()
			turnActive := active
			activeMu.Unlock()
			if turnActive {
				backend.Cancel()
			}
		case TypeClose:
			if frame.ID == "" {
				_ = writer.stableError("", "invalid_request", "chat request id is required")
				continue
			}
			cleanup()
			_ = writer.send(TypeGoodbye, frame.ID, nil)
			return
		}
	}
}

type serverWriter struct {
	mu    sync.Mutex
	codec *Codec
}

func (w *serverWriter) stableError(id, code, message string) error {
	return w.send(TypeError, id, ErrorPayload{Code: code, Message: message})
}

func (w *serverWriter) send(frameType, id string, payload any) error {
	frame, err := newFrame(frameType, id, payload)
	if err != nil {
		return err
	}
	frame.Payload, err = sanitizePayload(frame.Payload)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.codec.Encode(frame)
}

func eventFrameType(kind chat.EventKind) (string, bool) {
	switch kind {
	case chat.EventState:
		return TypeState, true
	case chat.EventTextDelta:
		return TypeTextDelta, true
	case chat.EventToolStarted:
		return TypeToolStarted, true
	case chat.EventToolFinished:
		return TypeToolFinished, true
	case chat.EventNotice:
		return TypeNotice, true
	case chat.EventTurnDone:
		return TypeTurnDone, true
	default:
		return "", false
	}
}

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`AGE-SECRET-KEY-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`/var/lib/waffle(?:/[A-Za-z0-9._/-]+)?`),
}

func sanitizePayload(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("sanitize chat wire payload: %w", err)
	}
	value = sanitizeValue(value)
	clean, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode sanitized chat wire payload: %w", err)
	}
	return clean, nil
}

func sanitizeValue(value any) any {
	switch value := value.(type) {
	case string:
		clean := strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
		for _, pattern := range sensitivePatterns {
			clean = pattern.ReplaceAllString(clean, "[redacted]")
		}
		return clean
	case []any:
		for i := range value {
			value[i] = sanitizeValue(value[i])
		}
		return value
	case map[string]any:
		for key, item := range value {
			value[key] = sanitizeValue(item)
		}
		return value
	default:
		return value
	}
}
