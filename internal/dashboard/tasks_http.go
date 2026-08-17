package dashboard

import (
	"bytes"
	"context"
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
	// work capable of committing after this method returns. A nil error is an
	// authoritative durable result even if ctx expires immediately afterward;
	// create and update must return the canonical committed Job.
	AddWithProfile(ctx context.Context, name, spec, prompt, deliver, profile string) (*schedule.Job, error)
	Get(ctx context.Context, id string) (*schedule.Job, error)
	Update(ctx context.Context, id string, in schedule.Update) (*schedule.Job, error)
}

// TaskRouteConfig is an additive integration seam for the caller-owned Desk
// mux. It deliberately does not create security, idempotency, or event state.
type TaskRouteConfig struct {
	Operations  *Operations
	Schedules   TaskScheduleStore
	Options     ScheduleOptions
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
	service.SetOptions(config.Options)
	mux.Handle("GET /api/v1/desk/tasks", negotiateFragments(newTasksReadHandler(service)))
	mux.Handle("GET /api/v1/desk/tasks/schedules/options", negotiateFragments(newScheduleOptionsHandler(service)))
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
		protected := NewDetachedMutationHandler(config.Security, config.Idempotency, taskMutationMaxBodyBytes, negotiateFragments(next), timeout)
		return preserveResponseType(protected)
	}
	mux.Handle("POST /api/v1/desk/tasks/schedules/preview", mutation(newSchedulePreviewHandler(service)))
	mux.Handle("POST /api/v1/desk/tasks/schedules", mutation(newTaskScheduleCreateHandler(config.Schedules, events)))
	mux.Handle("POST /api/v1/desk/tasks/schedules/{id}", mutation(newTaskScheduleUpdateHandler(config.Schedules, events)))
}

func newScheduleOptionsHandler(service *TasksService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, service.Options())
	})
}

func newSchedulePreviewHandler(service *TasksService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request SchedulePreviewRequest
		if !decodeTaskRequest(w, r, &request) {
			return
		}
		writeJSON(w, http.StatusOK, service.Preview(r.Context(), request))
	})
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
		writeJSON(w, http.StatusOK, snapshot)
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
		view := scheduleTaskView(*job)
		publishTaskScheduleEvent(events, TaskScheduleCreatedEvent, view)
		writeJSON(w, http.StatusCreated, TaskMutationResponse{Task: view})
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
		view := scheduleTaskView(*job)
		publishTaskScheduleEvent(events, TaskScheduleUpdatedEvent, view)
		writeJSON(w, http.StatusOK, TaskMutationResponse{Task: view})
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
	writeJSON(w, status, errorResponse{Code: code, Message: message})
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
