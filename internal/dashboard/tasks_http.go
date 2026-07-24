package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/matt-riley/waffle/internal/schedule"
)

const (
	taskMutationMaxBodyBytes = 64 << 10

	TaskScheduleCreatedEvent = "task.schedule.created"
	TaskScheduleUpdatedEvent = "task.schedule.updated"
)

type TaskScheduleStore interface {
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
		protected := NewMutationHandler(config.Security, config.Idempotency, taskMutationMaxBodyBytes, next)
		return preserveTaskResponseType(protected)
	}
	mux.Handle("POST /api/v1/desk/tasks/schedules", mutation(newTaskScheduleCreateHandler(config.Schedules, events)))
	mux.Handle("POST /api/v1/desk/tasks/schedules/{id}", mutation(newTaskScheduleUpdateHandler(config.Schedules, events)))
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
		job, err = store.Get(r.Context(), job.ID)
		if err != nil {
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
		var input schedule.Update
		if !decodeTaskRequest(w, r, &input) {
			return
		}
		current, err := store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeTaskStoreError(w, err)
			return
		}
		restoreRedactedTaskScheduleFields(current, &input)
		job, err := store.Update(r.Context(), r.PathValue("id"), input)
		if err != nil {
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

func restoreRedactedTaskScheduleFields(current *schedule.Job, input *schedule.Update) {
	if current == nil || input == nil {
		return
	}
	restore := func(exact string, candidate *string) {
		if exact != sanitizeDashboardString(exact) && *candidate == sanitizeDashboardString(exact) {
			*candidate = exact
		}
	}
	restore(current.Name, &input.Name)
	restore(current.Cron, &input.Cron)
	restore(current.Prompt, &input.Prompt)
	restore(current.Deliver, &input.Deliver)
	restore(current.Profile, &input.Profile)
}

func decodeTaskRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
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
