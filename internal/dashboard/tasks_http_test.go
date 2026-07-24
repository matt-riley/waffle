package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/store"
)

func TestTasksRouteFiltersAndRejectsRepeatedOrUnknownFilter(t *testing.T) {
	harness := newTaskRouteHarness(t)
	_, err := harness.schedules.Add(context.Background(), "broken", "0 9 * * *", "brief", "")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := harness.schedules.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.store.DB.ExecContext(context.Background(),
		`UPDATE jobs SET last_status='failed: canary' WHERE id=?`, jobs[0].ID); err != nil {
		t.Fatal(err)
	}

	rec := harness.request(http.MethodGet, "/api/v1/desk/tasks?filter=attention", "", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var response TasksSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Filter != TaskFilterAttention || len(response.Tasks) != 1 || response.Tasks[0].ID != jobs[0].ID {
		t.Fatalf("response = %+v", response)
	}

	for _, path := range []string{
		"/api/v1/desk/tasks?filter=nope",
		"/api/v1/desk/tasks?filter=all&filter=active",
	} {
		rec := harness.request(http.MethodGet, path, "", "", false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "nope") {
			t.Fatalf("filter error echoed input: %s", rec.Body.String())
		}
		assertTaskError(t, rec.Body.Bytes(), "invalid_filter")
	}
}

func TestTaskScheduleCreateIsProtectedIdempotentAndPublishesOnce(t *testing.T) {
	harness := newTaskRouteHarness(t)
	body := `{"name":"Morning brief","cron":"0 9 * * *","prompt":"Summarize","deliver":"telegram:900","profile":"researcher"}`

	withoutToken := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules", body, "create-once", false)
	if withoutToken.Code != http.StatusForbidden {
		t.Fatalf("missing token status = %d, want 403", withoutToken.Code)
	}

	first := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules", body, "create-once", true)
	second := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules", body, "create-once", true)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d/%d: %s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("replay body differs: %q / %q", first.Body.Bytes(), second.Body.Bytes())
	}
	var created struct {
		Task TaskView `json:"task"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Task.Enabled || created.Task.Retry.MaxAttempts != 1 {
		t.Fatalf("create response is not the persisted canonical job: %+v", created.Task)
	}
	jobs, err := harness.schedules.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want one idempotent insert", len(jobs))
	}
	if got := harness.events.Cursor(); got != 1 {
		t.Fatalf("event cursor = %d, want one publication", got)
	}

	conflict := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules",
		`{"name":"Changed","cron":"0 9 * * *","prompt":"Summarize","deliver":"","profile":""}`,
		"create-once", true)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("altered replay status = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}
	if got := harness.events.Cursor(); got != 1 {
		t.Fatalf("conflict published event, cursor = %d", got)
	}

	pathConflict := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+jobs[0].ID,
		`{"name":"Changed","cron":"0 9 * * *","prompt":"Summarize","deliver":"","profile":"","enabled":true}`,
		"create-once", true)
	if pathConflict.Code != http.StatusConflict {
		t.Fatalf("altered path replay status = %d, want 409: %s", pathConflict.Code, pathConflict.Body.String())
	}
	if got := harness.events.Cursor(); got != 1 {
		t.Fatalf("path conflict published event, cursor = %d", got)
	}
}

func TestTaskScheduleUpdatePreservesStateAndPublishesCanonicalEvent(t *testing.T) {
	harness := newTaskRouteHarness(t)
	job, err := harness.schedules.Add(context.Background(), "Old", "0 8 * * *", "Old prompt", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.store.DB.ExecContext(context.Background(),
		`UPDATE jobs SET last_status='failed: provider token=event-secret' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	canary := "WAFFLE_AGE_IDENTITY=secret-value"
	body := `{"name":"Edited","cron":"30 10 * * 1-5","prompt":"` + canary + `","deliver":"telegram:901","profile":"reviewer","enabled":false}`
	rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+job.ID, body, "update-once", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	updated, err := harness.schedules.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Edited" || updated.Enabled || updated.Profile != "reviewer" {
		t.Fatalf("updated = %+v", updated)
	}

	subscription, resync := harness.events.Subscribe(0)
	if resync {
		t.Fatal("unexpected event resync")
	}
	t.Cleanup(func() { harness.events.Unsubscribe(subscription) })
	event := <-subscription
	if event.Type != TaskScheduleUpdatedEvent || event.Resource != "schedule" || event.ResourceID != job.ID {
		t.Fatalf("event = %+v", event)
	}
	if bytes.Contains(event.Data, []byte("secret-value")) ||
		bytes.Contains(event.Data, []byte("event-secret")) ||
		bytes.Contains(event.Data, []byte(canary)) {
		t.Fatalf("event leaked request data: %s", event.Data)
	}
	var public TaskView
	if err := json.Unmarshal(event.Data, &public); err != nil {
		t.Fatal(err)
	}
	if public.ID != job.ID || public.Name != "Edited" || public.Prompt != "" {
		t.Fatalf("canonical event view = %+v", public)
	}
}

func TestTaskScheduleInvalidUpdateDoesNotMutateOrPublish(t *testing.T) {
	harness := newTaskRouteHarness(t)
	job, err := harness.schedules.Add(context.Background(), "Old", "0 8 * * *", "Old prompt", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := harness.schedules.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}

	rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+job.ID,
		`{"name":"Edited","cron":"not cron","prompt":"changed","deliver":"","profile":"","enabled":true}`,
		"bad-update", true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	assertTaskError(t, rec.Body.Bytes(), "invalid_schedule")
	after, err := harness.schedules.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || after.Cron != before.Cron || after.Prompt != before.Prompt {
		t.Fatalf("invalid update mutated job: before=%+v after=%+v", before, after)
	}
	if harness.events.Cursor() != 0 {
		t.Fatal("invalid update published an event")
	}
}

func TestTaskScheduleUnrelatedEditPreservesExactRedactedFields(t *testing.T) {
	harness := newTaskRouteHarness(t)
	const secret = "AGE-SECRET-KEY-original-secret"
	job, err := harness.schedules.Add(context.Background(), "Sensitive", "0 8 * * *", secret, "telegram:"+secret)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewTasksService(&Operations{
		Jobs: harness.schedules, Runs: taskRunReader{},
		Sessions: taskSessionReader{}, Usage: taskUsageReader{},
	}).Read(context.Background(), TaskFilterAll)
	if err != nil {
		t.Fatal(err)
	}
	view := taskByID(t, snapshot.Tasks, job.ID)
	if view.Prompt == secret || view.Deliver == "telegram:"+secret {
		t.Fatalf("test fixture was not redacted: %+v", view)
	}
	body, err := json.Marshal(map[string]any{
		"name": "Renamed", "cron": view.Cron, "prompt": view.Prompt,
		"deliver": view.Deliver, "profile": view.Profile, "enabled": view.Enabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+job.ID, string(body), "safe-round-trip", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := harness.schedules.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Renamed" || stored.Prompt != secret || stored.Deliver != "telegram:"+secret {
		t.Fatalf("unrelated edit corrupted exact fields: %+v", stored)
	}
	for _, value := range []string{stored.Name, stored.Prompt, stored.Deliver} {
		if strings.Contains(value, "[redacted]") {
			t.Fatalf("stored redaction placeholder: %+v", stored)
		}
	}
}

func TestTaskScheduleRoutesRejectStrictJSONAndOversizedBodies(t *testing.T) {
	harness := newTaskRouteHarness(t)
	for _, body := range []string{
		`{"name":"x","cron":"0 9 * * *","prompt":"x","unknown":true}`,
		`{"name":"x","cron":"0 9 * * *","prompt":"x"} {}`,
	} {
		rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules", body, "strict-"+body, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("strict JSON status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		assertTaskError(t, rec.Body.Bytes(), "invalid_request")
	}

	oversized := strings.Repeat("x", int(taskMutationMaxBodyBytes)+1)
	rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules", oversized, "oversized", true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if harness.events.Cursor() != 0 {
		t.Fatal("rejected request published an event")
	}
}

func TestTaskScheduleMissingUpdateIsSanitizedAndDoesNotPublish(t *testing.T) {
	harness := newTaskRouteHarness(t)
	rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/job-missing",
		`{"name":"Edited","cron":"0 9 * * *","prompt":"changed","deliver":"","profile":"","enabled":true}`,
		"missing", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	assertTaskError(t, rec.Body.Bytes(), "schedule_not_found")
	if strings.Contains(rec.Body.String(), "job-missing") {
		t.Fatalf("not found response leaked resource input: %s", rec.Body.String())
	}
	if harness.events.Cursor() != 0 {
		t.Fatal("missing update published an event")
	}
}

func TestTaskScheduleStoreFailureIsSanitizedAndDoesNotPublish(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	events := NewEventHub(4)
	mux := http.NewServeMux()
	RegisterTaskRoutes(mux, TaskRouteConfig{
		Operations: &Operations{
			Jobs: taskJobReader{}, Runs: taskRunReader{},
			Sessions: taskSessionReader{}, Usage: taskUsageReader{},
		},
		Schedules:   failingTaskScheduleStore{err: errors.New("sqlite password=super-secret")},
		Security:    security,
		Idempotency: NewIdempotencyStore(nil, 4, time.Minute),
		Events:      events,
	})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/tasks/schedules",
		strings.NewReader(`{"name":"Valid","cron":"0 9 * * *","prompt":"Valid","deliver":"","profile":""}`))
	req.Host = "127.0.0.1:8422"
	req.Header.Set("X-Waffle-Desk-Token", security.Token())
	req.Header.Set("Idempotency-Key", "store-failure")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	assertTaskError(t, rec.Body.Bytes(), "schedule_unavailable")
	if strings.Contains(rec.Body.String(), "super-secret") || strings.Contains(rec.Body.String(), "sqlite") {
		t.Fatalf("response leaked store error: %s", rec.Body.String())
	}
	if events.Cursor() != 0 {
		t.Fatal("failed store mutation published an event")
	}
}

type taskRouteHarness struct {
	t         *testing.T
	store     *store.Store
	schedules *schedule.Store
	security  *Security
	events    *EventHub
	handler   http.Handler
}

func newTaskRouteHarness(t *testing.T) *taskRouteHarness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	schedules := schedule.NewStore(st)
	security := mustSecurity(t, "127.0.0.1:8422")
	events := NewEventHub(16)
	operations := &Operations{
		Jobs:     schedules,
		Runs:     taskRunReader{},
		Sessions: taskSessionReader{},
		Usage:    taskUsageReader{},
		Events:   events,
	}
	mux := http.NewServeMux()
	RegisterTaskRoutes(mux, TaskRouteConfig{
		Operations:  operations,
		Schedules:   schedules,
		Security:    security,
		Idempotency: NewIdempotencyStore(nil, 32, time.Minute),
		Events:      events,
	})
	return &taskRouteHarness{
		t: t, store: st, schedules: schedules, security: security,
		events: events, handler: mux,
	}
}

func (h *taskRouteHarness) request(method, path, body, key string, token bool) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8422"+path, strings.NewReader(body))
	req.Host = "127.0.0.1:8422"
	if token {
		req.Header.Set("X-Waffle-Desk-Token", h.security.Token())
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func assertTaskError(t *testing.T, body []byte, code string) {
	t.Helper()
	var response struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode error response %q: %v", body, err)
	}
	if response.Code != code || response.Message == "" {
		t.Fatalf("error = %+v, want code %q", response, code)
	}
}

type failingTaskScheduleStore struct {
	err error
}

func (f failingTaskScheduleStore) AddWithProfile(context.Context, string, string, string, string, string) (*schedule.Job, error) {
	return nil, f.err
}

func (f failingTaskScheduleStore) Get(context.Context, string) (*schedule.Job, error) {
	return nil, f.err
}

func (f failingTaskScheduleStore) Update(context.Context, string, schedule.Update) (*schedule.Job, error) {
	return nil, f.err
}
