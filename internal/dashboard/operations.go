package dashboard

import (
	"context"
	"errors"
	"time"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

const (
	OperationsSectionRuns       = "runs"
	OperationsSectionJobs       = "jobs"
	OperationsSectionWorkspaces = "workspaces"
	OperationsSectionUsage      = "usage"

	OperationsSectionUnavailableCode    = "section_unavailable"
	OperationsSectionUnavailableMessage = "section unavailable"
)

var ErrOperationsDependencyUnavailable = errors.New("operations dependency unavailable")

type RunReader interface {
	Snapshot(context.Context) (observability.Snapshot, error)
}

type JobStore interface {
	List(context.Context) ([]schedule.Job, error)
}

type SessionStore interface {
	Get(context.Context, string) (*session.Session, error)
	Search(context.Context, string, int) ([]session.Hit, error)
	SearchSummaries(context.Context, string, int) ([]session.Hit, error)
}

type NotesSearcher interface {
	Search(context.Context, string, int) ([]memory.NoteHit, error)
}

type WorksetStore interface {
	Add(context.Context, string, string, string, string, bool) (*workset.Entry, error)
}

type UsageReader interface {
	List(context.Context, string) ([]usage.Row, error)
}

type WorkspaceManager interface {
	List(context.Context) ([]workspace.Workspace, error)
	Get(context.Context, string) (*workspace.Workspace, error)
	OpenWithProfile(context.Context, string, string) (*workspace.Workspace, *sandbox.Client, error)
	Idle(context.Context, string) error
	Resume(context.Context, string) (*workspace.Workspace, *sandbox.Client, error)
	InspectClose(context.Context, string) (*workspace.CloseReport, error)
	Close(context.Context, string, bool) (*workspace.CloseReport, error)
}

// WorkspaceCloseLifecycle is the narrower close coordination contract used by
// Waffle Desk. It linearizes preview acceptance with close transitions and
// reports whether this caller actually changed the workspace state.
type WorkspaceCloseLifecycle interface {
	InspectCloseGuarded(context.Context, string, func(*workspace.CloseReport) error) (*workspace.CloseReport, error)
	CloseTransition(context.Context, string, bool) (*workspace.CloseReport, bool, error)
}

var (
	_ RunReader               = (*observability.Service)(nil)
	_ JobStore                = (*schedule.Store)(nil)
	_ SessionStore            = (*session.Store)(nil)
	_ NotesSearcher           = (*memory.NotesIndex)(nil)
	_ WorksetStore            = (*workset.Store)(nil)
	_ UsageReader             = (*usage.Store)(nil)
	_ WorkspaceManager        = (*workspace.Manager)(nil)
	_ WorkspaceCloseLifecycle = (*workspace.Manager)(nil)
)

// SectionError is a sanitized public error for one independently readable
// section. Its internal cause remains available to errors.Is without being
// serialized.
type SectionError struct {
	Section string `json:"section"`
	Code    string `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (e *SectionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *SectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// OperationsSection holds canonical section data and, independently, a
// sanitized read failure.
type OperationsSection[T any] struct {
	Data  T             `json:"data"`
	Error *SectionError `json:"error,omitempty"`
}

// OperationsSnapshot is an application-service aggregate of listable
// operational state. Section-specific handlers remain free to define their own
// HTTP response views.
type OperationsSnapshot struct {
	Runs       OperationsSection[observability.Snapshot] `json:"runs"`
	Jobs       OperationsSection[[]schedule.Job]         `json:"jobs"`
	Workspaces OperationsSection[[]workspace.Workspace]  `json:"workspaces"`
	Usage      OperationsSection[[]usage.Row]            `json:"usage"`
}

// Operations owns the narrow domain seams shared by Desk section services.
type Operations struct {
	Runs       RunReader
	Jobs       JobStore
	Workspaces WorkspaceManager
	Sessions   SessionStore
	Notes      NotesSearcher
	Workset    WorksetStore
	Usage      UsageReader
	Previews   *PreviewStore
	Events     *EventHub
	Now        func() time.Time
}

// Snapshot reads each listable section independently so one dependency failure
// does not discard healthy state.
func (o *Operations) Snapshot(ctx context.Context) OperationsSnapshot {
	snapshot := OperationsSnapshot{
		Runs: OperationsSection[observability.Snapshot]{
			Data: emptyRunSnapshot(),
		},
		Jobs: OperationsSection[[]schedule.Job]{
			Data: make([]schedule.Job, 0),
		},
		Workspaces: OperationsSection[[]workspace.Workspace]{
			Data: make([]workspace.Workspace, 0),
		},
		Usage: OperationsSection[[]usage.Row]{
			Data: make([]usage.Row, 0),
		},
	}

	if o == nil || o.Runs == nil {
		snapshot.Runs.Error = newSectionError(OperationsSectionRuns, ErrOperationsDependencyUnavailable)
	} else if runs, err := o.Runs.Snapshot(ctx); err != nil {
		snapshot.Runs.Error = newSectionError(OperationsSectionRuns, err)
	} else {
		snapshot.Runs.Data = canonicalRunSnapshot(runs)
	}

	if o == nil || o.Jobs == nil {
		snapshot.Jobs.Error = newSectionError(OperationsSectionJobs, ErrOperationsDependencyUnavailable)
	} else if jobs, err := o.Jobs.List(ctx); err != nil {
		snapshot.Jobs.Error = newSectionError(OperationsSectionJobs, err)
	} else if jobs != nil {
		snapshot.Jobs.Data = jobs
	}

	if o == nil || o.Workspaces == nil {
		snapshot.Workspaces.Error = newSectionError(OperationsSectionWorkspaces, ErrOperationsDependencyUnavailable)
	} else if workspaces, err := o.Workspaces.List(ctx); err != nil {
		snapshot.Workspaces.Error = newSectionError(OperationsSectionWorkspaces, err)
	} else if workspaces != nil {
		snapshot.Workspaces.Data = workspaces
	}

	if o == nil || o.Usage == nil {
		snapshot.Usage.Error = newSectionError(OperationsSectionUsage, ErrOperationsDependencyUnavailable)
	} else if rows, err := o.Usage.List(ctx, ""); err != nil {
		snapshot.Usage.Error = newSectionError(OperationsSectionUsage, err)
	} else if rows != nil {
		snapshot.Usage.Data = rows
	}

	return snapshot
}

func newSectionError(section string, cause error) *SectionError {
	return &SectionError{
		Section: section,
		Code:    OperationsSectionUnavailableCode,
		Message: OperationsSectionUnavailableMessage,
		cause:   cause,
	}
}

func emptyRunSnapshot() observability.Snapshot {
	return observability.Snapshot{
		Active:     make([]observability.ActiveRun, 0),
		Recent:     make([]observability.RecentRun, 0),
		RetryQueue: make([]any, 0),
	}
}

func canonicalRunSnapshot(snapshot observability.Snapshot) observability.Snapshot {
	if snapshot.Active == nil {
		snapshot.Active = make([]observability.ActiveRun, 0)
	}
	if snapshot.Recent == nil {
		snapshot.Recent = make([]observability.RecentRun, 0)
	}
	if snapshot.RetryQueue == nil {
		snapshot.RetryQueue = make([]any, 0)
	}
	return snapshot
}
