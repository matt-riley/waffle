package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/usage"
)

const (
	OperationsSectionSessions = "sessions"

	TaskKindSchedule = "schedule"
	TaskKindActive   = "active"
	TaskKindRecent   = "recent"
)

type TaskFilter string

const (
	TaskFilterAll       TaskFilter = "all"
	TaskFilterActive    TaskFilter = "active"
	TaskFilterScheduled TaskFilter = "scheduled"
	TaskFilterCompleted TaskFilter = "completed"
	TaskFilterAttention TaskFilter = "attention"
)

var ErrInvalidTaskFilter = errors.New("invalid task filter")

// ParseTaskFilter accepts one optional filter value. An absent filter is the
// canonical all view; repeated values are rejected even when identical.
func ParseTaskFilter(values []string) (TaskFilter, error) {
	if len(values) == 0 {
		return TaskFilterAll, nil
	}
	if len(values) != 1 {
		return "", ErrInvalidTaskFilter
	}
	filter := TaskFilter(values[0])
	switch filter {
	case TaskFilterAll, TaskFilterActive, TaskFilterScheduled, TaskFilterCompleted, TaskFilterAttention:
		return filter, nil
	default:
		return "", ErrInvalidTaskFilter
	}
}

type TaskUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type TaskRetry struct {
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	NextRetry   *time.Time `json:"next_retry,omitempty"`
	Status      string     `json:"status,omitempty"`
}

// TaskView is the stable, sanitized public shape shared by the Tasks API,
// events, and browser client.
type TaskView struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Name           string     `json:"name,omitempty"`
	Source         string     `json:"source"`
	Phase          string     `json:"phase,omitempty"`
	Profile        string     `json:"profile,omitempty"`
	SessionID      string     `json:"session,omitempty"`
	ElapsedMS      int64      `json:"elapsed_ms,omitempty"`
	RuntimeMS      int64      `json:"runtime_ms,omitempty"`
	Usage          TaskUsage  `json:"usage"`
	Outcome        string     `json:"outcome,omitempty"`
	Retry          TaskRetry  `json:"retry"`
	EvidenceLabel  string     `json:"evidence_label"`
	Attention      bool       `json:"attention"`
	OpenAtDesk     bool       `json:"open_at_desk,omitempty"`
	Cron           string     `json:"cron,omitempty"`
	Prompt         string     `json:"prompt,omitempty"`
	Deliver        string     `json:"deliver,omitempty"`
	Enabled        bool       `json:"enabled,omitempty"`
	LastRun        *time.Time `json:"last_run,omitempty"`
	RedactedFields []string   `json:"redacted_fields,omitempty"`
}

type TasksSnapshot struct {
	Filter         TaskFilter      `json:"filter"`
	Tasks          []TaskView      `json:"tasks"`
	AttentionCount int             `json:"attention_count"`
	Errors         []*SectionError `json:"errors"`
}

type TasksService struct {
	operations *Operations
}

func NewTasksService(operations *Operations) *TasksService {
	return &TasksService{operations: operations}
}

// Read shapes schedules and cron runs independently. One failed dependency
// contributes a sanitized section error without discarding healthy cards.
func (s *TasksService) Read(ctx context.Context, filter TaskFilter) (TasksSnapshot, error) {
	if _, err := ParseTaskFilter([]string{string(filter)}); err != nil {
		return TasksSnapshot{}, err
	}
	result := TasksSnapshot{
		Filter: filter,
		Tasks:  make([]TaskView, 0),
		Errors: make([]*SectionError, 0),
	}
	operations := s.operations

	var jobs []schedule.Job
	if operations == nil || operations.Jobs == nil {
		result.Errors = append(result.Errors, newSectionError(OperationsSectionJobs, ErrOperationsDependencyUnavailable))
	} else if loaded, err := operations.Jobs.List(ctx); err != nil {
		result.Errors = append(result.Errors, newSectionError(OperationsSectionJobs, err))
	} else {
		jobs = loaded
	}

	var runs observability.Snapshot
	if operations == nil || operations.Runs == nil {
		result.Errors = append(result.Errors, newSectionError(OperationsSectionRuns, ErrOperationsDependencyUnavailable))
	} else if loaded, err := operations.Runs.Snapshot(ctx); err != nil {
		result.Errors = append(result.Errors, newSectionError(OperationsSectionRuns, err))
	} else {
		runs = canonicalRunSnapshot(loaded)
	}

	usageBySession := make(map[string]TaskUsage)
	if operations == nil || operations.Usage == nil {
		result.Errors = append(result.Errors, newSectionError(OperationsSectionUsage, ErrOperationsDependencyUnavailable))
	} else if rows, err := operations.Usage.List(ctx, ""); err != nil {
		result.Errors = append(result.Errors, newSectionError(OperationsSectionUsage, err))
	} else {
		usageBySession = canonicalTaskUsage(rows)
	}

	for _, job := range jobs {
		view := scheduleTaskView(job)
		result.Tasks = append(result.Tasks, view)
	}
	for _, run := range runs.Active {
		if run.Source != "cron" {
			continue
		}
		view := activeTaskView(run, usageBySession)
		s.setOpenAtDesk(ctx, &result, &view)
		result.Tasks = append(result.Tasks, view)
	}
	for _, run := range runs.Recent {
		if run.Source != "cron" {
			continue
		}
		view := recentTaskView(run, usageBySession)
		s.setOpenAtDesk(ctx, &result, &view)
		result.Tasks = append(result.Tasks, view)
	}

	for _, task := range result.Tasks {
		if task.Attention {
			result.AttentionCount++
		}
	}
	result.Tasks = filterTaskViews(result.Tasks, filter)
	return result, nil
}

func (s *TasksService) setOpenAtDesk(ctx context.Context, result *TasksSnapshot, view *TaskView) {
	if view.SessionID == "" {
		return
	}
	if s.operations == nil || s.operations.Sessions == nil {
		appendTaskSectionError(result, OperationsSectionSessions, ErrOperationsDependencyUnavailable)
		return
	}
	persisted, err := s.operations.Sessions.Get(ctx, view.SessionID)
	switch {
	case err == nil && persisted != nil && persisted.ID == view.SessionID:
		view.OpenAtDesk = true
	case errors.Is(err, session.ErrNotFound):
		return
	case err != nil:
		appendTaskSectionError(result, OperationsSectionSessions, err)
	}
}

func appendTaskSectionError(result *TasksSnapshot, section string, cause error) {
	for _, existing := range result.Errors {
		if existing.Section == section {
			return
		}
	}
	result.Errors = append(result.Errors, newSectionError(section, cause))
}

func canonicalTaskUsage(rows []usage.Row) map[string]TaskUsage {
	result := make(map[string]TaskUsage)
	for _, row := range rows {
		if row.SessionID == "" || row.Period != "day" {
			continue
		}
		if _, exists := result[row.SessionID]; exists {
			continue
		}
		result[row.SessionID] = TaskUsage{
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
		}
	}
	return result
}

func scheduleTaskView(job schedule.Job) TaskView {
	view := TaskView{
		ID:      sanitizeDashboardString(job.ID),
		Kind:    TaskKindSchedule,
		Name:    sanitizeDashboardString(job.Name),
		Source:  "schedule",
		Profile: sanitizeDashboardString(job.Profile),
		Outcome: canonicalTaskState(job.LastStatus),
		Retry: TaskRetry{
			Attempt:     job.Attempt,
			MaxAttempts: job.MaxAttempts,
			NextRetry:   taskTimePointer(job.NextRetry),
			Status:      canonicalTaskState(job.LastStatus),
		},
		Cron:    sanitizeDashboardString(job.Cron),
		Prompt:  sanitizeDashboardString(job.Prompt),
		Deliver: sanitizeDashboardString(job.Deliver),
		Enabled: job.Enabled,
		LastRun: taskTimePointer(job.LastRun),
	}
	view.RedactedFields = redactedTaskScheduleFields(job)
	view.Attention = TaskNeedsAttention(job, nil)
	view.EvidenceLabel = scheduleEvidenceLabel(job)
	return view
}

func redactedTaskScheduleFields(job schedule.Job) []string {
	fields := make([]string, 0, 5)
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "name", value: job.Name},
		{name: "cron", value: job.Cron},
		{name: "prompt", value: job.Prompt},
		{name: "deliver", value: job.Deliver},
		{name: "profile", value: job.Profile},
	} {
		if sanitizeDashboardString(field.value) != field.value {
			fields = append(fields, field.name)
		}
	}
	return fields
}

func activeTaskView(run observability.ActiveRun, usageBySession map[string]TaskUsage) TaskView {
	view := TaskView{
		ID:            sanitizeDashboardString(run.ID),
		Kind:          TaskKindActive,
		Source:        sanitizeDashboardString(run.Source),
		Phase:         sanitizeDashboardString(run.Phase),
		Profile:       sanitizeDashboardString(run.Profile),
		SessionID:     sanitizeDashboardString(run.SessionID),
		ElapsedMS:     run.ElapsedMS,
		Usage:         TaskUsage{InputTokens: run.InputTokens, OutputTokens: run.OutputTokens},
		EvidenceLabel: "Running now",
	}
	if aggregate, ok := usageBySession[run.SessionID]; ok {
		view.Usage = aggregate
	}
	view.Attention = TaskNeedsAttention(schedule.Job{}, &view)
	return view
}

func recentTaskView(run observability.RecentRun, usageBySession map[string]TaskUsage) TaskView {
	view := TaskView{
		ID:        sanitizeDashboardString(run.ID),
		Kind:      TaskKindRecent,
		Source:    sanitizeDashboardString(run.Source),
		Phase:     sanitizeDashboardString(run.Phase),
		Profile:   sanitizeDashboardString(run.Profile),
		SessionID: sanitizeDashboardString(run.SessionID),
		RuntimeMS: run.RuntimeMS,
		Usage:     TaskUsage{InputTokens: run.InputTokens, OutputTokens: run.OutputTokens},
		Outcome:   canonicalTaskState(run.Outcome),
	}
	if aggregate, ok := usageBySession[run.SessionID]; ok {
		view.Usage = aggregate
	}
	view.Attention = TaskNeedsAttention(schedule.Job{}, &view)
	if view.Attention {
		view.EvidenceLabel = "Run needs attention"
	} else if view.Outcome == "ok" {
		view.EvidenceLabel = "Completed successfully"
	} else {
		view.EvidenceLabel = "Run completed"
	}
	return view
}

func filterTaskViews(tasks []TaskView, filter TaskFilter) []TaskView {
	filtered := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		include := false
		switch filter {
		case TaskFilterAll:
			include = true
		case TaskFilterActive:
			include = task.Kind == TaskKindActive
		case TaskFilterScheduled:
			include = task.Kind == TaskKindSchedule
		case TaskFilterCompleted:
			include = task.Kind == TaskKindRecent
		case TaskFilterAttention:
			include = task.Attention
		}
		if include {
			filtered = append(filtered, task)
		}
	}
	return filtered
}

// TaskNeedsAttention recognizes only canonical scheduler/run states. It does
// not substring-match arbitrary status text.
func TaskNeedsAttention(job schedule.Job, run *TaskView) bool {
	if taskFailureState(job.LastStatus) {
		return true
	}
	if run != nil && taskFailureState(run.Outcome) {
		return true
	}
	return false
}

func taskFailureState(value string) bool {
	normalized := canonicalTaskState(value)
	switch normalized {
	case "failed", "error", "stalled":
		return true
	}
	return false
}

func canonicalTaskState(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch {
	case normalized == "":
		return ""
	case normalized == "ok", normalized == "success", normalized == "completed":
		return "ok"
	case normalized == "error":
		return "error"
	case normalized == "failed", strings.HasPrefix(normalized, "failed:"):
		return "failed"
	case normalized == "stalled":
		return "stalled"
	case normalized == "retrying", strings.HasPrefix(normalized, "retrying:"):
		return "retrying"
	case normalized == "cancelled", normalized == "canceled":
		return "cancelled"
	default:
		return "unknown"
	}
}

func taskTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	canonical := value.UTC()
	return &canonical
}

func scheduleEvidenceLabel(job schedule.Job) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(job.LastStatus), "stalled"):
		return "Last run stalled"
	case taskFailureState(job.LastStatus) && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts:
		return fmt.Sprintf("Retries exhausted after %d attempts", job.Attempt)
	case taskFailureState(job.LastStatus):
		return "Last run failed"
	case strings.EqualFold(strings.TrimSpace(job.LastStatus), "ok"):
		return "Last run succeeded"
	case job.LastRun.IsZero():
		return "Not run yet"
	default:
		return "Run evidence available"
	}
}
