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
		case <-parent.Done():
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
			cancel()
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
			if err := writer.stableError(frame.ID, "already_open", "chat connection is already open"); err != nil {
				return
			}
		case TypeTurn:
			if frame.ID == "" {
				if err := writer.stableError("", "invalid_request", "chat request id is required"); err != nil {
					return
				}
				continue
			}
			var turn TurnPayload
			if err := decodePayload(frame, &turn); err != nil {
				if err := writer.stableError(frame.ID, "invalid_request", "invalid turn request"); err != nil {
					return
				}
				continue
			}
			activeMu.Lock()
			if active {
				activeMu.Unlock()
				if err := writer.stableError(frame.ID, "turn_active", "a chat turn is already active"); err != nil {
					return
				}
				continue
			}
			active = true
			turnGroup.Add(1)
			activeMu.Unlock()
			go func(id, input string) {
				defer turnGroup.Done()
				var eventMu sync.Mutex
				acceptEvents := true
				var doneEvent chat.Event
				var streamErr error
				turnErr := backend.Turn(ctx, input, func(event chat.Event) {
					eventMu.Lock()
					if !acceptEvents {
						eventMu.Unlock()
						return
					}
					if streamErr != nil {
						eventMu.Unlock()
						return
					}
					if event.Kind == chat.EventTurnDone {
						doneEvent = event
						eventMu.Unlock()
						return
					}
					if frameType, ok := eventFrameType(event.Kind); ok {
						if err := writer.send(frameType, id, event); err != nil {
							streamErr = err
							eventMu.Unlock()
							backend.Cancel()
							if !errors.Is(err, ErrFrameTooLarge) {
								cancel()
								_ = conn.Close()
							}
							return
						}
					}
					eventMu.Unlock()
				})
				eventMu.Lock()
				acceptEvents = false
				eventMu.Unlock()
				if doneEvent.Kind == "" {
					doneEvent.Kind = chat.EventTurnDone
				}
				if turnErr != nil {
					doneEvent.IsError = true
				}

				activeMu.Lock()
				active = false
				activeMu.Unlock()

				if streamErr != nil {
					if responseWriteFailure(writer, id, "turn_failed", "chat turn failed", streamErr) {
						cancel()
						_ = conn.Close()
					}
					return
				}
				if err := writer.send(TypeTurnDone, id, doneEvent); err != nil {
					if responseWriteFailure(writer, id, "turn_failed", "chat turn failed", err) {
						cancel()
						_ = conn.Close()
					}
				}
			}(frame.ID, turn.Text)
		case TypeCommand:
			if frame.ID == "" {
				if err := writer.stableError("", "invalid_request", "chat request id is required"); err != nil {
					return
				}
				continue
			}
			var command chat.ParsedCommand
			if err := decodePayload(frame, &command); err != nil {
				if err := writer.stableError(frame.ID, "invalid_request", "invalid command request"); err != nil {
					return
				}
				continue
			}
			var streamErr error
			var eventMu sync.Mutex
			acceptEvents := true
			result, err := backend.Command(ctx, command, func(event chat.Event) {
				eventMu.Lock()
				if !acceptEvents {
					eventMu.Unlock()
					return
				}
				if streamErr != nil {
					eventMu.Unlock()
					return
				}
				if frameType, ok := eventFrameType(event.Kind); ok {
					if err := writer.send(frameType, frame.ID, event); err != nil {
						streamErr = err
						eventMu.Unlock()
						backend.Cancel()
						if !errors.Is(err, ErrFrameTooLarge) {
							cancel()
							_ = conn.Close()
						}
						return
					}
				}
				eventMu.Unlock()
			})
			eventMu.Lock()
			acceptEvents = false
			eventMu.Unlock()
			if streamErr != nil {
				if responseWriteFailure(writer, frame.ID, "command_failed", "chat command failed", streamErr) {
					cancel()
					_ = conn.Close()
					return
				}
				continue
			}
			if err != nil {
				if err := writer.stableError(frame.ID, "command_failed", "chat command failed"); err != nil {
					return
				}
				continue
			}
			if err := writer.send(TypeCommandResult, frame.ID, result); err != nil {
				_ = responseWriteFailure(writer, frame.ID, "command_failed", "chat command failed", err)
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

// responseWriteFailure reports whether the connection must be abandoned.
// Size rejection happens before any bytes are written, so a compact stable
// error can still be sent safely. I/O failures may have left a partial line and
// therefore require closing the connection instead of appending another frame.
func responseWriteFailure(writer *serverWriter, id, code, message string, cause error) bool {
	if errors.Is(cause, ErrFrameTooLarge) {
		return writer.stableError(id, code, message) != nil
	}
	return true
}

func (w *serverWriter) stableError(id, code, message string) error {
	return w.send(TypeError, id, ErrorPayload{Code: code, Message: message})
}

func (w *serverWriter) send(frameType, id string, payload any) error {
	frame, err := newFrame(frameType, id, payload)
	if err != nil {
		return err
	}
	frame.ID = sanitizeString(frame.ID)
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
		return sanitizeString(value)
	case []any:
		for i := range value {
			value[i] = sanitizeValue(value[i])
		}
		return value
	case map[string]any:
		clean := make(map[string]any, len(value))
		for key, item := range value {
			clean[sanitizeString(key)] = sanitizeValue(item)
		}
		return clean
	default:
		return value
	}
}

func sanitizeString(value string) string {
	clean := strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
	for _, pattern := range sensitivePatterns {
		clean = pattern.ReplaceAllString(clean, "[redacted]")
	}
	return clean
}
