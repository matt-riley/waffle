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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/chat"
)

const (
	serverCloseTimeout = 5 * time.Second
	serverWriteTimeout = 5 * time.Second
	serverAuditTimeout = 2 * time.Second
	maxRequestIDBytes  = 128
)

var errServerWriteInterrupted = errors.New("chat wire server write interrupted")
var errServerWriteCorrupted = errors.New("chat wire server write partially emitted")

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
		defer func() {
			auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(ctx), serverAuditTimeout)
			defer auditCancel()
			audit(auditCtx, conn, "disconnected")
		}()
	}

	trackedWriter := &serverTrackedWriter{conn: conn}
	codec := NewServerCodec(conn, trackedWriter)
	writer := &serverWriter{conn: conn, codec: codec, trackedWriter: trackedWriter}
	openFrame, err := codec.Decode()
	if err != nil {
		var mismatch *ProtocolVersionError
		if errors.As(err, &mismatch) {
			message := fmt.Sprintf("chat protocol mismatch: client version %d, service version %d; deploy the matching Waffle binary and waffle service together", mismatch.Got, ProtocolVersion)
			_ = writer.stableError(mismatch.ID, "protocol_version_mismatch", message)
			return
		}
		_ = writer.stableError("", "protocol_error", "invalid chat protocol frame")
		return
	}
	if !validRequestID(openFrame.ID) {
		_ = writer.stableError("", "invalid_request", "invalid chat request id")
		return
	}
	if openFrame.Type != TypeOpen {
		_ = writer.stableError(openFrame.ID, "open_required", "open must be the first chat request")
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
	var active *activeTurnState
	var turnGroup sync.WaitGroup
	var commandMu sync.Mutex
	var activeCommand *activeTurnState
	var commandGroup sync.WaitGroup
	var deferredMu sync.Mutex
	var deferred []deferredServerFrame
	deferFrame := func(frameType, id string, payload any) {
		deferredMu.Lock()
		deferred = append(deferred, deferredServerFrame{frameType: frameType, id: id, payload: payload})
		deferredMu.Unlock()
	}
	var cleanupOnce sync.Once
	var cleanupErr error
	cleanup := func() {
		cleanupOnce.Do(func() {
			activeMu.Lock()
			turnActive := active
			activeMu.Unlock()
			commandMu.Lock()
			command := activeCommand
			commandMu.Unlock()
			writer.interrupt()
			cancel()
			if turnActive != nil {
				turnActive.stop(backend)
			}
			if command != nil {
				command.cancel()
			}
			turnGroup.Wait()
			commandGroup.Wait()
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), serverCloseTimeout)
			defer closeCancel()
			cleanupErr = backend.Close(closeCtx)
		})
	}
	defer cleanup()

	state, err := backend.Open(ctx, options)
	if err != nil {
		_ = writeBackendError(writer, openFrame.ID, "open_failed", "could not open chat", err)
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
		if frame.Type != TypeCancel && !validRequestID(frame.ID) {
			if err := writer.stableError("", "invalid_request", "invalid chat request id"); err != nil {
				return
			}
			continue
		}
		switch frame.Type {
		case TypeOpen:
			if err := writer.stableError(frame.ID, "already_open", "chat connection is already open"); err != nil {
				return
			}
		case TypeTurn:
			var turn TurnPayload
			if err := decodePayload(frame, &turn); err != nil {
				if err := writer.stableError(frame.ID, "invalid_request", "invalid turn request"); err != nil {
					return
				}
				continue
			}
			turnCtx, turnCancel := context.WithCancel(ctx)
			turnState := &activeTurnState{cancel: turnCancel}
			activeMu.Lock()
			if active != nil {
				activeMu.Unlock()
				turnCancel()
				if err := writer.stableError(frame.ID, "turn_active", "a chat turn is already active"); err != nil {
					return
				}
				continue
			}
			active = turnState
			turnGroup.Add(1)
			activeMu.Unlock()
			go func(id, input string, turnCtx context.Context, turnCancel context.CancelFunc, turnState *activeTurnState) {
				defer turnGroup.Done()
				defer turnCancel()
				var eventMu sync.Mutex
				acceptEvents := true
				var doneEvent chat.Event
				var streamErr error
				turnErr := backend.Turn(turnCtx, input, func(event chat.Event) {
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
							turnState.stop(backend)
							if !errors.Is(err, ErrFrameTooLarge) && !errors.Is(err, errServerWriteInterrupted) {
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
				if active == turnState {
					active = nil
				}
				activeMu.Unlock()

				if streamErr != nil {
					if errors.Is(streamErr, errServerWriteInterrupted) {
						deferFrame(TypeTurnDone, id, doneEvent)
						return
					}
					if responseWriteFailure(writer, id, "turn_failed", "chat turn failed", streamErr) {
						cancel()
						_ = conn.Close()
					}
					return
				}
				if err := writer.send(TypeTurnDone, id, doneEvent); err != nil {
					if errors.Is(err, errServerWriteInterrupted) {
						deferFrame(TypeTurnDone, id, doneEvent)
						return
					}
					if responseWriteFailure(writer, id, "turn_failed", "chat turn failed", err) {
						cancel()
						_ = conn.Close()
					}
				}
			}(frame.ID, turn.Text, turnCtx, turnCancel, turnState)
		case TypeCommand:
			var command chat.ParsedCommand
			if err := decodePayload(frame, &command); err != nil {
				if err := writer.stableError(frame.ID, "invalid_request", "invalid command request"); err != nil {
					return
				}
				continue
			}
			commandCtx, commandCancel := context.WithCancel(ctx)
			commandState := &activeTurnState{cancel: commandCancel}
			commandMu.Lock()
			if activeCommand != nil {
				commandMu.Unlock()
				commandCancel()
				if err := writer.stableError(frame.ID, "command_active", "a chat command is already active"); err != nil {
					return
				}
				continue
			}
			activeCommand = commandState
			commandGroup.Add(1)
			commandMu.Unlock()
			go func(id string, parsed chat.ParsedCommand, commandCtx context.Context, commandCancel context.CancelFunc, commandState *activeTurnState) {
				defer commandGroup.Done()
				defer commandCancel()
				var streamErr error
				var eventMu sync.Mutex
				acceptEvents := true
				result, commandErr := backend.Command(commandCtx, parsed, func(event chat.Event) {
					eventMu.Lock()
					defer eventMu.Unlock()
					if !acceptEvents || streamErr != nil {
						return
					}
					if frameType, ok := eventFrameType(event.Kind); ok {
						if err := writer.send(frameType, id, event); err != nil {
							streamErr = err
							commandState.cancel()
							if !errors.Is(err, ErrFrameTooLarge) && !errors.Is(err, errServerWriteInterrupted) {
								cancel()
								_ = conn.Close()
							}
						}
					}
				})
				eventMu.Lock()
				acceptEvents = false
				eventMu.Unlock()
				if streamErr != nil {
					if responseWriteFailure(writer, id, "command_failed", "chat command failed", streamErr) {
						cancel()
						_ = conn.Close()
					}
					commandMu.Lock()
					if activeCommand == commandState {
						activeCommand = nil
					}
					commandMu.Unlock()
					return
				}
				if commandErr != nil {
					if err := writeBackendError(writer, id, "command_failed", "chat command failed", commandErr); err != nil && responseWriteFailure(writer, id, "command_failed", "chat command failed", err) {
						cancel()
						_ = conn.Close()
					}
					commandMu.Lock()
					if activeCommand == commandState {
						activeCommand = nil
					}
					commandMu.Unlock()
					return
				}
				if err := writer.send(TypeCommandResult, id, result); err != nil {
					if responseWriteFailure(writer, id, "command_failed", "chat command failed", err) {
						cancel()
						_ = conn.Close()
					}
				}
				commandMu.Lock()
				if activeCommand == commandState {
					activeCommand = nil
				}
				commandMu.Unlock()
			}(frame.ID, command, commandCtx, commandCancel, commandState)
		case TypeCancel:
			activeMu.Lock()
			turnActive := active
			activeMu.Unlock()
			if turnActive != nil {
				turnActive.stop(backend)
			}
			commandMu.Lock()
			command := activeCommand
			commandMu.Unlock()
			if command != nil {
				command.cancel()
			}
		case TypeClose:
			cleanup()
			if err := writer.resume(); err != nil {
				return
			}
			deferredMu.Lock()
			finalFrames := append([]deferredServerFrame(nil), deferred...)
			deferredMu.Unlock()
			for _, finalFrame := range finalFrames {
				if err := writer.send(finalFrame.frameType, finalFrame.id, finalFrame.payload); err != nil {
					return
				}
			}
			if cleanupErr != nil {
				if err := writeCloseWarning(writer, frame.ID, cleanupErr); err != nil {
					return
				}
			}
			_ = writer.send(TypeGoodbye, frame.ID, nil)
			return
		}
	}
}

func writeCloseWarning(writer *serverWriter, id string, closeErr error) error {
	type safeCloseError interface{ SafeMessage() string }
	message := "chat closed with a cleanup warning"
	var safe safeCloseError
	if errors.As(closeErr, &safe) && safe.SafeMessage() != "" {
		message = safe.SafeMessage()
	}
	return writer.stableError(id, "close_warning", message)
}

func writeBackendError(writer *serverWriter, id, fallbackCode, fallbackMessage string, backendErr error) error {
	type stableBackendError interface {
		ErrorCode() string
		SafeMessage() string
	}
	var stable stableBackendError
	if errors.As(backendErr, &stable) && stable.ErrorCode() != "" && stable.SafeMessage() != "" {
		return writer.stableError(id, stable.ErrorCode(), stable.SafeMessage())
	}
	return writer.stableError(id, fallbackCode, fallbackMessage)
}

type serverWriter struct {
	mu            sync.Mutex
	conn          net.Conn
	codec         *Codec
	trackedWriter *serverTrackedWriter
	writeTimeout  time.Duration
	interrupted   atomic.Bool
	corrupted     atomic.Bool
}

type serverTrackedWriter struct {
	conn    net.Conn
	written atomic.Int64
}

func (w *serverTrackedWriter) Write(payload []byte) (int, error) {
	n, err := w.conn.Write(payload)
	w.written.Add(int64(n))
	return n, err
}

type activeTurnState struct {
	cancel     context.CancelFunc
	cancelOnce sync.Once
}

func (s *activeTurnState) stop(backend chat.Backend) {
	s.cancel()
	s.cancelOnce.Do(backend.Cancel)
}

type deferredServerFrame struct {
	frameType string
	id        string
	payload   any
}

// responseWriteFailure reports whether the connection must be abandoned.
// Size rejection happens before any bytes are written, so a compact stable
// error can still be sent safely. I/O failures may have left a partial line and
// therefore require closing the connection instead of appending another frame.
func responseWriteFailure(writer *serverWriter, id, code, message string, cause error) bool {
	if errors.Is(cause, errServerWriteInterrupted) {
		return false
	}
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
	frame.Payload, err = sanitizePayload(frame.Payload)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.interrupted.Load() {
		return errServerWriteInterrupted
	}
	if w.trackedWriter != nil {
		w.trackedWriter.written.Store(0)
	}
	if w.conn != nil {
		timeout := w.writeTimeout
		if timeout <= 0 {
			timeout = serverWriteTimeout
		}
		if err := w.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("set chat wire write deadline: %w", err)
		}
		defer w.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // best-effort reset on a closing connection
		if w.interrupted.Load() {
			_ = w.conn.SetWriteDeadline(time.Now())
			return errServerWriteInterrupted
		}
	}
	err = w.codec.Encode(frame)
	if err != nil && w.trackedWriter != nil && w.trackedWriter.written.Load() > 0 {
		w.corrupted.Store(true)
		return fmt.Errorf("%w: %v", errServerWriteCorrupted, err)
	}
	if err != nil && w.interrupted.Load() {
		return errServerWriteInterrupted
	}
	return err
}

func (w *serverWriter) interrupt() {
	w.interrupted.Store(true)
	if w.conn != nil {
		_ = w.conn.SetWriteDeadline(time.Now())
	}
}

func (w *serverWriter) resume() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.corrupted.Load() {
		return errServerWriteCorrupted
	}
	if w.conn != nil {
		if err := w.conn.SetWriteDeadline(time.Time{}); err != nil {
			return fmt.Errorf("reset chat wire write deadline: %w", err)
		}
		if w.trackedWriter != nil {
			w.codec = NewServerCodec(nil, w.trackedWriter)
		} else {
			w.codec = NewServerCodec(nil, w.conn)
		}
	}
	w.interrupted.Store(false)
	return nil
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
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			base := sanitizeString(key)
			candidate := base
			for suffix := 2; ; suffix++ {
				if _, exists := clean[candidate]; !exists {
					break
				}
				candidate = fmt.Sprintf("%s#%d", base, suffix)
			}
			clean[candidate] = sanitizeValue(value[key])
		}
		return clean
	default:
		return value
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDBytes || !utf8.ValidString(id) || sanitizeString(id) != id {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func sanitizeString(value string) string {
	clean := strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
	for _, pattern := range sensitivePatterns {
		clean = pattern.ReplaceAllString(clean, "[redacted]")
	}
	return clean
}
