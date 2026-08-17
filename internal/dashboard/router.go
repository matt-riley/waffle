package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/policy"
)

const (
	// dashboardChatMaxBodyBytes is the JSON mutation cap for open, command,
	// export, cancel, and close. Turn is the only chat route that carries
	// inline attachments, so it uses the larger turn limit below.
	dashboardChatMaxBodyBytes = 64 << 10
	// dashboardChatTurnMaxBodyBytes allows a 12MiB JSON body on /turn so
	// a few base64 attachments can fit under llm.MaxMediaBytes each.
	dashboardChatTurnMaxBodyBytes = 12 << 20
)

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
	if config.ChatClients != nil && config.Artifacts != nil {
		RegisterArtifactRoutes(mux, ArtifactRouteConfig{
			Clients:     config.ChatClients,
			Artifacts:   config.Artifacts,
			Previews:    config.Previews,
			Security:    config.Security,
			Idempotency: config.Idempotency,
		})
	}
	if config.Operations != nil {
		RegisterTaskRoutes(mux, TaskRouteConfig{
			Operations:  config.Operations,
			Schedules:   config.Schedules,
			Options:     config.ScheduleOptions,
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
		RegisterProjectRoutes(mux, ProjectRouteConfig{
			Projects:    config.Projects,
			Operations:  config.Operations,
			Security:    config.Security,
			Idempotency: config.Idempotency,
		})
	}
	if config.Posture != nil {
		RegisterPostureRoutes(mux, PostureRouteConfig{Service: config.Posture})
	}
	if config.Profiles != nil {
		RegisterProfileRoutes(mux, ProfileRouteConfig{
			Editor: config.Profiles,
			Mutation: func(limit int64, next http.Handler) http.Handler {
				return NewMutationHandler(
					config.Security,
					config.Idempotency,
					limit,
					next,
					composeRestartOutcomeObservers(config.Hub, config.RestartOutcome),
				)
			},
		})
	}
	if config.Setup != nil {
		// Without an idempotency store the read-only checklist still mounts;
		// only the identity action, which is a guarded mutation, drops out.
		routes := SetupRouteConfig{Service: config.Setup, Restart: config.Restart}
		if config.Idempotency != nil {
			routes.Mutation = func(limit int64, next http.Handler) http.Handler {
				return NewMutationHandler(
					config.Security,
					config.Idempotency,
					limit,
					next,
					composeRestartOutcomeObservers(config.Hub, config.RestartOutcome),
				)
			}
		}
		RegisterSetupRoutes(mux, routes)
	}
	if config.Capabilities != nil && config.Idempotency != nil {
		RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
			Service: config.Capabilities,
			Mutation: func(limit int64, next http.Handler) http.Handler {
				return NewMutationHandler(
					config.Security,
					config.Idempotency,
					limit,
					next,
					composeRestartOutcomeObservers(config.Hub, config.RestartOutcome),
				)
			},
			Restart: config.Restart,
		})
	}
}

// composeRestartOutcomeObservers publishes the sanitized restart outcome on
// the Desk event hub and invokes any coordinator-supplied observer (logging).
func composeRestartOutcomeObservers(hub *EventHub, observe MutationOutcomeObserver) MutationOutcomeObserver {
	if hub == nil && observe == nil {
		return nil
	}
	return func(outcome RestartScheduleOutcome) {
		PublishRestartOutcome(hub, outcome)
		if observe != nil {
			observe(outcome)
		}
	}
}

func registerChatRoutes(mux *http.ServeMux, config APIConfig) {
	mutation := func(next http.Handler) http.Handler {
		return NewMutationHandler(config.Security, config.Idempotency, dashboardChatMaxBodyBytes, next)
	}
	turnMutation := func(next http.Handler) http.Handler {
		return NewMutationHandler(config.Security, config.Idempotency, dashboardChatTurnMaxBodyBytes, next)
	}
	mux.Handle("GET /api/v1/desk/chat/commands", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			Commands []chat.Command `json:"commands"`
		}{
			Commands: chat.Commands(),
		})
	}))
	mux.Handle("POST /api/v1/desk/chat/open", mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Continue         bool     `json:"continue"`
			SessionID        string   `json:"session_id"`
			Profile          string   `json:"profile"`
			Capabilities     []string `json:"capabilities"`
			Temporary        bool     `json:"temporary"`
			ReattachClientID string   `json:"reattach_client_id"`
			ReattachToken    string   `json:"reattach_token"`
		}
		if !decodeChatRequest(w, r, &request) {
			return
		}
		lease, state, err := config.ChatClients.OpenWithLease(
			r.Context(),
			chat.OpenOptions{
				Continue:     request.Continue,
				SessionID:    request.SessionID,
				Profile:      request.Profile,
				Capabilities: request.Capabilities,
				Temporary:    request.Temporary,
			},
			ChatClientLease{
				ClientID:      request.ReattachClientID,
				ReattachToken: request.ReattachToken,
			},
		)
		if err != nil {
			writeChatError(w, err, "open_failed")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			ClientID      string     `json:"client_id"`
			ReattachToken string     `json:"reattach_token"`
			State         chat.State `json:"state"`
		}{
			ClientID:      lease.ClientID,
			ReattachToken: lease.ReattachToken,
			State:         config.ChatClients.safeChatState(state),
		})
	})))
	mux.Handle("POST /api/v1/desk/chat/turn", turnMutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID        string           `json:"client_id"`
			Text            string           `json:"text"`
			Attachments     []deskAttachment `json:"attachments"`
			TaskMode        string           `json:"task_mode"`
			ReasoningEffort string           `json:"reasoning_effort"`
		}
		if !decodeChatRequest(w, r, &request) {
			return
		}
		options := chat.TurnModeOptions{}
		switch request.TaskMode {
		case "", "quick", "deep", "draft":
			options.TaskMode = request.TaskMode
		default:
			writeChatError(w, errors.New("invalid task mode"), "turn_failed")
			return
		}
		switch request.ReasoningEffort {
		case "", "low", "medium", "high":
			options.ReasoningEffort = request.ReasoningEffort
		default:
			writeChatError(w, errors.New("invalid reasoning effort"), "turn_failed")
			return
		}
		if len(request.Attachments) > 0 {
			media, err := buildDeskMediaBlocks(request.Attachments)
			if err != nil {
				writeChatError(w, err, "attachment_invalid")
				return
			}
			options.Media = media
		}
		if err := config.ChatClients.TurnModes(r.Context(), request.ClientID, request.Text, options); err != nil {
			writeChatError(w, err, "turn_failed")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
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
		writeJSON(w, http.StatusOK, config.ChatClients.safeChatResult(result))
	})))
	for _, route := range []struct {
		path string
		run  func(context.Context, string) error
	}{
		{path: "POST /api/v1/desk/chat/cancel", run: func(_ context.Context, clientID string) error { return config.ChatClients.Cancel(clientID) }},
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
			writeJSON(w, http.StatusOK, struct{}{})
		})))
	}
	mux.Handle("POST /api/v1/desk/chat/export", mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID      string `json:"client_id"`
			ReattachToken string `json:"reattach_token"`
			Format        string `json:"format"`
		}
		if !decodeChatRequest(w, r, &request) {
			return
		}
		format := request.Format
		if format != "markdown" && format != "json" {
			format = "markdown"
		}
		state, err := config.ChatClients.Export(ChatClientLease{
			ClientID:      request.ClientID,
			ReattachToken: request.ReattachToken,
		})
		if err != nil {
			writeChatError(w, err, "export_failed")
			return
		}
		safe := config.ChatClients.safeChatState(state)
		if format == "json" {
			writeJSON(w, http.StatusOK, struct {
				Format    string               `json:"format"`
				Version   int                  `json:"version"`
				SessionID string               `json:"session_id"`
				Title     string               `json:"title"`
				Profile   string               `json:"profile"`
				Workspace string               `json:"workspace"`
				Messages  []chat.ExportMessage `json:"messages"`
			}{
				Format: "waffle-desk-transcript", Version: 1,
				SessionID: safe.SessionID, Title: safe.Title, Profile: safe.Profile,
				Workspace: safe.Workspace,
				Messages:  chat.ExportMessages(safe.History),
			})
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="conversation.md"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chat.ExportMarkdown(safe.Title, safe.Profile, safe.History)))
	})))
	mux.Handle("POST /api/v1/desk/chat/close", mutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID      string `json:"client_id"`
			ReattachToken string `json:"reattach_token"`
		}
		if !decodeChatRequest(w, r, &request) {
			return
		}
		err := config.ChatClients.CloseWithLease(r.Context(), ChatClientLease{
			ClientID:      request.ClientID,
			ReattachToken: request.ReattachToken,
		})
		if err != nil {
			writeChatError(w, err, "chat_failed")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	})))
}

func decodeChatRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeStrictJSON(w, r, target, func(w http.ResponseWriter) {
		http.Error(w, "invalid_request", http.StatusBadRequest)
	})
}

// decodeStrictJSON decodes exactly one JSON object into target, rejecting
// unknown fields and any trailing data. onInvalid writes the domain-specific
// error response when decoding fails.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any, onInvalid func(http.ResponseWriter)) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		onInvalid(w)
		return false
	}
	return true
}

// errorResponse is the shared {code, message} shape returned by every Desk
// mutation and read endpoint on failure.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON writes value as the JSON response body with the given status.
func writeJSON(w http.ResponseWriter, status int, value any) {
	if writeNegotiatedValue(w, status, value) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// preserveResponseType restores the JSON Content-Type on a response that was
// replayed through an intermediate capture (e.g. an idempotency handler)
// without changing that shared primitive's status/body-only contract.
func preserveResponseType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := newResponseCapture()
		next.ServeHTTP(capture, r)
		header := capture.committedHeader
		if header == nil {
			header = capture.header
		}
		copyResponseHeader(w.Header(), header)
		if w.Header().Get("Content-Type") == "" && json.Valid(capture.body.Bytes()) {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(capture.status)
		_, _ = w.Write(capture.body.Bytes())
	})
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
	writeJSON(w, status, errorResponse{Code: code, Message: message})
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
	return newMutationHandler(security, store, maxBodyBytes, next, 0, observers...)
}

// NewDetachedMutationHandler behaves like NewMutationHandler, except the
// mutation runs with a context detached from client disconnect: it keeps
// running (and its response gets cached for replay) for up to timeout after
// the request context is cancelled, instead of being abandoned mid-write.
func NewDetachedMutationHandler(
	security *Security,
	store *IdempotencyStore,
	maxBodyBytes int64,
	next http.Handler,
	timeout time.Duration,
	observers ...MutationOutcomeObserver,
) http.Handler {
	return newMutationHandler(security, store, maxBodyBytes, next, timeout, observers...)
}

func newMutationHandler(
	security *Security,
	store *IdempotencyStore,
	maxBodyBytes int64,
	next http.Handler,
	detachTimeout time.Duration,
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
		run := func(ctx context.Context) (int, []byte) {
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
		}
		var status int
		var responseBody []byte
		var doErr error
		key, digestHex := r.Header.Get("Idempotency-Key"), hex.EncodeToString(digest[:])
		if detachTimeout > 0 {
			runCtx, cancel := detachedTimeoutContext(r.Context(), detachTimeout)
			defer cancel()
			status, responseBody, doErr = store.DoDetached(r.Context(), runCtx, key, operation, digestHex, run)
		} else {
			status, responseBody, doErr = store.Do(r.Context(), key, operation, digestHex, run)
		}
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
		// Record only first-execution mutations (not idempotent replays) so the
		// policy_audit trail matches real state changes on this surface.
		if executed {
			auditDeskMutation(r.Context(), security, operation, status)
		}
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

// auditDeskMutation writes one policy_audit row for an executed Desk mutation.
// The audit write is detached from the request context so a client disconnect
// after the response is written cannot cancel the policy_audit insert (#152 review).
func auditDeskMutation(ctx context.Context, security *Security, operation string, status int) {
	if security == nil {
		return
	}
	db := security.PolicyAuditDB()
	if db == nil {
		return
	}
	detail := "status=" + strconv.Itoa(status)
	err := policy.LogMutation(context.WithoutCancel(ctx), db, "", "desk.mutation", operation, detail)
	// The mutation is already durable by the time it is audited, so the write
	// cannot fail closed; the loss is reported instead of discarded (#297).
	policy.ReportAuditFailure(security.AuditLogger(), err, "", "desk.mutation", operation)
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

// deskAttachment is one browser-supplied attachment: a display name, an
// exact media type, and the base64 payload (#473).
type deskAttachment struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	DataB64   string `json:"data_base64"`
}

// buildDeskMediaBlocks validates browser-supplied attachments against the
// canonical llm allowlist and size bounds. The agent injects the untrusted
// media label on the provider request; it is not persisted into history.
func buildDeskMediaBlocks(attachments []deskAttachment) ([]llm.Block, error) {
	if len(attachments) > 4 {
		return nil, errors.New("too many attachments (max 4)")
	}
	blocks := make([]llm.Block, 0, len(attachments))
	total := 0
	for _, attachment := range attachments {
		decoded, err := base64.StdEncoding.DecodeString(attachment.DataB64)
		if err != nil {
			return nil, errors.New("attachment payload is not valid base64")
		}
		total += len(decoded)
		if total > llm.MaxMediaBytes {
			return nil, errors.New("attachment payloads are too large")
		}
		var block llm.Block
		switch {
		case strings.HasPrefix(attachment.MediaType, "image/"):
			block, err = llm.NewImageBlock(attachment.MediaType, decoded)
		default:
			block, err = llm.NewDocumentBlock(attachment.MediaType, decoded)
		}
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}
