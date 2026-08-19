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
	"sync"
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

func TestTaskScheduleUpdatePassesCanonicalInputToStore(t *testing.T) {
	schedules := &capturingTaskUpdateStore{job: schedule.Job{
		ID: "job-normalize", Name: "Old", Cron: "0 8 * * *", Prompt: "Old prompt", Enabled: true,
	}}
	handler, security := newTaskMutationTestHandler(t, schedules, NewEventHub(4), time.Second)
	req := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:8422/api/v1/desk/tasks/schedules/job-normalize",
		strings.NewReader(`{"name":"  Edited  ","cron":" 0 9 * * * ","prompt":"  Changed  ","deliver":" telegram:901 ","profile":"reviewer","enabled":false}`))
	req = req.WithContext(context.Background())
	req.Host = "127.0.0.1:8422"
	req.Header.Set("X-Waffle-Desk-Token", security.Token())
	req.Header.Set("Idempotency-Key", "canonical-update")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	want := schedule.Update{
		Name: "Edited", Cron: "0 9 * * *", Prompt: "Changed", Deliver: "telegram:901", Profile: "reviewer",
		Enabled: false,
	}
	if schedules.input != want {
		t.Fatalf("store input = %+v, want canonical %+v", schedules.input, want)
	}
}

func TestTaskScheduleLateCancellationReplaysCommittedUpdate(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	events := NewEventHub(4)
	requestContext, cancel := context.WithCancel(context.Background())
	schedules := &cancelAfterTaskUpdateStore{
		cancel: cancel,
		job: schedule.Job{
			ID: "job-cancel", Name: "Old", Cron: "0 8 * * *", Prompt: "Old prompt", Enabled: true,
		},
	}
	mux := http.NewServeMux()
	RegisterTaskRoutes(mux, TaskRouteConfig{
		Operations: &Operations{
			Jobs: taskJobReader{}, Runs: taskRunReader{},
			Sessions: taskSessionReader{}, Usage: taskUsageReader{},
		},
		Schedules: schedules, Security: security,
		Idempotency: NewIdempotencyStore(nil, 4, time.Minute),
		Events:      events,
	})
	body := `{"name":"Edited","cron":"0 9 * * *","prompt":"Changed","deliver":"","profile":"","enabled":true}`
	request := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost,
			"http://127.0.0.1:8422/api/v1/desk/tasks/schedules/job-cancel", strings.NewReader(body))
		req = req.WithContext(ctx)
		req.Host = "127.0.0.1:8422"
		req.Header.Set("X-Waffle-Desk-Token", security.Token())
		req.Header.Set("Idempotency-Key", "cancel-after-commit")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	first := request(requestContext)
	replay := request(context.Background())
	if first.Code != http.StatusOK || replay.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d: %s / %s", first.Code, replay.Code, first.Body.String(), replay.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("replay differs: %q / %q", first.Body.Bytes(), replay.Body.Bytes())
	}
	if schedules.updates != 1 {
		t.Fatalf("updates = %d, want one committed update", schedules.updates)
	}
	if events.Cursor() != 1 {
		t.Fatalf("event cursor = %d, want one publication", events.Cursor())
	}
}

func TestTaskMutationRejectsCancellationBeforeAdmission(t *testing.T) {
	schedules := newControlledTaskUpdateStore()
	events := NewEventHub(4)
	handler, security := newTaskMutationTestHandler(t, schedules, events, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := serveTaskUpdate(handler, security, ctx, "cancel-before-admission")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if gets, updates := schedules.counts(); gets != 0 || updates != 0 {
		t.Fatalf("store calls = get:%d update:%d, want zero", gets, updates)
	}
	if events.Cursor() != 0 {
		t.Fatal("pre-admission cancellation published an event")
	}
}

func TestTaskMutationCompletionPreservesInheritedDeadlineAndHardBound(t *testing.T) {
	tests := []struct {
		name           string
		requestTimeout time.Duration
		hardTimeout    time.Duration
		wantMaximum    time.Duration
	}{
		{name: "inherited deadline", requestTimeout: 35 * time.Millisecond, hardTimeout: time.Second, wantMaximum: 200 * time.Millisecond},
		{name: "hard completion bound", hardTimeout: 40 * time.Millisecond, wantMaximum: 200 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schedules := newDeadlineTaskUpdateStore()
			events := NewEventHub(4)
			handler, security := newTaskMutationTestHandler(t, schedules, events, test.hardTimeout)
			ctx := context.Background()
			var cancel context.CancelFunc = func() {}
			if test.requestTimeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, test.requestTimeout)
			}
			defer cancel()
			requestDeadline, inherited := ctx.Deadline()
			started := time.Now()

			rec := serveTaskUpdate(handler, security, ctx, "bounded-"+test.name)
			elapsed := time.Since(started)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
			}
			if elapsed > test.wantMaximum {
				t.Fatalf("mutation took %v, want at most %v", elapsed, test.wantMaximum)
			}
			seen := <-schedules.deadline
			if seen.IsZero() {
				t.Fatal("store context had no completion deadline")
			}
			if inherited && !seen.Equal(requestDeadline) {
				t.Fatalf("store deadline = %v, want inherited %v", seen, requestDeadline)
			}
			if events.Cursor() != 0 {
				t.Fatal("timed-out mutation published an event")
			}
		})
	}
}

func TestTaskMutationCanceledReplayWaiterDoesNotCancelAdmittedMutation(t *testing.T) {
	schedules := newControlledTaskUpdateStore()
	events := NewEventHub(4)
	handler, security := newTaskMutationTestHandler(t, schedules, events, time.Second)

	ownerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ownerDone <- serveTaskUpdate(handler, security, context.Background(), "join-in-flight")
	}()
	<-schedules.started

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		waiterDone <- serveTaskUpdate(handler, security, waiterCtx, "join-in-flight")
	}()
	time.AfterFunc(20*time.Millisecond, cancelWaiter)
	select {
	case waiter := <-waiterDone:
		if waiter.Code != http.StatusServiceUnavailable {
			t.Fatalf("waiter status = %d, want 503: %s", waiter.Code, waiter.Body.String())
		}
	case <-time.After(200 * time.Millisecond):
		close(schedules.release)
		<-ownerDone
		t.Fatal("canceled replay waiter remained blocked")
	}
	select {
	case owner := <-ownerDone:
		t.Fatalf("waiter cancellation ended owner early: %d %s", owner.Code, owner.Body.String())
	default:
	}

	close(schedules.release)
	owner := <-ownerDone
	replay := serveTaskUpdate(handler, security, context.Background(), "join-in-flight")
	if owner.Code != http.StatusOK || replay.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d: %s / %s", owner.Code, replay.Code, owner.Body.String(), replay.Body.String())
	}
	if !bytes.Equal(owner.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("completed replay differs: %q / %q", owner.Body.Bytes(), replay.Body.Bytes())
	}
	if _, updates := schedules.counts(); updates != 1 {
		t.Fatalf("updates = %d, want one", updates)
	}
	if events.Cursor() != 1 {
		t.Fatalf("event cursor = %d, want one", events.Cursor())
	}
}

func TestTaskMutationCachesUncooperativeSuccessOnlyAfterTermination(t *testing.T) {
	schedules := newLateTaskUpdateStore()
	events := NewEventHub(4)
	handler, security := newTaskMutationTestHandler(t, schedules, events, 35*time.Millisecond)
	ownerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ownerDone <- serveTaskUpdate(handler, security, context.Background(), "late-store-result")
	}()
	<-schedules.started
	<-schedules.deadline
	select {
	case early := <-ownerDone:
		close(schedules.release)
		<-schedules.returned
		t.Fatalf("owner returned before store termination: %d %s", early.Code, early.Body.String())
	default:
	}

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelWaiter()
	waiter := serveTaskUpdate(handler, security, waiterCtx, "late-store-result")
	if waiter.Code != http.StatusServiceUnavailable || waiter.Body.String() != "mutation_unavailable\n" {
		t.Fatalf("in-flight waiter = %d %q, want request-scoped cancellation", waiter.Code, waiter.Body.String())
	}
	if events.Cursor() != 0 {
		t.Fatal("in-flight uncooperative store published an event")
	}

	close(schedules.release)
	<-schedules.returned
	owner := <-ownerDone
	if owner.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want authoritative durable success: %s", owner.Code, owner.Body.String())
	}
	replay := serveTaskUpdate(handler, security, context.Background(), "late-store-result")
	if replay.Code != owner.Code || !bytes.Equal(replay.Body.Bytes(), owner.Body.Bytes()) {
		t.Fatalf("replay = %d %q, want cached actual result %d %q", replay.Code, replay.Body.Bytes(), owner.Code, owner.Body.Bytes())
	}
	if events.Cursor() != 1 {
		t.Fatalf("durable success events = %d, want one", events.Cursor())
	}
	if schedules.updateCount() != 1 {
		t.Fatalf("updates = %d, want one", schedules.updateCount())
	}
}

func TestTaskScheduleCreateSuccessAfterDeadlineIsAuthoritativeAndCanonical(t *testing.T) {
	schedules := newDeadlineSuccessCreateStore()
	events := NewEventHub(4)
	handler, security := newTaskMutationTestHandler(t, schedules, events, 35*time.Millisecond)
	ownerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ownerDone <- serveTaskCreate(handler, security, context.Background(), "create-after-deadline")
	}()
	<-schedules.started
	<-schedules.deadline
	select {
	case early := <-ownerDone:
		close(schedules.release)
		t.Fatalf("create returned before store termination: %d %s", early.Code, early.Body.String())
	default:
	}
	close(schedules.release)

	owner := <-ownerDone
	if owner.Code != http.StatusCreated {
		t.Fatalf("owner status = %d, want authoritative 201: %s", owner.Code, owner.Body.String())
	}
	var response struct {
		Task TaskView `json:"task"`
	}
	if err := json.Unmarshal(owner.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Task.ID != schedules.job.ID || response.Task.Name != schedules.job.Name ||
		response.Task.Profile != schedules.job.Profile || response.Task.Retry.MaxAttempts != schedules.job.MaxAttempts ||
		!response.Task.Enabled {
		t.Fatalf("response task = %+v, want canonical durable job %+v", response.Task, schedules.job)
	}
	replay := serveTaskCreate(handler, security, context.Background(), "create-after-deadline")
	if replay.Code != owner.Code || !bytes.Equal(replay.Body.Bytes(), owner.Body.Bytes()) {
		t.Fatalf("replay = %d %q, want %d %q", replay.Code, replay.Body.Bytes(), owner.Code, owner.Body.Bytes())
	}
	adds, gets := schedules.counts()
	if adds != 1 || gets != 0 {
		t.Fatalf("store calls = add:%d get:%d, want one atomic create and no follow-up read", adds, gets)
	}
	if events.Cursor() != 1 {
		t.Fatalf("events = %d, want one", events.Cursor())
	}
}

func TestTaskMutationProductionStoreDeadlineLeavesScheduleUnchanged(t *testing.T) {
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
	job, err := schedules.Add(context.Background(), "Old", "0 8 * * *", "Old prompt", "")
	if err != nil {
		t.Fatal(err)
	}
	held, err := st.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	events := NewEventHub(4)
	handler, security := newTaskMutationTestHandler(t, schedules, events, 35*time.Millisecond)
	started := time.Now()

	rec := serveTaskUpdateID(handler, security, context.Background(), "production-deadline", job.ID)
	if rec.Code != http.StatusServiceUnavailable {
		_ = held.Rollback()
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		_ = held.Rollback()
		t.Fatalf("owner returned after %v", elapsed)
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	stored, err := schedules.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Old" || stored.Cron != "0 8 * * *" || stored.Prompt != "Old prompt" {
		t.Fatalf("deadline mutation changed production store: %+v", stored)
	}
	if events.Cursor() != 0 {
		t.Fatal("deadline mutation published an event")
	}
	replay := serveTaskUpdateID(handler, security, context.Background(), "production-deadline", job.ID)
	// A non-committed mutation is not cached as terminal: a same-key retry
	// re-runs and commits now that the production store lock is free (#469).
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d %q, want 200 re-run", replay.Code, replay.Body.String())
	}
	stored, err = schedules.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Edited" {
		t.Fatalf("retried mutation did not commit: %+v", stored)
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

func TestTaskScheduleUpdateRequiresObjectAndExplicitEnabled(t *testing.T) {
	tests := map[string]string{
		"null":            `null`,
		"array":           `[]`,
		"string":          `"schedule"`,
		"omitted enabled": `{"name":"Edited","cron":"0 9 * * *","prompt":"changed","deliver":"","profile":""}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			harness := newTaskRouteHarness(t)
			job, err := harness.schedules.Add(context.Background(), "Old", "0 8 * * *", "Old prompt", "")
			if err != nil {
				t.Fatal(err)
			}

			rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+job.ID,
				body, "strict-update-"+strings.ReplaceAll(name, " ", "-"), true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			assertTaskError(t, rec.Body.Bytes(), "invalid_request")
			stored, err := harness.schedules.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != "Old" || stored.Cron != "0 8 * * *" ||
				stored.Prompt != "Old prompt" || !stored.Enabled {
				t.Fatalf("rejected request mutated schedule: %+v", stored)
			}
			if harness.events.Cursor() != 0 {
				t.Fatal("rejected request published an event")
			}
		})
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
		"name": "Renamed", "cron": view.Cron, "prompt": "",
		"deliver": "", "profile": view.Profile, "enabled": view.Enabled,
		"field_intents": map[string]any{
			"prompt":  map[string]any{"action": "preserve"},
			"deliver": map[string]any{"action": "preserve"},
		},
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
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("public response exposed preserved exact value: %s", rec.Body.String())
	}
	subscription, _ := harness.events.Subscribe(0)
	t.Cleanup(func() { harness.events.Unsubscribe(subscription) })
	event := <-subscription
	if bytes.Contains(event.Data, []byte(secret)) {
		t.Fatalf("event exposed preserved exact value: %s", event.Data)
	}
}

func TestTaskScheduleRedactedFieldIntentsReplaceAndClear(t *testing.T) {
	tests := []struct {
		name        string
		intents     map[string]any
		wantPrompt  string
		wantDeliver string
	}{
		{
			name: "replace",
			intents: map[string]any{
				"prompt":  map[string]any{"action": "replace", "value": "New prompt"},
				"deliver": map[string]any{"action": "replace", "value": "telegram:902"},
			},
			wantPrompt: "New prompt", wantDeliver: "telegram:902",
		},
		{
			name: "clear optional delivery",
			intents: map[string]any{
				"prompt":  map[string]any{"action": "preserve"},
				"deliver": map[string]any{"action": "clear"},
			},
			wantPrompt: "AGE-SECRET-KEY-original-secret", wantDeliver: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newTaskRouteHarness(t)
			const secret = "AGE-SECRET-KEY-original-secret"
			job, err := harness.schedules.Add(context.Background(), "Sensitive", "0 8 * * *", secret, "telegram:"+secret)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{
				"name": "Sensitive", "cron": "0 8 * * *", "prompt": "",
				"deliver": "", "profile": "", "enabled": true,
				"field_intents": test.intents,
			})
			if err != nil {
				t.Fatal(err)
			}

			rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+job.ID,
				string(body), "redacted-"+strings.ReplaceAll(test.name, " ", "-"), true)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			stored, err := harness.schedules.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Prompt != test.wantPrompt || stored.Deliver != test.wantDeliver {
				t.Fatalf("stored = %+v, want prompt %q deliver %q", stored, test.wantPrompt, test.wantDeliver)
			}
			if strings.Contains(stored.Prompt, "[redacted]") || strings.Contains(stored.Deliver, "[redacted]") {
				t.Fatalf("stored schedule contains display placeholder: %+v", stored)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("public response exposed exact secret: %s", rec.Body.String())
			}
			subscription, _ := harness.events.Subscribe(0)
			t.Cleanup(func() { harness.events.Unsubscribe(subscription) })
			event := <-subscription
			if bytes.Contains(event.Data, []byte(secret)) || bytes.Contains(event.Data, []byte("New prompt")) {
				t.Fatalf("event exposed editable content: %s", event.Data)
			}
		})
	}
}

func TestTaskScheduleRedactedFieldsRejectMissingOrPlaceholderIntent(t *testing.T) {
	tests := []struct {
		name    string
		intents map[string]any
	}{
		{
			name: "missing intent",
			intents: map[string]any{
				"prompt": map[string]any{"action": "preserve"},
			},
		},
		{
			name: "replace with display placeholder",
			intents: map[string]any{
				"prompt":  map[string]any{"action": "replace", "value": "[redacted]"},
				"deliver": map[string]any{"action": "preserve"},
			},
		},
		{
			name: "replace with derived placeholder",
			intents: map[string]any{
				"prompt":  map[string]any{"action": "preserve"},
				"deliver": map[string]any{"action": "replace", "value": "telegram:[redacted]-changed"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newTaskRouteHarness(t)
			const secret = "AGE-SECRET-KEY-original-secret"
			job, err := harness.schedules.Add(context.Background(), "Sensitive", "0 8 * * *", secret, "telegram:"+secret)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{
				"name": "Renamed", "cron": "0 8 * * *", "prompt": "",
				"deliver": "", "profile": "", "enabled": true,
				"field_intents": test.intents,
			})
			if err != nil {
				t.Fatal(err)
			}

			rec := harness.request(http.MethodPost, "/api/v1/desk/tasks/schedules/"+job.ID,
				string(body), "invalid-intent-"+strings.ReplaceAll(test.name, " ", "-"), true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			assertTaskError(t, rec.Body.Bytes(), "invalid_request")
			stored, err := harness.schedules.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != "Sensitive" || stored.Prompt != secret || stored.Deliver != "telegram:"+secret {
				t.Fatalf("invalid intent mutated schedule: %+v", stored)
			}
			if harness.events.Cursor() != 0 {
				t.Fatal("invalid intent published an event")
			}
		})
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

type capturingTaskUpdateStore struct {
	job   schedule.Job
	input schedule.Update
}

func (s *capturingTaskUpdateStore) AddWithProfile(context.Context, string, string, string, string, string) (*schedule.Job, error) {
	return nil, errors.New("unexpected create")
}

func (s *capturingTaskUpdateStore) Get(context.Context, string) (*schedule.Job, error) {
	job := s.job
	return &job, nil
}

func (s *capturingTaskUpdateStore) Update(_ context.Context, _ string, input schedule.Update) (*schedule.Job, error) {
	s.input = input
	s.job.Name = input.Name
	s.job.Cron = input.Cron
	s.job.Prompt = input.Prompt
	s.job.Deliver = input.Deliver
	s.job.Profile = input.Profile
	s.job.Enabled = input.Enabled
	job := s.job
	return &job, nil
}

type cancelAfterTaskUpdateStore struct {
	cancel  context.CancelFunc
	job     schedule.Job
	updates int
}

func newTaskMutationTestHandler(
	t *testing.T,
	schedules TaskScheduleStore,
	events *EventHub,
	timeout time.Duration,
) (http.Handler, *Security) {
	t.Helper()
	security := mustSecurity(t, "127.0.0.1:8422")
	mux := http.NewServeMux()
	RegisterTaskRoutes(mux, TaskRouteConfig{
		Operations: &Operations{
			Jobs: taskJobReader{}, Runs: taskRunReader{},
			Sessions: taskSessionReader{}, Usage: taskUsageReader{},
		},
		Schedules: schedules, Security: security,
		Idempotency:     NewIdempotencyStore(nil, 8, time.Minute),
		Events:          events,
		MutationTimeout: timeout,
	})
	return mux, security
}

func serveTaskUpdate(
	handler http.Handler,
	security *Security,
	ctx context.Context,
	key string,
) *httptest.ResponseRecorder {
	return serveTaskUpdateID(handler, security, ctx, key, "job-control")
}

func serveTaskUpdateID(
	handler http.Handler,
	security *Security,
	ctx context.Context,
	key string,
	jobID string,
) *httptest.ResponseRecorder {
	body := `{"name":"Edited","cron":"0 9 * * *","prompt":"Changed","deliver":"","profile":"","enabled":true}`
	req := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:8422/api/v1/desk/tasks/schedules/"+jobID, strings.NewReader(body))
	req = req.WithContext(ctx)
	req.Host = "127.0.0.1:8422"
	req.Header.Set("X-Waffle-Desk-Token", security.Token())
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func serveTaskCreate(
	handler http.Handler,
	security *Security,
	ctx context.Context,
	key string,
) *httptest.ResponseRecorder {
	body := `{"name":"Morning brief","cron":"0 9 * * *","prompt":"Summarize","deliver":"","profile":"researcher"}`
	req := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:8422/api/v1/desk/tasks/schedules", strings.NewReader(body))
	req = req.WithContext(ctx)
	req.Host = "127.0.0.1:8422"
	req.Header.Set("X-Waffle-Desk-Token", security.Token())
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

type controlledTaskUpdateStore struct {
	mu      sync.Mutex
	job     schedule.Job
	gets    int
	updates int
	started chan struct{}
	release chan struct{}
}

func newControlledTaskUpdateStore() *controlledTaskUpdateStore {
	return &controlledTaskUpdateStore{
		job: schedule.Job{
			ID: "job-control", Name: "Old", Cron: "0 8 * * *", Prompt: "Old prompt", Enabled: true,
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *controlledTaskUpdateStore) AddWithProfile(context.Context, string, string, string, string, string) (*schedule.Job, error) {
	return nil, errors.New("unexpected create")
}

func (s *controlledTaskUpdateStore) Get(context.Context, string) (*schedule.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	job := s.job
	return &job, nil
}

func (s *controlledTaskUpdateStore) Update(ctx context.Context, _ string, input schedule.Update) (*schedule.Job, error) {
	s.mu.Lock()
	s.updates++
	if s.updates == 1 {
		close(s.started)
	}
	s.mu.Unlock()
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.job.Name = input.Name
	s.job.Cron = input.Cron
	s.job.Prompt = input.Prompt
	s.job.Deliver = input.Deliver
	s.job.Profile = input.Profile
	s.job.Enabled = input.Enabled
	job := s.job
	return &job, nil
}

func (s *controlledTaskUpdateStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.updates
}

type deadlineTaskUpdateStore struct {
	job      schedule.Job
	deadline chan time.Time
}

type deadlineSuccessCreateStore struct {
	mu       sync.Mutex
	job      schedule.Job
	adds     int
	gets     int
	started  chan struct{}
	deadline chan struct{}
	release  chan struct{}
}

func newDeadlineSuccessCreateStore() *deadlineSuccessCreateStore {
	return &deadlineSuccessCreateStore{
		job: schedule.Job{
			ID: "job-canonical", Name: "Morning brief", Cron: "0 9 * * *",
			Prompt: "Summarize", Profile: "researcher", Enabled: true,
			CreatedAt:   time.Date(2026, time.July, 24, 13, 30, 0, 0, time.UTC),
			MaxAttempts: 4, BaseBackoff: 15 * time.Second,
			MaxBackoff: 10 * time.Minute, StallTimeout: 5 * time.Minute,
		},
		started: make(chan struct{}), deadline: make(chan struct{}), release: make(chan struct{}),
	}
}

func (s *deadlineSuccessCreateStore) AddWithProfile(ctx context.Context, _, _, _, _, _ string) (*schedule.Job, error) {
	s.mu.Lock()
	s.adds++
	s.mu.Unlock()
	close(s.started)
	<-ctx.Done()
	close(s.deadline)
	<-s.release
	job := s.job
	return &job, nil
}

func (s *deadlineSuccessCreateStore) Get(context.Context, string) (*schedule.Job, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	job := s.job
	return &job, nil
}

func (s *deadlineSuccessCreateStore) Update(context.Context, string, schedule.Update) (*schedule.Job, error) {
	return nil, errors.New("unexpected update")
}

func (s *deadlineSuccessCreateStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adds, s.gets
}

type lateTaskUpdateStore struct {
	mu       sync.Mutex
	job      schedule.Job
	updates  int
	started  chan struct{}
	deadline chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func newLateTaskUpdateStore() *lateTaskUpdateStore {
	return &lateTaskUpdateStore{
		job: schedule.Job{
			ID: "job-control", Name: "Old", Cron: "0 8 * * *", Prompt: "Old prompt", Enabled: true,
		},
		started:  make(chan struct{}),
		deadline: make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
}

func (s *lateTaskUpdateStore) AddWithProfile(context.Context, string, string, string, string, string) (*schedule.Job, error) {
	return nil, errors.New("unexpected create")
}

func (s *lateTaskUpdateStore) Get(context.Context, string) (*schedule.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.job
	return &job, nil
}

func (s *lateTaskUpdateStore) Update(ctx context.Context, _ string, input schedule.Update) (*schedule.Job, error) {
	s.mu.Lock()
	s.updates++
	s.mu.Unlock()
	close(s.started)
	<-ctx.Done()
	close(s.deadline)
	<-s.release
	s.mu.Lock()
	s.job.Name = input.Name
	s.job.Cron = input.Cron
	s.job.Prompt = input.Prompt
	s.job.Deliver = input.Deliver
	s.job.Profile = input.Profile
	s.job.Enabled = input.Enabled
	job := s.job
	s.mu.Unlock()
	close(s.returned)
	return &job, nil
}

func (s *lateTaskUpdateStore) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates
}

func newDeadlineTaskUpdateStore() *deadlineTaskUpdateStore {
	return &deadlineTaskUpdateStore{
		job: schedule.Job{
			ID: "job-control", Name: "Old", Cron: "0 8 * * *", Prompt: "Old prompt", Enabled: true,
		},
		deadline: make(chan time.Time, 1),
	}
}

func (s *deadlineTaskUpdateStore) AddWithProfile(context.Context, string, string, string, string, string) (*schedule.Job, error) {
	return nil, errors.New("unexpected create")
}

func (s *deadlineTaskUpdateStore) Get(context.Context, string) (*schedule.Job, error) {
	job := s.job
	return &job, nil
}

func (s *deadlineTaskUpdateStore) Update(ctx context.Context, _ string, _ schedule.Update) (*schedule.Job, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		s.deadline <- time.Time{}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
		return nil, errors.New("missing deadline")
	}
	s.deadline <- deadline
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *cancelAfterTaskUpdateStore) AddWithProfile(context.Context, string, string, string, string, string) (*schedule.Job, error) {
	return nil, errors.New("unexpected create")
}

func (s *cancelAfterTaskUpdateStore) Get(context.Context, string) (*schedule.Job, error) {
	job := s.job
	return &job, nil
}

func (s *cancelAfterTaskUpdateStore) Update(_ context.Context, _ string, input schedule.Update) (*schedule.Job, error) {
	s.updates++
	s.job.Name = input.Name
	s.job.Cron = input.Cron
	s.job.Prompt = input.Prompt
	s.job.Deliver = input.Deliver
	s.job.Profile = input.Profile
	s.job.Enabled = input.Enabled
	s.cancel()
	job := s.job
	return &job, nil
}
