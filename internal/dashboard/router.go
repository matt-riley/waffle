package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/matt-riley/waffle/internal/chat"
)

const dashboardChatMaxBodyBytes = 64 << 10

const mutationResponseEnvelopeVersion = "waffle-desk-mutation-v1"

type MutationOutcomeObserver func(RestartScheduleOutcome)

type mutationResponseEnvelope struct {
	Version string      `json:"version"`
	Header  http.Header `json:"header"`
	Body    []byte      `json:"body"`
}

// RegisterRoutes mounts the Desk shell and exact live-state APIs on the
// caller-owned mux. Security.Wrap remains the caller's single outer boundary.
func RegisterRoutes(mux *http.ServeMux, config APIConfig) {
	mux.Handle("/desk/", ShellHandler(config.Security))
	mux.Handle("GET /api/v1/desk/bootstrap", newBootstrapHandler(config))
	mux.Handle("GET /api/v1/desk/events", newEventsHandler(config))
	if config.ChatClients != nil && config.Idempotency != nil {
		registerChatRoutes(mux, config)
	}
	if config.Operations != nil {
		RegisterTaskRoutes(mux, TaskRouteConfig{
			Operations:  config.Operations,
			Schedules:   config.Schedules,
			Security:    config.Security,
			Idempotency: config.Idempotency,
			Events:      config.Hub,
		})
		RegisterWorkspaceRoutes(mux, WorkspaceRouteConfig{
			Operations:  config.Operations,
			Security:    config.Security,
			Idempotency: config.Idempotency,
			Events:      config.Hub,
			Egress:      config.WorkspaceEgress,
		})
		RegisterMemoryRoutes(mux, MemoryRouteConfig{
			Operations:  config.Operations,
			Workspace:   config.Memory,
			Security:    config.Security,
			Idempotency: config.Idempotency,
			Events:      config.Hub,
		})
	}
	if config.Capabilities != nil && config.Idempotency != nil {
		RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
			Service: config.Capabilities,
			Mutation: func(limit int64, next http.Handler) http.Handler {
				return NewMutationHandler(config.Security, config.Idempotency, limit, next, config.RestartOutcome)
			},
			Restart: config.Restart,
		})
	}
}

func registerChatRoutes(mux *http.ServeMux, config APIConfig) {
	mutation := func(next http.Handler) http.Handler {
		return NewMutationHandler(config.Security, config.Idempotency, dashboardChatMaxBodyBytes, next)
	}
	mux.Handle("POST /api/v1/desk/chat/open", mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var options chat.OpenOptions
		if !decodeChatRequest(w, r, &options) {
			return
		}
		clientID, state, err := config.ChatClients.Open(r.Context(), options)
		if err != nil {
			writeChatError(w, err, "open_failed")
			return
		}
		writeChatJSON(w, http.StatusOK, struct {
			ClientID string     `json:"client_id"`
			State    chat.State `json:"state"`
		}{ClientID: clientID, State: config.ChatClients.safeChatState(state)})
	})))
	mux.Handle("POST /api/v1/desk/chat/turn", mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID string `json:"client_id"`
			Text     string `json:"text"`
		}
		if !decodeChatRequest(w, r, &request) {
			return
		}
		if err := config.ChatClients.Turn(r.Context(), request.ClientID, request.Text); err != nil {
			writeChatError(w, err, "turn_failed")
			return
		}
		writeChatJSON(w, http.StatusOK, struct{}{})
	})))
	mux.Handle("POST /api/v1/desk/chat/command", mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID string             `json:"client_id"`
			Command  chat.ParsedCommand `json:"command"`
		}
		if !decodeChatRequest(w, r, &request) {
			return
		}
		result, err := config.ChatClients.Command(r.Context(), request.ClientID, request.Command)
		if err != nil {
			writeChatError(w, err, "command_failed")
			return
		}
		writeChatJSON(w, http.StatusOK, config.ChatClients.safeChatResult(result))
	})))
	for _, route := range []struct {
		path string
		run  func(context.Context, string) error
	}{
		{path: "POST /api/v1/desk/chat/cancel", run: func(_ context.Context, clientID string) error { return config.ChatClients.Cancel(clientID) }},
		{path: "POST /api/v1/desk/chat/close", run: config.ChatClients.Close},
	} {
		route := route
		mux.Handle(route.path, mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				ClientID string `json:"client_id"`
			}
			if !decodeChatRequest(w, r, &request) {
				return
			}
			if err := route.run(r.Context(), request.ClientID); err != nil {
				writeChatError(w, err, "chat_failed")
				return
			}
			writeChatJSON(w, http.StatusOK, struct{}{})
		})))
	}
}

func decodeChatRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeChatJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeChatError(w http.ResponseWriter, err error, fallback string) {
	status := http.StatusBadRequest
	code, message := fallback, "chat request could not be completed"
	switch {
	case errors.Is(err, errChatClientNotFound):
		status, code, message = http.StatusNotFound, "chat_client_not_found", "chat client was not found"
	case errors.Is(err, errChatTurnActive):
		status, code, message = http.StatusConflict, "turn_active", "a chat turn is already active"
	case errors.Is(err, errChatUnavailable):
		status, code, message = http.StatusServiceUnavailable, "chat_unavailable", "chat service is unavailable"
	default:
		var coded interface {
			ErrorCode() string
		}
		if errors.As(err, &coded) && coded.ErrorCode() == "session_active" {
			status, code, message = http.StatusConflict, "session_active", "chat session is already active"
		}
	}
	writeChatJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

// NewMutationHandler adds request-bound idempotency to one exact dashboard
// mutation endpoint. Callers register its returned handler on that endpoint;
// it intentionally does not claim a route prefix.
func NewMutationHandler(
	security *Security,
	store *IdempotencyStore,
	maxBodyBytes int64,
	next http.Handler,
	observers ...MutationOutcomeObserver,
) http.Handler {
	return security.RequireMutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request_body_too_large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid_request_body", http.StatusBadRequest)
			return
		}
		defer clear(body)
		digest := sha256.Sum256(body)
		operation := r.Method + " " + r.URL.Path
		var capture *responseCapture
		executed := false
		status, responseBody, doErr := store.Do(r.Context(), r.Header.Get("Idempotency-Key"), operation, hex.EncodeToString(digest[:]), func(ctx context.Context) (int, []byte) {
			executed = true
			r.Body = io.NopCloser(bytes.NewReader(body))
			capture = newResponseCapture()
			next.ServeHTTP(capture, r.WithContext(ctx))
			header := capture.committedHeader
			if header == nil {
				header = capture.header.Clone()
			}
			envelope, marshalErr := json.Marshal(mutationResponseEnvelope{
				Version: mutationResponseEnvelopeVersion,
				Header:  header,
				Body:    append([]byte(nil), capture.body.Bytes()...),
			})
			if marshalErr != nil {
				return http.StatusInternalServerError, []byte("mutation_response_unavailable")
			}
			return capture.status, envelope
		})
		if doErr != nil && status == 0 {
			http.Error(w, "mutation_unavailable", http.StatusServiceUnavailable)
			return
		}
		var envelope mutationResponseEnvelope
		if err := json.Unmarshal(responseBody, &envelope); err == nil && envelope.Version == mutationResponseEnvelopeVersion {
			copyResponseHeader(w.Header(), envelope.Header)
			responseBody = envelope.Body
		}
		w.WriteHeader(status)
		_, _ = w.Write(responseBody)
		if !executed || capture == nil || len(capture.afterResponse) == 0 {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		for _, callback := range capture.afterResponse {
			outcome := callback()
			for _, observe := range observers {
				if observe != nil {
					observe(outcome)
				}
			}
		}
	}))
}

func copyResponseHeader(destination, source http.Header) {
	for name, values := range source {
		destination.Del(name)
		destination[name] = append([]string(nil), values...)
	}
}

type responseCapture struct {
	header          http.Header
	committedHeader http.Header
	body            bytes.Buffer
	status          int
	wroteHeader     bool
	afterResponse   []func() RestartScheduleOutcome
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: make(http.Header), status: http.StatusOK}
}

func (w *responseCapture) Header() http.Header {
	return w.header
}

func (w *responseCapture) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.committedHeader = w.header.Clone()
}

func (w *responseCapture) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.committedHeader = w.header.Clone()
	}
	return w.body.Write(body)
}

func (w *responseCapture) AfterResponse(callback func() RestartScheduleOutcome) {
	if callback != nil {
		w.afterResponse = append(w.afterResponse, callback)
	}
}
