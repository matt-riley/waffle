package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/dashboard"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skillinstall"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

var fixtureNow = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, err := os.MkdirTemp("", "waffle-dashboard-fixture-")
	if err != nil {
		fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	stateStore, err := store.Open(ctx, filepath.Join(root, "state", "waffle.db"))
	if err != nil {
		fatal(err)
	}
	defer func() { _ = stateStore.Close() }()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal(err)
	}
	defer func() { _ = listener.Close() }()

	entropy := &counterReader{}
	security, err := dashboard.NewSecurity(listener.Addr().String(), entropy)
	if err != nil {
		fatal(err)
	}
	hub := dashboard.NewEventHub(128)
	idempotency := dashboard.NewIdempotencyStore(func() time.Time { return fixtureNow }, 256, 5*time.Minute)
	previews := dashboard.NewPreviewStore(func() time.Time { return fixtureNow }, entropy)

	sessions := newFixtureSessions()
	workspaces := newFixtureWorkspaces()
	jobs := fixtureJobs{
		{
			ID:          "job-daily",
			Name:        "Daily review",
			Cron:        "0 9 * * 1-5",
			Prompt:      "Review the release queue",
			Enabled:     true,
			MaxAttempts: 1,
		},
	}
	operations := &dashboard.Operations{
		Runs: fixtureRuns{snapshot: observability.Snapshot{
			Active: []observability.ActiveRun{},
			Recent: []observability.RecentRun{
				{
					ID:        "run-attention",
					SessionID: "session-primary",
					Source:    "cron",
					Phase:     "complete",
					Outcome:   "failed",
					RuntimeMS: 1800,
				},
			},
			RetryQueue: []any{},
		}},
		Jobs:       &jobs,
		Workspaces: workspaces,
		Sessions:   sessions,
		Notes:      fixtureNotes{},
		Workset:    &fixtureWorkset{},
		Usage: fixtureUsage{rows: []usage.Row{
			{
				SessionID:    "session-primary",
				Period:       "day",
				PeriodStart:  "2026-07-24",
				InputTokens:  320,
				OutputTokens: 140,
			},
		}},
		Previews: previews,
		Events:   hub,
		Now:      func() time.Time { return fixtureNow },
	}

	memoryRoot := filepath.Join(root, "workspace")
	if err := os.MkdirAll(memoryRoot, 0o700); err != nil {
		fatal(err)
	}
	memoryWorkspace := memory.Workspace{Dir: memoryRoot, Agent: memory.DefaultAgent}

	providers := &fixtureProviders{listing: providerconfig.Listing{
		State:        "ready",
		DefaultModel: "primary",
		UtilityModel: "primary",
		Providers: map[string]providerconfig.ProviderSummary{
			"fixture": {Type: "openai", BaseURL: "http://127.0.0.1:11434/v1"},
		},
		Models: map[string]providerconfig.ModelSummary{
			"local":   {Provider: "fixture", Model: "local-model"},
			"primary": {Provider: "fixture", Model: "primary-model"},
		},
	}}
	skills := &fixtureSkills{items: []dashboard.CapabilitySkill{
		{Name: "review", Description: "Review changes", Active: true},
	}}
	capabilities := &dashboard.Capabilities{
		Providers: providers,
		Sessions:  sessions,
		Skills:    skills,
		Catalogue: fixtureCatalogue{},
	}

	chatClients := dashboard.NewChatClients(
		func(context.Context) (chat.Backend, error) {
			return &fixtureChatBackend{sessions: sessions, skills: skills}, nil
		},
		entropy,
	)
	chatClients.SetEventHub(hub)
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = chatClients.Shutdown(shutdown)
	}()

	obs := observability.New(stateStore, func() time.Time { return fixtureNow })
	obs.MarkSchedulerTick()

	mux := http.NewServeMux()
	dashboard.RegisterRoutes(mux, dashboard.APIConfig{
		Observability:     obs,
		Security:          security,
		Hub:               hub,
		ChatClients:       chatClients,
		Idempotency:       idempotency,
		Version:           "dashboard-fixture",
		ProcessGeneration: "dashboard-fixture-generation",
		Now:               func() time.Time { return fixtureNow },
	})
	dashboard.RegisterTaskRoutes(mux, dashboard.TaskRouteConfig{
		Operations:  operations,
		Schedules:   &jobs,
		Security:    security,
		Idempotency: idempotency,
		Events:      hub,
	})
	dashboard.RegisterWorkspaceRoutes(mux, dashboard.WorkspaceRouteConfig{
		Operations:  operations,
		Security:    security,
		Idempotency: idempotency,
		Events:      hub,
		Egress:      "allowlist",
	})
	dashboard.RegisterMemoryRoutes(mux, dashboard.MemoryRouteConfig{
		Operations:  operations,
		Workspace:   memoryWorkspace,
		Security:    security,
		Idempotency: idempotency,
		Events:      hub,
	})
	dashboard.RegisterCapabilitiesRoutes(mux, dashboard.CapabilitiesRouteConfig{
		Service: capabilities,
		Mutation: func(limit int64, next http.Handler) http.Handler {
			return dashboard.NewMutationHandler(security, idempotency, limit, next)
		},
		Restart: fixtureRestart{},
	})

	server := &http.Server{
		Handler:           security.Wrap(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	fmt.Printf("http://%s\n", listener.Addr().String())

	select {
	case <-ctx.Done():
	case err := <-serveDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal(err)
		}
		return
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		fatal(err)
	}
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

type counterReader struct {
	mu   sync.Mutex
	next byte
}

func (r *counterReader) Read(target []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range target {
		r.next++
		target[index] = r.next
	}
	return len(target), nil
}

type fixtureRuns struct {
	snapshot observability.Snapshot
}

func (r fixtureRuns) Snapshot(context.Context) (observability.Snapshot, error) {
	return r.snapshot, nil
}

type fixtureJobs []schedule.Job

func (j *fixtureJobs) List(context.Context) ([]schedule.Job, error) {
	return append([]schedule.Job(nil), (*j)...), nil
}

func (j *fixtureJobs) AddWithProfile(_ context.Context, name, spec, prompt, deliver, profile string) (*schedule.Job, error) {
	job := schedule.Job{
		ID: "job-added", Name: name, Cron: spec, Prompt: prompt, Deliver: deliver,
		Profile: profile, Enabled: true, MaxAttempts: 1,
	}
	*j = append(*j, job)
	return cloneJob(job), nil
}

func (j *fixtureJobs) Get(_ context.Context, id string) (*schedule.Job, error) {
	for _, job := range *j {
		if job.ID == id {
			return cloneJob(job), nil
		}
	}
	return nil, schedule.ErrJobNotFound
}

func (j *fixtureJobs) Update(_ context.Context, id string, update schedule.Update) (*schedule.Job, error) {
	for index := range *j {
		if (*j)[index].ID != id {
			continue
		}
		(*j)[index].Name = update.Name
		(*j)[index].Cron = update.Cron
		(*j)[index].Prompt = update.Prompt
		(*j)[index].Deliver = update.Deliver
		(*j)[index].Profile = update.Profile
		(*j)[index].Enabled = update.Enabled
		return cloneJob((*j)[index]), nil
	}
	return nil, schedule.ErrJobNotFound
}

func cloneJob(job schedule.Job) *schedule.Job {
	copy := job
	return &copy
}

type fixtureSessions struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
}

func newFixtureSessions() *fixtureSessions {
	return &fixtureSessions{sessions: map[string]*session.Session{
		"session-primary": {
			ID:         "session-primary",
			Title:      "Release review",
			Summary:    "Reviewing the release queue.",
			ModelAlias: "primary",
			CreatedAt:  fixtureNow.Add(-time.Hour),
			UpdatedAt:  fixtureNow,
		},
	}}
}

func (s *fixtureSessions) Get(_ context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	copy := *value
	return &copy, nil
}

func (s *fixtureSessions) Search(context.Context, string, int) ([]session.Hit, error) {
	return []session.Hit{}, nil
}

func (s *fixtureSessions) SearchSummaries(context.Context, string, int) ([]session.Hit, error) {
	return []session.Hit{
		{
			SessionID: "session-primary",
			Title:     "Release review",
			Summary:   "Reviewing the release queue.",
			Snippet:   "Reviewing the release queue.",
			CreatedAt: fixtureNow,
		},
	}, nil
}

func (s *fixtureSessions) SetModelAlias(_ context.Context, id, alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok {
		return session.ErrNotFound
	}
	value.ModelAlias = strings.TrimSpace(alias)
	return nil
}

type fixtureNotes struct{}

func (fixtureNotes) Search(context.Context, string, int) ([]memory.NoteHit, error) {
	return []memory.NoteHit{
		{
			ID:       "note-release",
			Agent:    memory.DefaultAgent,
			Body:     "Use the verified release artifact.",
			RawLine:  "- [2026-07-24] Use the verified release artifact. ^note-release",
			Snippet:  "Use the verified release artifact.",
			NoteDate: fixtureNow,
			Archived: false,
		},
	}, nil
}

type fixtureWorkset struct {
	mu      sync.Mutex
	entries []workset.Entry
}

func (s *fixtureWorkset) Add(_ context.Context, sessionID, kind, body, source string, pinned bool) (*workset.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := workset.Entry{
		ID: "workset-fixture", Kind: kind, Body: body, Source: source, Pinned: pinned,
		CreatedAt: fixtureNow, UpdatedAt: fixtureNow,
	}
	s.entries = append(s.entries, entry)
	return &entry, nil
}

type fixtureUsage struct {
	rows []usage.Row
}

func (u fixtureUsage) List(context.Context, string) ([]usage.Row, error) {
	return append([]usage.Row(nil), u.rows...), nil
}

type fixtureWorkspaces struct {
	mu         sync.Mutex
	workspaces map[string]workspace.Workspace
}

func newFixtureWorkspaces() *fixtureWorkspaces {
	return &fixtureWorkspaces{workspaces: map[string]workspace.Workspace{
		"workspace-clean": {
			ID: "workspace-clean", Repo: "matt-riley/waffle", Image: "waffle-dev:latest",
			SessionID: "session-primary", Status: workspace.StatusOpen, Profile: "reviewer",
			CreatedAt: fixtureNow.Add(-time.Hour), UpdatedAt: fixtureNow, LastActive: fixtureNow,
		},
	}}
}

func (w *fixtureWorkspaces) List(context.Context) ([]workspace.Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]workspace.Workspace, 0, len(w.workspaces))
	for _, item := range w.workspaces {
		result = append(result, item)
	}
	return result, nil
}

func (w *fixtureWorkspaces) Get(_ context.Context, id string) (*workspace.Workspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	item, ok := w.workspaces[id]
	if !ok {
		return nil, workspace.ErrWorkspaceNotFound
	}
	copy := item
	return &copy, nil
}

func (w *fixtureWorkspaces) OpenWithProfile(_ context.Context, repository, profile string) (*workspace.Workspace, *sandbox.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	item := workspace.Workspace{
		ID: "workspace-opened", Repo: repository, Image: "waffle-dev:latest",
		SessionID: "session-primary", Status: workspace.StatusOpen, Profile: profile,
		CreatedAt: fixtureNow, UpdatedAt: fixtureNow, LastActive: fixtureNow,
	}
	w.workspaces[item.ID] = item
	copy := item
	return &copy, nil, nil
}

func (w *fixtureWorkspaces) Idle(_ context.Context, id string) error {
	return w.updateStatus(id, workspace.StatusIdle)
}

func (w *fixtureWorkspaces) Resume(_ context.Context, id string) (*workspace.Workspace, *sandbox.Client, error) {
	if err := w.updateStatus(id, workspace.StatusOpen); err != nil {
		return nil, nil, err
	}
	item, err := w.Get(context.Background(), id)
	return item, nil, err
}

func (w *fixtureWorkspaces) InspectClose(context.Context, string) (*workspace.CloseReport, error) {
	return &workspace.CloseReport{}, nil
}

func (w *fixtureWorkspaces) Close(_ context.Context, id string, _ bool) (*workspace.CloseReport, error) {
	_, _, err := w.CloseTransition(context.Background(), id, false)
	return &workspace.CloseReport{}, err
}

func (w *fixtureWorkspaces) InspectCloseGuarded(_ context.Context, id string, accept func(*workspace.CloseReport) error) (*workspace.CloseReport, error) {
	if _, err := w.Get(context.Background(), id); err != nil {
		return nil, err
	}
	report := &workspace.CloseReport{}
	if accept != nil {
		if err := accept(report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func (w *fixtureWorkspaces) CloseTransition(_ context.Context, id string, _ bool) (*workspace.CloseReport, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	item, ok := w.workspaces[id]
	if !ok {
		return nil, false, workspace.ErrWorkspaceNotFound
	}
	if item.Status == workspace.StatusClosed {
		return &workspace.CloseReport{}, false, workspace.ErrWorkspaceAlreadyClosed
	}
	item.Status = workspace.StatusClosed
	item.UpdatedAt = fixtureNow
	w.workspaces[id] = item
	return &workspace.CloseReport{}, true, nil
}

func (w *fixtureWorkspaces) updateStatus(id, status string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	item, ok := w.workspaces[id]
	if !ok {
		return workspace.ErrWorkspaceNotFound
	}
	item.Status = status
	item.UpdatedAt = fixtureNow
	w.workspaces[id] = item
	return nil
}

type fixtureProviders struct {
	mu      sync.Mutex
	listing providerconfig.Listing
}

func (p *fixtureProviders) Snapshot(context.Context) (providerconfig.Listing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listing, nil
}

func (p *fixtureProviders) AddWithMode(_ context.Context, request providerconfig.AddRequest, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listing.Providers[request.ConnectionName] = providerconfig.ProviderSummary{
		Type: request.Connection.Type, BaseURL: request.Connection.BaseURL,
	}
	for alias, target := range request.Models {
		p.listing.Models[alias] = providerconfig.ModelSummary{
			Provider: request.ConnectionName, Model: target.Model,
		}
	}
	if request.DefaultModel != "" {
		p.listing.DefaultModel = request.DefaultModel
	}
	return providerconfig.MutationResult{}, nil
}

func (p *fixtureProviders) AddModelWithMode(_ context.Context, request providerconfig.AddModelRequest, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listing.Models[request.Alias] = providerconfig.ModelSummary{
		Provider: request.ConnectionName, Model: request.UpstreamModel,
	}
	return providerconfig.MutationResult{}, nil
}

func (p *fixtureProviders) ActivateModelWithMode(_ context.Context, alias string, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listing.DefaultModel = alias
	return providerconfig.MutationResult{}, nil
}

func (p *fixtureProviders) ActivateUtilityModelWithMode(_ context.Context, alias string, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listing.UtilityModel = alias
	return providerconfig.MutationResult{}, nil
}

type fixtureSkills struct {
	mu    sync.Mutex
	items []dashboard.CapabilitySkill
}

func (s *fixtureSkills) List(context.Context, string) ([]dashboard.CapabilitySkill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dashboard.CapabilitySkill(nil), s.items...), nil
}

func (s *fixtureSkills) Attach(context.Context, string, string) error { return nil }

func (s *fixtureSkills) Detach(context.Context, string, string) error { return nil }

func (s *fixtureSkills) Stage(context.Context, skillinstall.StageRequest) (skillinstall.Manifest, error) {
	return skillinstall.Manifest{
		Name: "fixture-reviewed", Description: "Reviewed fixture skill",
		SourceRef: "fixture", ContentDigest: "sha256:fixture", StageID: "stage-fixture",
		ExpiresAt: fixtureNow.Add(time.Minute), Audit: skillinstall.AuditView{Passed: true},
		Files: []skillinstall.FileEntry{},
	}, nil
}

func (s *fixtureSkills) Install(context.Context, string, string) (dashboard.CapabilitySkill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := dashboard.CapabilitySkill{
		Name: "fixture-reviewed", Description: "Reviewed fixture skill", Active: false,
	}
	s.items = append(s.items, item)
	return item, nil
}

func (s *fixtureSkills) Activate(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.items {
		if s.items[index].Name == name {
			s.items[index].Active = true
			return nil
		}
	}
	return dashboard.ErrCapabilitySkillNotFound
}

type fixtureCatalogue struct{}

func (fixtureCatalogue) Refresh(context.Context, string) (dashboard.CapabilityCatalogueResult, error) {
	return dashboard.CapabilityCatalogueResult{
		Result: modelcatalog.Result{
			Record: modelcatalog.Record{
				SchemaVersion: modelcatalog.SchemaVersion,
				Connection:    modelcatalog.Connection{Name: "fixture"},
				FetchedAt:     fixtureNow,
				Models: []modelcatalog.Model{
					{ID: "fixture-model", DisplayName: "Fixture model", Owner: "Waffle"},
				},
			},
		},
	}, nil
}

type fixtureRestart struct{}

func (fixtureRestart) Schedule(context.Context, string) error { return nil }

type fixtureChatBackend struct {
	sessions *fixtureSessions
	skills   *fixtureSkills
	session  string
}

func (b *fixtureChatBackend) Open(_ context.Context, options chat.OpenOptions) (chat.State, error) {
	b.session = strings.TrimSpace(options.SessionID)
	if b.session == "" {
		b.session = "session-primary"
	}
	return b.state()
}

func (b *fixtureChatBackend) Turn(ctx context.Context, input string, emit func(chat.Event)) error {
	if strings.Contains(strings.ToLower(input), "wait") {
		<-ctx.Done()
		emit(chat.Event{Kind: chat.EventTurnDone})
		return nil
	}
	emit(chat.Event{Kind: chat.EventTextDelta, Text: "Fixture reply"})
	emit(chat.Event{Kind: chat.EventTurnDone})
	return nil
}

func (b *fixtureChatBackend) Command(_ context.Context, command chat.ParsedCommand, _ func(chat.Event)) (chat.Result, error) {
	switch command.Name {
	case "model":
		if err := b.sessions.SetModelAlias(context.Background(), b.session, command.Args); err != nil {
			return chat.Result{}, err
		}
	case "skills":
		parts := strings.Fields(command.Args)
		if len(parts) == 2 && parts[0] == "attach" {
			_ = b.skills.Attach(context.Background(), b.session, parts[1])
		}
		if len(parts) == 2 && parts[0] == "detach" {
			_ = b.skills.Detach(context.Background(), b.session, parts[1])
		}
	}
	state, err := b.state()
	if err != nil {
		return chat.Result{}, err
	}
	return chat.Result{State: &state}, nil
}

func (b *fixtureChatBackend) Cancel() {}

func (b *fixtureChatBackend) Close(context.Context) error { return nil }

func (b *fixtureChatBackend) state() (chat.State, error) {
	current, err := b.sessions.Get(context.Background(), b.session)
	if err != nil {
		return chat.State{}, err
	}
	return chat.State{
		SessionID:      current.ID,
		Title:          current.Title,
		ModelAlias:     current.ModelAlias,
		ProviderLabel:  "Fixture provider",
		Profile:        "reviewer",
		ConnectionMode: "Connected",
		SandboxMode:    "workspace-write",
		Workspace:      "matt-riley/waffle",
		History:        []llm.Message{},
		Models: []chat.Model{
			{Alias: "primary", Provider: "fixture", Upstream: "primary-model", Current: current.ModelAlias == "primary"},
			{Alias: "local", Provider: "fixture", Upstream: "local-model", Current: current.ModelAlias == "local"},
		},
		Skills: []chat.SkillRef{
			{Name: "review", Description: "Review changes"},
		},
		Capabilities: []string{},
	}, nil
}

var (
	_ io.Reader                         = (*counterReader)(nil)
	_ dashboard.RunReader               = fixtureRuns{}
	_ dashboard.TaskScheduleStore       = (*fixtureJobs)(nil)
	_ dashboard.SessionStore            = (*fixtureSessions)(nil)
	_ dashboard.CapabilitySessions      = (*fixtureSessions)(nil)
	_ dashboard.NotesSearcher           = fixtureNotes{}
	_ dashboard.WorksetStore            = (*fixtureWorkset)(nil)
	_ dashboard.UsageReader             = fixtureUsage{}
	_ dashboard.WorkspaceManager        = (*fixtureWorkspaces)(nil)
	_ dashboard.WorkspaceCloseLifecycle = (*fixtureWorkspaces)(nil)
	_ dashboard.CapabilityProviders     = (*fixtureProviders)(nil)
	_ dashboard.CapabilitySkills        = (*fixtureSkills)(nil)
	_ dashboard.CapabilityCatalogue     = fixtureCatalogue{}
	_ dashboard.RestartScheduler        = fixtureRestart{}
	_ chat.Backend                      = (*fixtureChatBackend)(nil)
)
