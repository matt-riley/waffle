package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/dashboard/ui"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/usage"
)

func TestAttentionClassification(t *testing.T) {
	tests := []struct {
		name string
		job  schedule.Job
		run  *TaskView
		want bool
	}{
		{
			name: "failed schedule",
			job:  schedule.Job{LastStatus: "failed: provider unavailable"},
			want: true,
		},
		{
			name: "stalled schedule",
			job:  schedule.Job{LastStatus: "Stalled", Attempt: 1, MaxAttempts: 3},
			want: true,
		},
		{
			name: "exhausted retry",
			job:  schedule.Job{LastStatus: "failed: no capacity", Attempt: 3, MaxAttempts: 3},
			want: true,
		},
		{
			name: "disabled schedule retains failure evidence",
			job:  schedule.Job{Enabled: false, LastStatus: "failed: denied"},
			want: true,
		},
		{
			name: "failed run",
			run:  &TaskView{Outcome: "failed"},
			want: true,
		},
		{
			name: "successful history",
			job:  schedule.Job{Enabled: true, LastStatus: "ok", Attempt: 1, MaxAttempts: 3},
			run:  &TaskView{Outcome: "ok"},
			want: false,
		},
		{
			name: "ordinary text containing failed is not attention",
			job:  schedule.Job{Enabled: true, LastStatus: "notified: failed search was expected"},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := TaskNeedsAttention(test.job, test.run); got != test.want {
				t.Fatalf("TaskNeedsAttention() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTasksShapesCanonicalScheduleAndCronRunCards(t *testing.T) {
	operations := &Operations{
		Jobs: taskJobReader{jobs: []schedule.Job{
			{
				ID: "job-1", Name: "Morning brief", Cron: "0 9 * * *",
				Prompt: "Summarize", Deliver: "telegram:900", Profile: "researcher",
				Enabled: true, LastStatus: "ok", Attempt: 1, MaxAttempts: 3,
			},
		}},
		Runs: taskRunReader{snapshot: observability.Snapshot{
			Active: []observability.ActiveRun{
				{ID: "run-active", SessionID: "session-live", Source: "cron", Phase: "agent", Profile: "researcher", ElapsedMS: 1250, InputTokens: 7, OutputTokens: 3},
				{ID: "run-chat", SessionID: "session-chat", Source: "chat", Phase: "agent"},
			},
			Recent: []observability.RecentRun{
				{ID: "run-recent", SessionID: "session-missing", Source: "cron", Phase: "done", Profile: "reviewer", Outcome: "ok", RuntimeMS: 2400, InputTokens: 11, OutputTokens: 5},
				{ID: "run-http", SessionID: "session-http", Source: "http", Outcome: "failed"},
			},
		}},
		Sessions: taskSessionReader{sessions: map[string]*session.Session{
			"session-live": {ID: "session-live", Title: "Persisted run"},
		}},
		Usage: taskUsageReader{rows: []usage.Row{
			{SessionID: "session-live", Period: "day", InputTokens: 50, OutputTokens: 20, Requests: 2},
			{SessionID: "session-live", Period: "hour", InputTokens: 12, OutputTokens: 4, Requests: 1},
		}},
	}

	snapshot, err := NewTasksService(operations).Read(context.Background(), TaskFilterAll)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snapshot.Tasks) != 3 {
		t.Fatalf("task count = %d, want 3: %+v", len(snapshot.Tasks), snapshot.Tasks)
	}
	if snapshot.Errors == nil || len(snapshot.Errors) != 0 {
		t.Fatalf("errors = %#v, want canonical empty slice", snapshot.Errors)
	}

	scheduleView := taskByID(t, snapshot.Tasks, "job-1")
	if scheduleView.Kind != TaskKindSchedule || scheduleView.Source != "schedule" ||
		scheduleView.Name != "Morning brief" || scheduleView.Profile != "researcher" ||
		scheduleView.Cron != "0 9 * * *" || scheduleView.Outcome != "ok" ||
		scheduleView.Retry.Attempt != 1 || scheduleView.Retry.MaxAttempts != 3 ||
		scheduleView.EvidenceLabel != "Last run succeeded" || scheduleView.OpenAtDesk {
		t.Fatalf("schedule view = %+v", scheduleView)
	}

	active := taskByID(t, snapshot.Tasks, "run-active")
	if active.Kind != TaskKindActive || active.Source != "cron" ||
		active.Phase != "agent" || active.Profile != "researcher" ||
		active.SessionID != "session-live" || active.ElapsedMS != 1250 ||
		active.Usage.InputTokens != 50 || active.Usage.OutputTokens != 20 ||
		!active.OpenAtDesk || active.EvidenceLabel != "Running now" {
		t.Fatalf("active view = %+v", active)
	}

	recent := taskByID(t, snapshot.Tasks, "run-recent")
	if recent.Kind != TaskKindRecent || recent.Source != "cron" ||
		recent.Phase != "done" || recent.Profile != "reviewer" ||
		recent.SessionID != "session-missing" || recent.RuntimeMS != 2400 ||
		recent.Usage.InputTokens != 11 || recent.Usage.OutputTokens != 5 ||
		recent.OpenAtDesk || recent.EvidenceLabel != "Completed successfully" {
		t.Fatalf("recent view = %+v", recent)
	}
}

func TestTasksFilters(t *testing.T) {
	operations := &Operations{
		Jobs: taskJobReader{jobs: []schedule.Job{
			{ID: "job-ok", Name: "OK", Enabled: true, LastStatus: "ok"},
			{ID: "job-attention", Name: "Broken", Enabled: false, LastStatus: "failed: canary"},
		}},
		Runs: taskRunReader{snapshot: observability.Snapshot{
			Active: []observability.ActiveRun{{ID: "run-active", Source: "cron"}},
			Recent: []observability.RecentRun{
				{ID: "run-complete", Source: "cron", Outcome: "ok"},
				{ID: "run-failed", Source: "cron", Outcome: "failed"},
			},
		}},
		Sessions: taskSessionReader{},
		Usage:    taskUsageReader{},
	}
	service := NewTasksService(operations)
	for _, test := range []struct {
		filter TaskFilter
		want   []string
	}{
		{filter: TaskFilterAll, want: []string{"job-ok", "job-attention", "run-active", "run-complete", "run-failed"}},
		{filter: TaskFilterActive, want: []string{"run-active"}},
		{filter: TaskFilterScheduled, want: []string{"job-ok", "job-attention"}},
		{filter: TaskFilterCompleted, want: []string{"run-complete", "run-failed"}},
		{filter: TaskFilterAttention, want: []string{"job-attention", "run-failed"}},
	} {
		t.Run(string(test.filter), func(t *testing.T) {
			snapshot, err := service.Read(context.Background(), test.filter)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(snapshot.Tasks))
			for _, task := range snapshot.Tasks {
				got = append(got, task.ID)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("IDs = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTasksKeepsHealthyCardsAndSanitizesPartialFailures(t *testing.T) {
	secret := errors.New("sqlite token=super-secret")
	operations := &Operations{
		Jobs:  taskJobReader{jobs: []schedule.Job{{ID: "job-ok", Name: "Healthy", Enabled: true}}},
		Runs:  taskRunReader{err: secret},
		Usage: taskUsageReader{err: secret},
		Sessions: taskSessionReader{
			errs: map[string]error{"session-one": secret},
		},
	}

	snapshot, err := NewTasksService(operations).Read(context.Background(), TaskFilterAll)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != "job-ok" {
		t.Fatalf("healthy tasks = %+v", snapshot.Tasks)
	}
	if len(snapshot.Errors) != 2 {
		t.Fatalf("errors = %+v, want runs and usage", snapshot.Errors)
	}
	public, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "super-secret") || strings.Contains(string(public), "sqlite") {
		t.Fatalf("public response leaked backend failure: %s", public)
	}
	for _, sectionErr := range snapshot.Errors {
		if sectionErr.Code != OperationsSectionUnavailableCode ||
			sectionErr.Message != OperationsSectionUnavailableMessage {
			t.Fatalf("unsanitized section error = %+v", sectionErr)
		}
	}
}

func TestTasksSanitizesScheduleFailureEvidence(t *testing.T) {
	const secret = "provider failed with token=super-secret"
	operations := &Operations{
		Jobs:     taskJobReader{jobs: []schedule.Job{{ID: "job-failed", LastStatus: "failed: " + secret}}},
		Runs:     taskRunReader{},
		Sessions: taskSessionReader{},
		Usage:    taskUsageReader{},
	}
	snapshot, err := NewTasksService(operations).Read(context.Background(), TaskFilterAll)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), secret) || strings.Contains(string(public), "super-secret") {
		t.Fatalf("task view leaked schedule failure details: %s", public)
	}
	if strings.Contains(string(public), "0001-01-01") {
		t.Fatalf("task view serialized absent timestamps: %s", public)
	}
	if got := snapshot.Tasks[0].Outcome; got != "failed" {
		t.Fatalf("outcome = %q, want canonical failed", got)
	}
	if got := snapshot.Tasks[0].Retry.Status; got != "failed" {
		t.Fatalf("retry status = %q, want canonical failed", got)
	}
}

func TestTasksMarksRedactedEditableFieldsWithoutExposingExactValues(t *testing.T) {
	const secret = "AGE-SECRET-KEY-original-secret"
	operations := &Operations{
		Jobs: taskJobReader{jobs: []schedule.Job{{
			ID:      "job-redacted",
			Name:    "Review " + secret,
			Cron:    "0 9 * * *",
			Prompt:  "Summarize " + secret,
			Deliver: "telegram:" + secret,
			Enabled: true,
		}}},
		Runs:     taskRunReader{},
		Sessions: taskSessionReader{},
		Usage:    taskUsageReader{},
	}

	snapshot, err := NewTasksService(operations).Read(context.Background(), TaskFilterAll)
	if err != nil {
		t.Fatal(err)
	}
	task := snapshot.Tasks[0]
	if got, want := strings.Join(task.RedactedFields, ","), "name,prompt,deliver"; got != want {
		t.Fatalf("redacted fields = %q, want %q", got, want)
	}
	public, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "original-secret") {
		t.Fatalf("task view exposed exact editable value: %s", public)
	}
}

func TestTasksKeepsRunWhenSessionHandoffCheckFails(t *testing.T) {
	secret := errors.New("session store token=super-secret")
	operations := &Operations{
		Jobs: taskJobReader{},
		Runs: taskRunReader{snapshot: observability.Snapshot{
			Recent: []observability.RecentRun{{
				ID: "run-one", SessionID: "session-one", Source: "cron", Outcome: "ok",
			}},
		}},
		Sessions: taskSessionReader{errs: map[string]error{"session-one": secret}},
		Usage:    taskUsageReader{},
	}
	snapshot, err := NewTasksService(operations).Read(context.Background(), TaskFilterAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].OpenAtDesk {
		t.Fatalf("run view = %+v", snapshot.Tasks)
	}
	if len(snapshot.Errors) != 1 || snapshot.Errors[0].Section != OperationsSectionSessions {
		t.Fatalf("errors = %+v, want one sessions section error", snapshot.Errors)
	}
	public, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "super-secret") || strings.Contains(string(public), "session store") {
		t.Fatalf("session failure leaked: %s", public)
	}
}

func TestTasksRejectsUnknownOrRepeatedFilter(t *testing.T) {
	for _, values := range [][]string{{"unknown"}, {"all", "active"}} {
		if _, err := ParseTaskFilter(values); !errors.Is(err, ErrInvalidTaskFilter) {
			t.Fatalf("ParseTaskFilter(%v) error = %v", values, err)
		}
	}
	for _, values := range [][]string{nil, {}, {"all"}, {"active"}, {"scheduled"}, {"completed"}, {"attention"}} {
		if _, err := ParseTaskFilter(values); err != nil {
			t.Fatalf("ParseTaskFilter(%v): %v", values, err)
		}
	}
}

func taskByID(t *testing.T, tasks []TaskView, id string) TaskView {
	t.Helper()
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %q not found in %+v", id, tasks)
	return TaskView{}
}

type taskJobReader struct {
	jobs []schedule.Job
	err  error
}

func (f taskJobReader) List(context.Context) ([]schedule.Job, error) {
	return f.jobs, f.err
}

type taskRunReader struct {
	snapshot observability.Snapshot
	err      error
}

func (f taskRunReader) Snapshot(context.Context) (observability.Snapshot, error) {
	return f.snapshot, f.err
}

type taskUsageReader struct {
	rows []usage.Row
	err  error
}

func (f taskUsageReader) List(context.Context, string) ([]usage.Row, error) {
	return f.rows, f.err
}

type taskSessionReader struct {
	sessions map[string]*session.Session
	errs     map[string]error
}

func (f taskSessionReader) Get(_ context.Context, id string) (*session.Session, error) {
	if err := f.errs[id]; err != nil {
		return nil, err
	}
	if value := f.sessions[id]; value != nil {
		return value, nil
	}
	return nil, session.ErrNotFound
}

func (f taskSessionReader) ExistIDs(_ context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := f.errs[id]; err != nil {
			return nil, err
		}
		if value := f.sessions[id]; value != nil {
			out[id] = true
		}
	}
	return out, nil
}

func (taskSessionReader) Search(context.Context, string, int) ([]session.Hit, error) {
	return nil, nil
}

func (taskSessionReader) SearchSummaries(context.Context, string, int) ([]session.Hit, error) {
	return nil, nil
}

func TestAttentionEvidenceFailedIgnoresUsageAndSessions(t *testing.T) {
	if attentionEvidenceFailed([]*SectionError{
		{Section: OperationsSectionUsage},
		{Section: OperationsSectionSessions},
	}) {
		t.Fatal("usage/session errors are not attention evidence")
	}
	if !attentionEvidenceFailed([]*SectionError{{Section: OperationsSectionJobs}}) {
		t.Fatal("jobs errors are attention evidence")
	}
	if !attentionEvidenceFailed([]*SectionError{{Section: OperationsSectionRuns}}) {
		t.Fatal("runs errors are attention evidence")
	}
}

func TestTasksAttentionLabel(t *testing.T) {
	cases := []struct {
		name      string
		count     int
		hasErrors bool
		want      string
	}{
		{name: "zero", count: 0, want: "No tasks need attention"},
		{name: "one", count: 1, want: "1 task needs attention"},
		{name: "many", count: 4, want: "4 tasks need attention"},
		{name: "partial failure", count: 2, hasErrors: true, want: "Attention unavailable"},
		{name: "total failure", count: 0, hasErrors: true, want: "Attention unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tasksAttentionLabel(tc.count, tc.hasErrors); got != tc.want {
				t.Fatalf("tasksAttentionLabel(%d, %v) = %q, want %q", tc.count, tc.hasErrors, got, tc.want)
			}
		})
	}
}

func TestTasksEmptyPresentationUsesTheFilterAndFailureStateMatrix(t *testing.T) {
	tests := []struct {
		name       string
		filter     TaskFilter
		wantTitle  string
		wantBody   string
		wantAction string
	}{
		{name: "all", filter: TaskFilterAll, wantTitle: "Nothing on Waffle's plate", wantBody: "Scheduled runs and completed work will appear here.", wantAction: "Start a conversation"},
		{name: "active", filter: TaskFilterActive, wantTitle: "No active runs", wantBody: "Nothing is running right now.", wantAction: "View all tasks"},
		{name: "scheduled", filter: TaskFilterScheduled, wantTitle: "No schedules yet", wantBody: "Create a schedule and Waffle can pick this up later.", wantAction: "View all tasks"},
		{name: "completed", filter: TaskFilterCompleted, wantTitle: "No completed runs", wantBody: "Finished runs will appear here.", wantAction: "View all tasks"},
		{name: "attention", filter: TaskFilterAttention, wantTitle: "Nothing needs attention", wantBody: "Waffle has no blocked or failed work to review.", wantAction: "View all tasks"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fragment := tasksFragment(TasksSnapshot{Filter: tc.filter})
			if fragment.EmptyState == nil {
				t.Fatal("empty state is nil")
			}
			if fragment.EmptyState.Title != tc.wantTitle || fragment.EmptyState.Body != tc.wantBody {
				t.Fatalf("copy = %q / %q, want %q / %q", fragment.EmptyState.Title, fragment.EmptyState.Body, tc.wantTitle, tc.wantBody)
			}
			if !fragmentHasAction(fragment.EmptyState, tc.wantAction) {
				t.Fatalf("missing action %q: %#v", tc.wantAction, fragment.EmptyState)
			}
		})
	}

	partial := tasksFragment(TasksSnapshot{
		Filter: TaskFilterAll,
		Tasks:  []TaskView{{ID: "healthy", Kind: TaskKindSchedule, Name: "Healthy", EvidenceLabel: "Scheduled"}},
		Errors: []*SectionError{{Section: OperationsSectionRuns}},
	})
	if partial.EmptyState != nil || partial.Status != "Some task evidence is temporarily unavailable." || len(partial.Items) != 1 {
		t.Fatalf("partial failure presentation = %#v", partial)
	}

	failure := tasksFragment(TasksSnapshot{Filter: TaskFilterAll, Errors: []*SectionError{{Section: OperationsSectionJobs}}})
	if failure.EmptyState == nil || !failure.EmptyState.NoArtwork || failure.EmptyState.Title != "Some task evidence is unavailable" || failure.EmptyState.Body != "Waffle could not check every task source. Try again before assuming the queue is empty." || !fragmentHasAction(failure.EmptyState, "Try again") {
		t.Fatalf("failure presentation = %#v", failure.EmptyState)
	}
}

func fragmentHasAction(state *ui.EmptyStateView, label string) bool {
	if state == nil {
		return false
	}
	for _, action := range []*ui.FragmentAction{state.PrimaryAction, state.SecondaryAction} {
		if action != nil && action.Label == label {
			return true
		}
	}
	return false
}

func TestTasksFragmentOwnsScheduleHierarchyAndAttentionLiveRegion(t *testing.T) {
	tests := []struct {
		name           string
		filter         TaskFilter
		triggerPrimary bool
		snapshot       TasksSnapshot
	}{
		{name: "all proven empty", filter: TaskFilterAll, triggerPrimary: true},
		{name: "active filtered empty", filter: TaskFilterActive},
		{name: "scheduled proven empty", filter: TaskFilterScheduled, triggerPrimary: true},
		{name: "completed filtered empty", filter: TaskFilterCompleted},
		{name: "attention filtered empty", filter: TaskFilterAttention},
		{name: "populated", filter: TaskFilterAll, snapshot: TasksSnapshot{Tasks: []TaskView{{ID: "healthy", Kind: TaskKindSchedule, Name: "Healthy", EvidenceLabel: "Scheduled"}}}},
		{name: "failure", filter: TaskFilterAll, snapshot: TasksSnapshot{Errors: []*SectionError{{Section: OperationsSectionJobs}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := tc.snapshot
			snapshot.Filter = tc.filter
			body := renderTasksFragment(t, tasksFragment(snapshot))
			start := strings.Index(body, `id="task-schedule-open"`)
			if start < 0 {
				t.Fatalf("fragment did not update the stable schedule trigger: %s", body)
			}
			end := strings.Index(body[start:], ">")
			if end < 0 {
				t.Fatalf("schedule trigger has no closing tag: %s", body[start:])
			}
			trigger := body[start : start+end+1]
			if got := strings.Contains(trigger, `class="action-primary"`); got != tc.triggerPrimary {
				t.Fatalf("schedule trigger primary = %t, want %t: %s", got, tc.triggerPrimary, trigger)
			}
			if !strings.Contains(trigger, `hx-swap-oob="outerHTML"`) {
				t.Fatalf("schedule trigger is not an OOB update: %s", trigger)
			}
			attentionStart := strings.Index(body, `id="tasks-attention-count"`)
			attentionEnd := strings.Index(body[attentionStart:], ">")
			if attentionStart < 0 || attentionEnd < 0 || !strings.Contains(body[attentionStart:attentionStart+attentionEnd], `aria-live="polite"`) {
				t.Fatalf("Tasks attention swap lost its opt-in live region: %s", body)
			}
		})
	}

	generic := renderTasksFragment(t, ui.FragmentView{
		ID:        "generic-fragment",
		TextSwaps: []ui.FragmentTextSwap{{ID: "generic-status", Class: "generic-status", Text: "Changed"}},
	})
	if strings.Contains(generic, `id="generic-status" class="generic-status" aria-live=`) {
		t.Fatalf("generic text swaps must not become live regions: %s", generic)
	}
}

func renderTasksFragment(t *testing.T, fragment ui.FragmentView) string {
	t.Helper()
	var rendered strings.Builder
	if err := ui.FragmentList(fragment).Render(context.Background(), &rendered); err != nil {
		t.Fatal(err)
	}
	return rendered.String()
}
