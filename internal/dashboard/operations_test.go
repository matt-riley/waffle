package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestOperationsSnapshotCanonicalizesEmptySections(t *testing.T) {
	operations := testOperations()

	snapshot := operations.Snapshot(context.Background())

	if snapshot.Runs.Error != nil || snapshot.Jobs.Error != nil ||
		snapshot.Workspaces.Error != nil || snapshot.Usage.Error != nil {
		t.Fatalf("healthy snapshot errors = %#v", snapshot)
	}
	if snapshot.Runs.Data.Active == nil ||
		snapshot.Runs.Data.Recent == nil ||
		snapshot.Runs.Data.RetryQueue == nil {
		t.Fatalf("run collections are not canonical: %#v", snapshot.Runs.Data)
	}
	if snapshot.Jobs.Data == nil || snapshot.Workspaces.Data == nil || snapshot.Usage.Data == nil {
		t.Fatalf("section collections are not canonical: %#v", snapshot)
	}
}

func TestOperationsSnapshotKeepsHealthySectionsAndSanitizesOneFailure(t *testing.T) {
	secret := errors.New("sqlite failed with token=super-secret")
	operations := testOperations()
	operations.Runs = fakeRunReader{snapshot: observability.Snapshot{
		Active:     []observability.ActiveRun{{ID: "run-1"}},
		Recent:     []observability.RecentRun{},
		RetryQueue: []any{},
	}}
	operations.Jobs = fakeJobStore{err: secret}
	operations.Workspaces = fakeWorkspaceManager{
		workspaces: []workspace.Workspace{{ID: "ws-1"}},
	}
	operations.Usage = fakeUsageReader{
		rows: []usage.Row{{SessionID: "session-1"}},
	}

	snapshot := operations.Snapshot(context.Background())

	if got := snapshot.Runs.Data.Active[0].ID; got != "run-1" {
		t.Fatalf("run ID = %q", got)
	}
	if got := snapshot.Workspaces.Data[0].ID; got != "ws-1" {
		t.Fatalf("workspace ID = %q", got)
	}
	if got := snapshot.Usage.Data[0].SessionID; got != "session-1" {
		t.Fatalf("usage session = %q", got)
	}
	if snapshot.Jobs.Data == nil || len(snapshot.Jobs.Data) != 0 {
		t.Fatalf("failed jobs data = %#v, want canonical empty slice", snapshot.Jobs.Data)
	}
	if snapshot.Jobs.Error == nil {
		t.Fatal("jobs error is nil")
	}
	if snapshot.Jobs.Error.Section != OperationsSectionJobs {
		t.Fatalf("error section = %q", snapshot.Jobs.Error.Section)
	}
	if snapshot.Jobs.Error.Code != OperationsSectionUnavailableCode {
		t.Fatalf("error code = %q", snapshot.Jobs.Error.Code)
	}
	if snapshot.Jobs.Error.Message != OperationsSectionUnavailableMessage {
		t.Fatalf("error message = %q", snapshot.Jobs.Error.Message)
	}
	if !errors.Is(snapshot.Jobs.Error, secret) {
		t.Fatal("typed section error does not retain its internal cause")
	}

	public, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(public), "super-secret") || strings.Contains(string(public), "sqlite failed") {
		t.Fatalf("public snapshot leaked backend error: %s", public)
	}
}

func TestOperationsSnapshotTreatsNilDependenciesAsUnavailable(t *testing.T) {
	snapshot := (&Operations{}).Snapshot(context.Background())

	sections := []*SectionError{
		snapshot.Runs.Error,
		snapshot.Jobs.Error,
		snapshot.Workspaces.Error,
		snapshot.Usage.Error,
	}
	for i, sectionErr := range sections {
		if sectionErr == nil {
			t.Fatalf("section %d error is nil", i)
		}
		if !errors.Is(sectionErr, ErrOperationsDependencyUnavailable) {
			t.Fatalf("section %d error = %v", i, sectionErr)
		}
	}
}

func testOperations() *Operations {
	return &Operations{
		Runs:       fakeRunReader{},
		Jobs:       fakeJobStore{},
		Workspaces: fakeWorkspaceManager{},
		Sessions:   fakeSessionStore{},
		Notes:      fakeNotesSearcher{},
		Workset:    fakeWorksetStore{},
		Usage:      fakeUsageReader{},
	}
}

type fakeRunReader struct {
	snapshot observability.Snapshot
	err      error
}

func (f fakeRunReader) Snapshot(context.Context) (observability.Snapshot, error) {
	return f.snapshot, f.err
}

type fakeJobStore struct {
	jobs []schedule.Job
	err  error
}

func (f fakeJobStore) List(context.Context) ([]schedule.Job, error) {
	return f.jobs, f.err
}

type fakeSessionStore struct{}

func (fakeSessionStore) Get(context.Context, string) (*session.Session, error) {
	return nil, nil
}

func (fakeSessionStore) Search(context.Context, string, int) ([]session.Hit, error) {
	return nil, nil
}

func (fakeSessionStore) SearchSummaries(context.Context, string, int) ([]session.Hit, error) {
	return nil, nil
}

type fakeNotesSearcher struct{}

func (fakeNotesSearcher) Search(context.Context, string, int) ([]memory.NoteHit, error) {
	return nil, nil
}

type fakeWorksetStore struct{}

func (fakeWorksetStore) Add(context.Context, string, string, string, string, bool) (*workset.Entry, error) {
	return nil, nil
}

type fakeUsageReader struct {
	rows []usage.Row
	err  error
}

func (f fakeUsageReader) List(context.Context, string) ([]usage.Row, error) {
	return f.rows, f.err
}

type fakeWorkspaceManager struct {
	workspaces []workspace.Workspace
	err        error
}

func (f fakeWorkspaceManager) List(context.Context) ([]workspace.Workspace, error) {
	return f.workspaces, f.err
}

func (fakeWorkspaceManager) Get(context.Context, string) (*workspace.Workspace, error) {
	return nil, nil
}

func (fakeWorkspaceManager) OpenWithProfile(context.Context, string, string) (*workspace.Workspace, *sandbox.Client, error) {
	return nil, nil, nil
}

func (fakeWorkspaceManager) Idle(context.Context, string) error {
	return nil
}

func (fakeWorkspaceManager) Resume(context.Context, string) (*workspace.Workspace, *sandbox.Client, error) {
	return nil, nil, nil
}

func (fakeWorkspaceManager) InspectClose(context.Context, string) (*workspace.CloseReport, error) {
	return nil, nil
}

func (fakeWorkspaceManager) Close(context.Context, string, bool) (*workspace.CloseReport, error) {
	return nil, nil
}
