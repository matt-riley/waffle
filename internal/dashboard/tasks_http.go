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
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/schedule"
)

const (
	taskMutationMaxBodyBytes   = 64 << 10
	defaultTaskMutationTimeout = 30 * time.Second

	TaskScheduleCreatedEvent = "task.schedule.created"
	TaskScheduleUpdatedEvent = "task.schedule.updated"
)

var errInvalidTaskFieldIntent = errors.New("invalid task field intent")

type TaskScheduleStore interface {
	// Implementations must honor ctx and return only after the storage
	// operation has terminated. In particular, a deadline error must not leave
	// work capable of committing after this method returns.
	AddWithProfile(ctx context.Context, name, spec, prompt, deliver, profile string) (*schedule.Job, error)
	Get(ctx context.Context, id string) (*schedule.Job, error)
	Update(ctx context.Context, id string, in schedule.Update) (*schedule.Job, error)
}

// TaskRouteConfig is an additive integration seam for the caller-owned Desk
// mux. It deliberately does not create security, idempotency, or event state.
type TaskRouteConfig struct {
	Operations  *Operations
	Schedules   TaskScheduleStore
	Security    *Security
	Idempotency *IdempotencyStore
	Events      *EventHub
	// MutationTimeout bounds admitted schedule mutations. Zero uses the
	// production default.
	MutationTimeout time.Duration
}

// RegisterTaskRoutes mounts only the exact Tasks endpoints. The shared router
// calls this with its process-scoped dependencies.
func RegisterTaskRoutes(mux *http.ServeMux, config TaskRouteConfig) {
	service := NewTasksService(config.Operations)
	mux.Handle("GET /api/v1/desk/tasks", newTasksReadHandler(service))
	if config.Schedules == nil || config.Security == nil || config.Idempotency == nil {
		return
	}
	events := config.Events
	if events == nil && config.Operations != nil {
		events = config.Operations.Events
	}
	mutation := func(next http.Handler) http.Handler {
		timeout := config.MutationTimeout
		if timeout <= 0 {
			timeout = defaultTaskMutationTimeout
		}
		protected := newTaskMutationHandler(
			config.Security,
			taskMutationExecutor{store: config.Idempotency, timeout: timeout},
			taskMutationMaxBodyBytes,
			next,
		)
		return preserveTaskResponseType(protected)
	}
	mux.Handle("POST /api/v1/desk/tasks/schedules", mutation(newTaskScheduleCreateHandler(config.Schedules, events)))
	mux.Handle("POST /api/v1/desk/tasks/schedules/{id}", mutation(newTaskScheduleUpdateHandler(config.Schedules, events)))
}

type taskMutationExecutor struct {
	store   *IdempotencyStore
	timeout time.Duration
}

func newTaskMutationHandler(
	security *Security,
	executor taskMutationExecutor,
	maxBodyBytes int64,
	next http.Handler,
) http.Handler {
	return security.RequireMutation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			http.Error(w, "mutation_unavailable", http.StatusServiceUnavailable)
			return
		}
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
		if r.Context().Err() != nil {
			http.Error(w, "mutation_unavailable", http.StatusServiceUnavailable)
			return
		}
		digest := sha256.Sum256(body)
		operation := r.Method + " " + r.URL.Path
		status, responseBody, doErr := executor.Do(
			r.Context(),
			r.Header.Get("Idempotency-Key"),
			operation,
			hex.EncodeToString(digest[:]),
			func(ctx context.Context) (int, []byte) {
				mutationRequest := r.Clone(ctx)
				mutationRequest.Body = io.NopCloser(bytes.NewReader(body))
				recorder := newResponseCapture()
				next.ServeHTTP(recorder, mutationRequest)
				return recorder.status, recorder.body.Bytes()
			},
		)
		if doErr != nil && status == 0 {
			http.Error(w, "mutation_unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(responseBody)
	}))
}

func (e taskMutationExecutor) Do(
	requestCtx context.Context,
	key, operation, requestDigest string,
	run func(context.Context) (status int, body []byte),
) (status int, body []byte, err error) {
	if err := requestCtx.Err(); err != nil {
		return 0, nil, err
	}

	store := e.store
	store.mu.Lock()
	store.pruneExpiredLocked()
	if entry, ok := store.entries[key]; ok {
		if entry.operation != operation || entry.digest != requestDigest {
			store.mu.Unlock()
			return http.StatusConflict, []byte("idempotency_conflict"), nil
		}
		if entry.ready == nil {
			status, body = entry.status, append([]byte(nil), entry.body...)
			store.mu.Unlock()
			return status, body, nil
		}
		ready := entry.ready
		store.mu.Unlock()
		return waitForTaskMutation(requestCtx, store, entry, ready)
	}
	if !store.makeSpaceLocked() {
		store.mu.Unlock()
		return http.StatusServiceUnavailable, []byte("idempotency_unavailable"), errIdempotencyCapacity
	}
	entry := &idempotencyEntry{
		operation: operation,
		digest:    requestDigest,
		ready:     make(chan struct{}),
	}
	store.entries[key] = entry
	ready := entry.ready
	store.mu.Unlock()

	completionCtx, cancel := taskMutationCompletionContext(requestCtx, e.timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			store.mu.Lock()
			if store.entries[key] == entry && entry.ready != nil {
				delete(store.entries, key)
				close(ready)
			}
			store.mu.Unlock()
			panic(recovered)
		}
	}()
	status, body = run(completionCtx)
	store.mu.Lock()
	if store.entries[key] == entry && entry.ready != nil {
		entry.status = status
		entry.body = append([]byte(nil), body...)
		entry.expiresAt = store.now().Add(store.ttl)
		entry.ready = nil
		close(ready)
	}
	store.mu.Unlock()
	return status, append([]byte(nil), body...), nil
}

func taskMutationCompletionContext(requestCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(requestCtx)
	hardDeadline := time.Now().Add(timeout)
	deadline := hardDeadline
	if inherited, ok := requestCtx.Deadline(); ok && inherited.Before(deadline) {
		deadline = inherited
	}
	return context.WithDeadline(base, deadline)
}

func waitForTaskMutation(
	ctx context.Context,
	store *IdempotencyStore,
	entry *idempotencyEntry,
	ready <-chan struct{},
) (status int, body []byte, err error) {
	select {
	case <-ready:
	case <-ctx.Done():
		select {
		case <-ready:
		default:
			return 0, nil, ctx.Err()
		}
	}
	store.mu.Lock()
	status, body = entry.status, append([]byte(nil), entry.body...)
	store.mu.Unlock()
	return status, body, nil
}

func newTasksReadHandler(service *TasksService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter, err := ParseTaskFilter(r.URL.Query()["filter"])
		if err != nil {
			writeTaskError(w, http.StatusBadRequest, "invalid_filter", "task filter is invalid")
			return
		}
		snapshot, err := service.Read(r.Context(), filter)
		if err != nil {
			writeTaskError(w, http.StatusBadRequest, "invalid_filter", "task filter is invalid")
			return
		}
		writeTaskJSON(w, http.StatusOK, snapshot)
	})
}

type createTaskScheduleRequest struct {
	Name    string `json:"name"`
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
	Deliver string `json:"deliver"`
	Profile string `json:"profile"`
}

type updateTaskScheduleRequest struct {
	Name         string                             `json:"name"`
	Cron         string                             `json:"cron"`
	Prompt       string                             `json:"prompt"`
	Deliver      string                             `json:"deliver"`
	Profile      string                             `json:"profile"`
	Enabled      *bool                              `json:"enabled"`
	FieldIntents map[string]redactedTaskFieldIntent `json:"field_intents,omitempty"`
}

type redactedTaskFieldIntent struct {
	Action string  `json:"action"`
	Value  *string `json:"value,omitempty"`
}

func newTaskScheduleCreateHandler(store TaskScheduleStore, events *EventHub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request createTaskScheduleRequest
		if !decodeTaskRequest(w, r, &request) {
			return
		}
		input, err := schedule.ValidateUpdate(schedule.Update{
			Name: request.Name, Cron: request.Cron, Prompt: request.Prompt,
			Deliver: request.Deliver, Profile: request.Profile, Enabled: true,
		})
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		job, err := store.AddWithProfile(r.Context(), input.Name, input.Cron, input.Prompt, input.Deliver, input.Profile)
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		if err := r.Context().Err(); err != nil {
			writeTaskStoreError(w, err)
			return
		}
		job, err = store.Get(r.Context(), job.ID)
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		if err := r.Context().Err(); err != nil {
			writeTaskStoreError(w, err)
			return
		}
		view := scheduleTaskView(*job)
		publishTaskScheduleEvent(events, TaskScheduleCreatedEvent, view)
		writeTaskJSON(w, http.StatusCreated, struct {
			Task TaskView `json:"task"`
		}{Task: view})
	})
}

func newTaskScheduleUpdateHandler(store TaskScheduleStore, events *EventHub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request updateTaskScheduleRequest
		if !decodeTaskRequest(w, r, &request) {
			return
		}
		if request.Enabled == nil {
			writeTaskError(w, http.StatusBadRequest, "invalid_request", "task request is invalid")
			return
		}
		current, err := store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		if err := r.Context().Err(); err != nil {
			writeTaskStoreError(w, err)
			return
		}
		input, err := resolveTaskScheduleUpdate(*current, request)
		if err != nil {
			writeTaskError(w, http.StatusBadRequest, "invalid_request", "task request is invalid")
			return
		}
		job, err := store.Update(r.Context(), r.PathValue("id"), input)
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		if err := r.Context().Err(); err != nil {
			writeTaskStoreError(w, err)
			return
		}
		view := scheduleTaskView(*job)
		publishTaskScheduleEvent(events, TaskScheduleUpdatedEvent, view)
		writeTaskJSON(w, http.StatusOK, struct {
			Task TaskView `json:"task"`
		}{Task: view})
	})
}

func resolveTaskScheduleUpdate(current schedule.Job, request updateTaskScheduleRequest) (schedule.Update, error) {
	input := schedule.Update{
		Name: request.Name, Cron: request.Cron, Prompt: request.Prompt,
		Deliver: request.Deliver, Profile: request.Profile, Enabled: *request.Enabled,
	}
	fields := map[string]struct {
		exact     string
		candidate *string
	}{
		"name":    {exact: current.Name, candidate: &input.Name},
		"cron":    {exact: current.Cron, candidate: &input.Cron},
		"prompt":  {exact: current.Prompt, candidate: &input.Prompt},
		"deliver": {exact: current.Deliver, candidate: &input.Deliver},
		"profile": {exact: current.Profile, candidate: &input.Profile},
	}
	for name := range request.FieldIntents {
		if _, ok := fields[name]; !ok {
			return schedule.Update{}, errInvalidTaskFieldIntent
		}
	}
	for name, field := range fields {
		safe := sanitizeDashboardString(field.exact)
		intent, supplied := request.FieldIntents[name]
		if safe == field.exact {
			if supplied {
				return schedule.Update{}, errInvalidTaskFieldIntent
			}
			continue
		}
		if !supplied {
			return schedule.Update{}, errInvalidTaskFieldIntent
		}
		switch intent.Action {
		case "preserve":
			if intent.Value != nil {
				return schedule.Update{}, errInvalidTaskFieldIntent
			}
			*field.candidate = field.exact
		case "replace":
			if intent.Value == nil || strings.Contains(*intent.Value, "[redacted]") {
				return schedule.Update{}, errInvalidTaskFieldIntent
			}
			*field.candidate = *intent.Value
		case "clear":
			if intent.Value != nil {
				return schedule.Update{}, errInvalidTaskFieldIntent
			}
			*field.candidate = ""
		default:
			return schedule.Update{}, errInvalidTaskFieldIntent
		}
	}
	return input, nil
}

func decodeTaskRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	var raw json.RawMessage
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&raw); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeTaskError(w, http.StatusBadRequest, "invalid_request", "task request is invalid")
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeTaskError(w, http.StatusBadRequest, "invalid_request", "task request is invalid")
		return false
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeTaskError(w, http.StatusBadRequest, "invalid_request", "task request is invalid")
		return false
	}
	return true
}

func writeTaskStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, schedule.ErrInvalidUpdate):
		writeTaskError(w, http.StatusUnprocessableEntity, "invalid_schedule", "schedule definition is invalid")
	case errors.Is(err, schedule.ErrJobNotFound):
		writeTaskError(w, http.StatusNotFound, "schedule_not_found", "schedule was not found")
	default:
		writeTaskError(w, http.StatusServiceUnavailable, "schedule_unavailable", "schedule could not be saved")
	}
}

func writeTaskError(w http.ResponseWriter, status int, code, message string) {
	writeTaskJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

func writeTaskJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func publishTaskScheduleEvent(events *EventHub, eventType string, view TaskView) {
	if events == nil {
		return
	}
	public := view
	public.Prompt = ""
	public.Deliver = ""
	data, err := json.Marshal(public)
	if err != nil {
		return
	}
	events.Publish(Event{
		Type:       eventType,
		Resource:   "schedule",
		ResourceID: public.ID,
		Data:       data,
	})
}

// NewMutationHandler intentionally replays only status and body. This additive
// adapter restores JSON Content-Type without changing that shared primitive.
func preserveTaskResponseType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := newResponseCapture()
		next.ServeHTTP(capture, r)
		if json.Valid(capture.body.Bytes()) {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(capture.status)
		_, _ = w.Write(capture.body.Bytes())
	})
}
