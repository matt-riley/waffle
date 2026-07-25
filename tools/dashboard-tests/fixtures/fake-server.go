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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/dashboard"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/policy"
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
	security, err := dashboard.NewSecurity(listener.Addr().String(), dashboard.TailnetOptions{}, entropy)
	if err != nil {
		fatal(err)
	}
	hub := dashboard.NewEventHub(128)
	idempotency := dashboard.NewIdempotencyStore(func() time.Time { return fixtureNow }, 256, 5*time.Minute)
	previews := dashboard.NewPreviewStore(func() time.Time { return fixtureNow }, entropy)

	sessions := newFixtureSessions()
	workspaces := newFixtureWorkspaces()
	memoryRoot := filepath.Join(root, "workspace")
	if err := os.MkdirAll(memoryRoot, 0o700); err != nil {
		fatal(err)
	}
	memoryLine := "- [2026-07-24] Use the verified release artifact. [id=a1b2c3]\n"
	if err := os.WriteFile(filepath.Join(memoryRoot, "MEMORY.md"), []byte(memoryLine), 0o600); err != nil {
		fatal(err)
	}
	notes := fixtureNotes{memoryPath: filepath.Join(memoryRoot, "MEMORY.md")}
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
		Notes:      notes,
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
		Operations:        operations,
		Schedules:         &jobs,
		Memory:            memoryWorkspace,
		WorkspaceEgress:   "allowlist",
		Capabilities:      capabilities,
		Restart:           fixtureRestart{},
		Posture:           dashboard.NewPostureService(postureFixtureConfig(), nil, fixturePostureAudit{}),
		Version:           "dashboard-fixture",
		ProcessGeneration: "dashboard-fixture-generation",
		Now:               func() time.Time { return fixtureNow },
	})
	dashboard.RegisterConnectionsRoutes(mux, dashboard.NewConnectionSource(config.Config{
		Providers: map[string]config.ProviderConnection{
			"fixture": {
				Type:   "openai",
				APIKey: "secret://desk-secret-canary",
			},
		},
		MCP: []config.MCPServer{
			{
				Name:      "fixture-tools",
				Command:   "mcp --raw-command-canary",
				Execution: "sandbox",
				Env:       []string{"WAFFLE_PRIVATE_ENV"},
			},
		},
		Agent: config.Agent{
			Profiles: map[string]config.AgentProfile{
				"reviewer": {
					System:  "@/var/lib/waffle/private",
					Sandbox: "docker",
				},
			},
		},
		Workspace: config.Workspace{Egress: "allowlist"},
		GitHub: config.GitHub{App: config.GitHubApp{
			AppID:          4242,
			InstallationID: 8484,
			PrivateKey:     "secret://desk-github-key-canary",
			BaseURL:        "https://github.example.invalid/api/v3",
		}},
		Intake: config.Intake{GitHub: []config.GitHubWatch{{
			Repo:           "fixture/board",
			Label:          "waffle",
			MaxConcurrency: 2,
			Deliver:        "telegram:canary",
			Token:          "secret://desk-intake-token-canary",
		}}},
	}, nil, nil))

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

type fixtureNotes struct {
	memoryPath string
}

func (n fixtureNotes) Search(context.Context, string, int) ([]memory.NoteHit, error) {
	content, err := os.ReadFile(n.memoryPath)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(string(content), "[id=a1b2c3]") {
		return []memory.NoteHit{}, nil
	}
	return []memory.NoteHit{
		{
			ID:       "a1b2c3",
			Agent:    memory.DefaultAgent,
			Body:     "Use the verified release artifact.",
			RawLine:  "- [2026-07-24] Use the verified release artifact. [id=a1b2c3]",
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
		"workspace-dirty": {
			ID: "workspace-dirty", Repo: "matt-riley/waffle-dirty", Image: "waffle-dev:latest",
			SessionID: "session-primary", Status: workspace.StatusOpen, Profile: "reviewer",
			CreatedAt: fixtureNow.Add(-30 * time.Minute), UpdatedAt: fixtureNow, LastActive: fixtureNow,
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
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
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

func (w *fixtureWorkspaces) InspectGit(_ context.Context, id string) (*workspace.GitStatus, error) {
	item, err := w.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if item.Status != workspace.StatusOpen {
		return nil, workspace.ErrWorkspaceNotRunning
	}
	if id == "workspace-dirty" {
		return &workspace.GitStatus{
			Branch: "feature/dirty", DirtyFiles: 1, Tracking: true, Ahead: 1,
			CommitSHA: "abc1234", Subject: "local commit",
		}, nil
	}
	return &workspace.GitStatus{
		Branch: "main", Tracking: true,
		CommitSHA: "def5678", Subject: "chore: release",
	}, nil
}

func (w *fixtureWorkspaces) InspectClose(_ context.Context, id string) (*workspace.CloseReport, error) {
	if _, err := w.Get(context.Background(), id); err != nil {
		return nil, err
	}
	return fixtureWorkspaceCloseReport(id), nil
}

func (w *fixtureWorkspaces) Close(_ context.Context, id string, _ bool) (*workspace.CloseReport, error) {
	_, _, err := w.CloseTransition(context.Background(), id, false)
	return &workspace.CloseReport{}, err
}

func (w *fixtureWorkspaces) InspectCloseGuarded(_ context.Context, id string, accept func(*workspace.CloseReport) error) (*workspace.CloseReport, error) {
	if _, err := w.Get(context.Background(), id); err != nil {
		return nil, err
	}
	report := fixtureWorkspaceCloseReport(id)
	if accept != nil {
		if err := accept(report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func (w *fixtureWorkspaces) CloseTransition(_ context.Context, id string, force bool) (*workspace.CloseReport, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	item, ok := w.workspaces[id]
	if !ok {
		return nil, false, workspace.ErrWorkspaceNotFound
	}
	if item.Status == workspace.StatusClosed {
		return &workspace.CloseReport{}, false, workspace.ErrWorkspaceAlreadyClosed
	}
	report := fixtureWorkspaceCloseReport(id)
	if !force && (report.Dirty != "" || report.Unpushed != "") {
		return report, false, fmt.Errorf("workspace %s has unsaved work", id)
	}
	item.Status = workspace.StatusClosed
	item.UpdatedAt = fixtureNow
	w.workspaces[id] = item
	return report, true, nil
}

func fixtureWorkspaceCloseReport(id string) *workspace.CloseReport {
	if id == "workspace-dirty" {
		return &workspace.CloseReport{
			Dirty:    "M main.go",
			Unpushed: "abc123 local commit",
		}
	}
	return &workspace.CloseReport{}
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

func (*fixtureProviders) Test(context.Context, string) error { return nil }

func (*fixtureProviders) TestProspective(context.Context, providerconfig.ProspectiveProbeRequest) error {
	return nil
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

func (s *fixtureSkills) Deactivate(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.items {
		if s.items[index].Name == name {
			s.items[index].Active = false
			return nil
		}
	}
	return dashboard.ErrCapabilitySkillNotFound
}

func (s *fixtureSkills) Uninstall(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.items {
		if s.items[index].Name == name {
			s.items = append(s.items[:index], s.items[index+1:]...)
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
	history  []llm.Message
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
	if strings.Contains(strings.ToLower(input), "markdown") {
		emit(chat.Event{
			Kind:       chat.EventToolStarted,
			ToolName:   "fixture_read",
			ToolCallID: "tool-1",
		})
		emit(chat.Event{
			Kind:       chat.EventToolFinished,
			ToolName:   "fixture_read",
			ToolCallID: "tool-1",
			ByteCount:  24,
			DurationMS: 18,
		})
		emit(chat.Event{
			Kind: chat.EventTextDelta,
			Text: "## Fixture markdown\n\n- one\n- two\n\nUse `mise`.\n\n```go\nfmt.Println(\"fixture\")\n```",
		})
		emit(chat.Event{Kind: chat.EventTurnDone})
		return nil
	}
	emit(chat.Event{Kind: chat.EventTextDelta, Text: "Fixture reply"})
	emit(chat.Event{Kind: chat.EventTurnDone})
	return nil
}

func (b *fixtureChatBackend) Command(_ context.Context, command chat.ParsedCommand, _ func(chat.Event)) (chat.Result, error) {
	switch command.Name {
	case "new":
		if strings.TrimSpace(command.Args) != "confirm" {
			return chat.Result{Confirm: true, Text: "Start a new fixture conversation?"}, nil
		}
		b.sessions.mu.Lock()
		b.sessions.sessions["session-fresh"] = &session.Session{
			ID:         "session-fresh",
			Title:      "Fresh conversation",
			Summary:    "A fresh fixture session.",
			ModelAlias: "primary",
			CreatedAt:  fixtureNow,
			UpdatedAt:  fixtureNow,
		}
		b.sessions.mu.Unlock()
		b.session = "session-fresh"
		b.history = []llm.Message{{
			Role: llm.RoleAssistant,
			Blocks: []llm.Block{{
				Type: llm.BlockText,
				Text: "Fresh start",
			}},
		}}
	case "sessions":
		return chat.Result{Sessions: []chat.Session{
			{
				ID:         "session-primary",
				Title:      "Release review",
				Summary:    "Reviewing the release queue.",
				ModelAlias: "primary",
				UpdatedAt:  fixtureNow,
			},
			{
				ID:         "session-fresh",
				Title:      "Fresh conversation",
				Summary:    "A fresh fixture session.",
				ModelAlias: "primary",
				UpdatedAt:  fixtureNow,
			},
		}}, nil
	case "resume":
		b.session = strings.TrimSpace(command.Args)
		b.history = nil
	case "usage":
		return chat.Result{Usage: []chat.UsageRow{{
			SessionID:      b.session,
			Period:         "today",
			Requests:       3,
			InputTokens:    120,
			OutputTokens:   45,
			ReservedTokens: 10,
		}}}, nil
	case "permissions":
		return chat.Result{Permissions: &chat.PermissionView{
			SandboxMode:  "workspace-write",
			Allow:        []string{"read"},
			Deny:         []string{"bash"},
			DenyPrefixes: []string{"secret."},
		}}, nil
	case "workset":
		return chat.Result{Workset: []chat.WorkItem{{
			ID:   "goal-fixture",
			Text: "Verify the Today experience",
		}}}, nil
	case "help":
		return chat.Result{Commands: []chat.Command{{
			Name:        chat.CommandNew,
			Usage:       "/new",
			Description: "Start a conversation",
		}}}, nil
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
		History:        append([]llm.Message(nil), b.history...),
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

// postureFixtureConfig carries canary-bearing policy so the browser suite can
// prove the posture surface redacts as well as renders (#193).
func postureFixtureConfig() config.Config {
	return config.Config{
		Sandbox: config.Sandbox{Mode: "host"},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				config.GroupMain: {Tools: config.ToolPolicy{
					Allow:        []string{"bash", "read"},
					DenyPrefixes: []string{"rm -rf", "/var/lib/waffle/private/run.sh"},
					Guidance:     "Group guidance.",
				}},
			},
			Profiles: map[string]config.AgentProfile{
				"reviewer": {
					System:        "You review changes. Never echo desk-secret-canary.",
					Model:         "primary",
					Sandbox:       "docker",
					Tools:         config.ToolPolicy{Allow: []string{"read"}, Deny: []string{"bash"}},
					DenyPrefixes:  []string{"git push"},
					MaxTokens:     4096,
					MaxIterations: 12,
				},
			},
		},
	}
}

type fixturePostureAudit struct{}

func (fixturePostureAudit) RecentDenials(context.Context, string, int) ([]policy.AuditEntry, error) {
	return []policy.AuditEntry{{
		At: "2026-07-24T12:00:00Z", Session: "session-primary", Tool: "bash",
		Command: "git push --force", Rule: "no-force-push", Verdict: "deny",
		Detail: "Force pushes are refused on shared branches.",
	}}, nil
}
