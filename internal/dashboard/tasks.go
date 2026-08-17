package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	HumanCron      string     `json:"human_cron,omitempty"`
	NextRun        *time.Time `json:"next_run,omitempty"`
	RedactedFields []string   `json:"redacted_fields,omitempty"`
}

type TasksSnapshot struct {
	Filter         TaskFilter      `json:"filter"`
	Tasks          []TaskView      `json:"tasks"`
	AttentionCount int             `json:"attention_count"`
	Errors         []*SectionError `json:"errors"`
}

// ScheduleOptionsView is the credential-free choice set for the guided
// schedule editor: configured profiles and delivery destinations (#460).
type ScheduleOptionsView struct {
	Profiles     []string `json:"profiles"`
	Deliveries   []string `json:"deliveries"`
	DefaultModel string   `json:"default_model,omitempty"`
}

// SchedulePreviewRequest is one draft of a schedule the editor asks the
// server to validate and describe before it is committed (#460).
type SchedulePreviewRequest struct {
	Name    string `json:"name"`
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
	Deliver string `json:"deliver"`
	Profile string `json:"profile"`
}

// SchedulePreviewResponse is the validated human summary of a draft. Field
// errors carry the exact key so the editor can highlight the control.
type SchedulePreviewResponse struct {
	Human    string            `json:"human,omitempty"`
	NextRun  string            `json:"next_run,omitempty"`
	Timezone string            `json:"timezone"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// ScheduleOptions exposes the choice lists behind the guided editor.
type ScheduleOptions interface {
	Profiles() []string
	Deliveries() []string
}

// OperationsScheduleOptions adapts the shared operations/config dependencies.
func OperationsScheduleOptions(profiles func() []string, deliveries func() []string) ScheduleOptions {
	return scheduleOptionsFuncs{profiles: profiles, deliveries: deliveries}
}

type scheduleOptionsFuncs struct {
	profiles   func() []string
	deliveries func() []string
}

func (s scheduleOptionsFuncs) Profiles() []string {
	if s.profiles == nil {
		return nil
	}
	return s.profiles()
}

func (s scheduleOptionsFuncs) Deliveries() []string {
	if s.deliveries == nil {
		return nil
	}
	return s.deliveries()
}

type TasksService struct {
	operations *Operations
	options    ScheduleOptions
}

func NewTasksService(operations *Operations) *TasksService {
	return &TasksService{operations: operations}
}

// SetOptions wires the configured choice lists for the guided schedule
// editor. Additive: Tasks still works without them.
func (s *TasksService) SetOptions(options ScheduleOptions) {
	s.options = options
}

// Read shapes schedules and cron runs independently. One failed dependency
// contributes a sanitized section error without discarding healthy cards.
// Options returns the configured choices for the guided schedule editor.
func (s *TasksService) Options() ScheduleOptionsView {
	view := ScheduleOptionsView{Profiles: make([]string, 0), Deliveries: make([]string, 0)}
	if s.options != nil {
		view.Profiles = s.options.Profiles()
		view.Deliveries = s.options.Deliveries()
	}
	sort.Strings(view.Profiles)
	sort.Strings(view.Deliveries)
	return view
}

// Preview validates one schedule draft and returns the human summary plus the
// next run in the host timezone, with exact field errors for inline feedback.
// The cadence summary is computed even while the draft is incomplete so the
// guided editor can preview a schedule before the name/prompt are typed.
func (s *TasksService) Preview(ctx context.Context, request SchedulePreviewRequest) SchedulePreviewResponse {
	response := SchedulePreviewResponse{
		Timezone: time.Now().Format("MST (UTC-07:00)"),
		Errors:   make(map[string]string),
	}
	if strings.TrimSpace(request.Name) == "" {
		response.Errors["name"] = "Give the schedule a name."
	}
	if strings.TrimSpace(request.Prompt) == "" {
		response.Errors["prompt"] = "Describe what the schedule should run."
	}
	if err := schedule.ValidateCron(request.Cron); err != nil {
		response.Errors["cron"] = "That cron expression is not valid. Use the guided controls or the examples below."
		return response
	}
	if request.Deliver != "" {
		if _, _, ok := schedule.ParseTarget(request.Deliver); !ok {
			response.Errors["deliver"] = "That delivery target is not valid. Choose a configured destination."
		}
	}
	if len(response.Errors) == 0 || response.Errors["cron"] == "" {
		response.Human = schedule.DescribeCron(request.Cron)
		response.NextRun = schedule.NextRun(request.Cron, time.Now()).Format(time.RFC3339)
	}
	return response
}

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

	cronViews := make([]TaskView, 0)
	sessionIDs := make([]string, 0)
	for _, run := range runs.Active {
		if run.Source != "cron" {
			continue
		}
		view := activeTaskView(run, usageBySession)
		if view.SessionID != "" {
			sessionIDs = append(sessionIDs, view.SessionID)
		}
		cronViews = append(cronViews, view)
	}
	for _, run := range runs.Recent {
		if run.Source != "cron" {
			continue
		}
		view := recentTaskView(run, usageBySession)
		if view.SessionID != "" {
			sessionIDs = append(sessionIDs, view.SessionID)
		}
		cronViews = append(cronViews, view)
	}
	openSessions, openErr := s.sessionExistence(ctx, sessionIDs)
	if openErr != nil {
		appendTaskSectionError(&result, OperationsSectionSessions, openErr)
	}
	for i := range cronViews {
		if openSessions[cronViews[i].SessionID] {
			cronViews[i].OpenAtDesk = true
		}
		result.Tasks = append(result.Tasks, cronViews[i])
	}

	for _, task := range result.Tasks {
		if task.Attention {
			result.AttentionCount++
		}
	}
	result.Tasks = filterTaskViews(result.Tasks, filter)
	return result, nil
}

// sessionExistence resolves which cron session IDs can open at Desk. Prefer a
// single batched ExistIDs lookup when the session store supports it (#150).
func (s *TasksService) sessionExistence(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	if s.operations == nil || s.operations.Sessions == nil {
		return nil, ErrOperationsDependencyUnavailable
	}
	if batch, ok := s.operations.Sessions.(sessionExistenceReader); ok {
		return batch.ExistIDs(ctx, ids)
	}
	// Fallback for test doubles that only implement Get.
	out := make(map[string]bool, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		persisted, err := s.operations.Sessions.Get(ctx, id)
		switch {
		case err == nil && persisted != nil && persisted.ID == id:
			out[id] = true
		case errors.Is(err, session.ErrNotFound):
			continue
		case err != nil:
			return nil, err
		}
	}
	return out, nil
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
		// Operator-language summary for the card (#460); the raw cron stays
		// available for the advanced editor.
		HumanCron: schedule.DescribeCron(job.Cron),
		NextRun:   taskTimePointer(schedule.NextRun(job.Cron, time.Now())),
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
