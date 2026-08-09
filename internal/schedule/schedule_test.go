package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/robfig/cron/v3"
)

func newTestStore(t *testing.T) *store.Store {
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
	return st
}

func TestAddValidatesCron(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestStore(t))

	if _, err := s.Add(ctx, "bad", "not a cron", "do it", ""); err == nil {
		t.Error("bad cron accepted")
	}
	j, err := s.Add(ctx, "standup", "0 9 * * 1-5", "summarize", "telegram:900")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if j.Cron != "0 9 * * 1-5" || j.Deliver != "telegram:900" || !j.Enabled {
		t.Errorf("job = %+v", j)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if err := s.Remove(ctx, j.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove(ctx, j.ID); err == nil {
		t.Error("double remove succeeded")
	}
}

func TestAddReturnsCanonicalCommittedJob(t *testing.T) {
	jobs := NewStore(newTestStore(t))
	created, err := jobs.AddWithProfile(
		context.Background(), "brief", "0 9 * * *", "summarize", "telegram:900", "researcher",
	)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := jobs.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.ID != stored.ID || created.Name != stored.Name ||
		created.Cron != stored.Cron || created.Prompt != stored.Prompt ||
		created.Deliver != stored.Deliver || created.Profile != stored.Profile ||
		created.Enabled != stored.Enabled || created.CreatedAt != stored.CreatedAt ||
		created.MaxAttempts != stored.MaxAttempts || created.BaseBackoff != stored.BaseBackoff ||
		created.MaxBackoff != stored.MaxBackoff || created.StallTimeout != stored.StallTimeout {
		t.Fatalf("created job is not canonical: created=%+v stored=%+v", created, stored)
	}
}

func TestJobProfileField(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestStore(t))
	j, err := s.AddWithProfile(ctx, "research", "0 * * * *", "dig", "", "researcher")
	if err != nil {
		t.Fatal(err)
	}
	if j.Profile != "researcher" {
		t.Fatalf("profile = %q", j.Profile)
	}
	got, err := s.Get(ctx, j.ID)
	if err != nil || got.Profile != "researcher" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if !sameDefinition(*j, *got) {
		t.Fatal("sameDefinition should include profile")
	}
	other := *got
	other.Profile = "other"
	if sameDefinition(*got, other) {
		t.Fatal("profile change should break sameDefinition")
	}
}

func TestUpdateValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	jobs := NewStore(st)
	job, err := jobs.AddWithProfile(ctx, "morning brief", "0 9 * * 1-5", "summarize", "telegram:900", "researcher")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		update Update
	}{
		{
			name:   "invalid cron",
			update: Update{Name: "edited", Cron: "not cron", Prompt: "changed", Deliver: "telegram:901", Profile: "reviewer", Enabled: false},
		},
		{
			name:   "empty name",
			update: Update{Name: " \t", Cron: "0 10 * * *", Prompt: "changed", Deliver: "", Profile: "", Enabled: true},
		},
		{
			name:   "empty prompt",
			update: Update{Name: "edited", Cron: "0 10 * * *", Prompt: "\n ", Deliver: "", Profile: "", Enabled: true},
		},
		{
			name:   "malformed delivery",
			update: Update{Name: "edited", Cron: "0 10 * * *", Prompt: "changed", Deliver: "telegram", Profile: "", Enabled: true},
		},
		{
			name:   "invalid profile",
			update: Update{Name: "edited", Cron: "0 10 * * *", Prompt: "changed", Deliver: "", Profile: "../admin", Enabled: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := jobs.Update(ctx, job.ID, test.update); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatal("invalid update succeeded")
			}
			got, err := jobs.Get(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != job.Name || got.Cron != job.Cron || got.Prompt != job.Prompt ||
				got.Deliver != job.Deliver || got.Profile != job.Profile || got.Enabled != job.Enabled {
				t.Fatalf("failed update mutated stored job: got %+v, want %+v", got, job)
			}
		})
	}
}

func TestUpdatePreservesExecutionBookkeeping(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	jobs := NewStore(st)
	job, err := jobs.Add(ctx, "old", "0 9 * * *", "old prompt", "")
	if err != nil {
		t.Fatal(err)
	}
	lastRun := time.Date(2026, time.July, 24, 8, 0, 0, 0, time.UTC)
	nextRetry := lastRun.Add(15 * time.Minute)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET
		last_run=?, last_status=?, attempt=?, next_retry=?, max_attempts=?,
		base_backoff=?, max_backoff=?, stall_timeout=?
		WHERE id=?`,
		lastRun.Format(time.RFC3339Nano), "failed: canary", 3,
		nextRetry.Format(time.RFC3339Nano), 3, "15s", "15m", "7m", job.ID); err != nil {
		t.Fatal(err)
	}
	before, err := jobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Update(ctx, job.ID, Update{
		Name: "new", Cron: "30 10 * * 1-5", Prompt: "new prompt",
		Deliver: "telegram:901", Profile: "reviewer", Enabled: false,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "new" || got.Cron != "30 10 * * 1-5" || got.Prompt != "new prompt" ||
		got.Deliver != "telegram:901" || got.Profile != "reviewer" || got.Enabled {
		t.Fatalf("updated definition = %+v", got)
	}
	if got.LastRun != before.LastRun || got.LastStatus != before.LastStatus ||
		got.CreatedAt != before.CreatedAt || got.Attempt != before.Attempt ||
		got.NextRetry != before.NextRetry || got.MaxAttempts != before.MaxAttempts ||
		got.BaseBackoff != before.BaseBackoff || got.MaxBackoff != before.MaxBackoff ||
		got.StallTimeout != before.StallTimeout {
		t.Fatalf("bookkeeping changed: before=%+v after=%+v", before, got)
	}
}

func TestUpdateMissingJobReturnsStableNotFound(t *testing.T) {
	jobs := NewStore(newTestStore(t))
	_, err := jobs.Update(context.Background(), "job-missing", Update{
		Name: "valid", Cron: "0 9 * * *", Prompt: "valid", Enabled: true,
	})
	if !errors.Is(err, ErrJobNotFound) || !strings.Contains(err.Error(), "job not found") {
		t.Fatalf("error = %v, want stable not found", err)
	}
}

func TestStoreOperationsHonorDeadlineWithoutCommitting(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Store, string) error
	}{
		{
			name: "add",
			run: func(ctx context.Context, jobs *Store, _ string) error {
				_, err := jobs.Add(ctx, "new", "0 10 * * *", "new prompt", "")
				return err
			},
		},
		{
			name: "get",
			run: func(ctx context.Context, jobs *Store, jobID string) error {
				_, err := jobs.Get(ctx, jobID)
				return err
			},
		},
		{
			name: "update",
			run: func(ctx context.Context, jobs *Store, jobID string) error {
				_, err := jobs.Update(ctx, jobID, Update{
					Name: "new", Cron: "0 10 * * *", Prompt: "new prompt", Enabled: true,
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newTestStore(t)
			jobs := NewStore(st)
			job, err := jobs.Add(context.Background(), "old", "0 9 * * *", "old prompt", "")
			if err != nil {
				t.Fatal(err)
			}
			held, err := st.DB.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			started := time.Now()
			err = test.run(ctx, jobs, job.ID)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				_ = held.Rollback()
				t.Fatalf("error = %v, want deadline exceeded", err)
			}
			if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
				_ = held.Rollback()
				t.Fatalf("operation returned after %v", elapsed)
			}
			if err := held.Rollback(); err != nil {
				t.Fatal(err)
			}

			stored, err := jobs.Get(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != "old" || stored.Cron != "0 9 * * *" || stored.Prompt != "old prompt" {
				t.Fatalf("deadline operation changed stored job: %+v", stored)
			}
			rows, err := jobs.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("jobs = %d, want only original job", len(rows))
			}
		})
	}
}

// echoProvider replies with the user's text, verbatim.
type echoProvider struct{}

func (echoProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].Text()
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done: " + last}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

type shutdownBlockingProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *shutdownBlockingProvider) Complete(ctx context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-p.release // deliberately ignore cancellation: accepted cron work drains
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}}, StopReason: llm.StopEndTurn}, nil
}

// captureDeliverer records deliveries.
type captureDeliverer struct {
	target, text string
}

func (c *captureDeliverer) Deliver(ctx context.Context, target, text string) error {
	c.target, c.text = target, text
	return nil
}

func TestRunnerExecutesAndPersistsSession(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sessions := session.New(st)
	cap := &captureDeliverer{}
	runner := &Runner{
		Agent:     &agent.Agent{Provider: echoProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		Sessions:  sessions,
		Deliverer: cap,
	}

	reply, err := runner.Run(ctx, Job{Name: "test", Prompt: "check the thing"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "done: check the thing" {
		t.Errorf("reply = %q", reply)
	}
	// Delivery is the scheduler's job (after the outcome is durable); the
	// runner itself never delivers (see fire and TestFirePersistsSuccessBeforeDelivering).
	if cap.target != "" || cap.text != "" {
		t.Errorf("runner delivered %q to %q; delivery belongs to the scheduler", cap.text, cap.target)
	}

	// The job's turns were persisted to a fresh session.
	list, _ := sessions.List(ctx, 10)
	if len(list) != 1 {
		t.Fatalf("sessions = %d", len(list))
	}
	if list[0].Summary == "" {
		t.Fatal("completed cron session was not reflected into a summary")
	}
	turns, _ := sessions.Turns(ctx, list[0].ID)
	if len(turns) != 2 {
		t.Errorf("turns = %d, want 2", len(turns))
	}
}

func TestRunnerReservedLearnJobReturnsDigestWithoutAgentDispatch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	cap := &captureDeliverer{}
	p := &recordingProvider{}
	var logs bytes.Buffer
	runner := &Runner{
		Agent:     &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"},
		Sessions:  session.New(st),
		Deliverer: cap,
		Log:       slog.New(slog.NewTextHandler(&logs, nil)),
		Learn: func(context.Context) (string, error) {
			return "PRIVATE_LEARN_DIGEST\npatterns=2 accepted=1", nil
		},
	}
	reply, err := runner.Run(ctx, Job{ID: "job-learn", Name: "learn-daily", Prompt: "/learn", Deliver: "telegram:900", Profile: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "PRIVATE_LEARN_DIGEST") {
		t.Fatalf("reply=%q", reply)
	}
	// Delivery is the scheduler's job; the runner returns the digest.
	if cap.target != "" || cap.text != "" {
		t.Fatalf("runner delivered %q to %q; delivery belongs to the scheduler", cap.text, cap.target)
	}
	if len(p.reqs) != 0 {
		t.Fatalf("reserved learn job reached agent provider: %d calls", len(p.reqs))
	}
	body := logs.String()
	for _, want := range []string{`msg="cron run started"`, `msg="cron run finished"`, "job_id=job-learn", "profile=reviewer"} {
		if !strings.Contains(body, want) {
			t.Fatalf("reserved learn logs missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "/learn") || strings.Contains(body, "PRIVATE_LEARN_DIGEST") {
		t.Fatalf("reserved learn input/output leaked into logs: %s", body)
	}
}

// recordingProvider captures Complete requests so tests can assert system
// prompt and tool definitions for profile selection (#71). Reflect may call
// Complete again; tests inspect the first agent-turn request via first().
type recordingProvider struct {
	reqs []llm.Request
}

func (p *recordingProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.reqs = append(p.reqs, req)
	// Text-shaped replies so Reflect (if it runs) also completes cleanly.
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func (p *recordingProvider) first() llm.Request {
	if len(p.reqs) == 0 {
		return llm.Request{}
	}
	return p.reqs[0]
}

type namedTool string

func (n namedTool) Def() llm.Tool {
	return llm.Tool{Name: string(n), InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (n namedTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	return string(n), nil
}

func TestRunnerUsesProfileAgent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defaultProv := &recordingProvider{}
	profileProv := &recordingProvider{}
	var logs bytes.Buffer
	runner := &Runner{
		Agent: &agent.Agent{
			Provider: defaultProv,
			Tools:    tool.NewRegistry(namedTool("bash")),
			System:   "default-cron-system",
			Model:    "default-model",
		},
		AgentsByProfile: map[string]*agent.Agent{
			"researcher": {
				Provider: profileProv,
				Tools:    tool.NewRegistry(namedTool("read_file"), namedTool("search")),
				System:   "researcher-profile-system",
				Model:    "research-model",
			},
		},
		Sessions: session.New(st),
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
	}

	if _, err := runner.Run(ctx, Job{ID: "j1", Name: "dig", Prompt: "research topic", Profile: "researcher"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := profileProv.first()
	if got.System != "researcher-profile-system" {
		t.Fatalf("system = %q, want researcher-profile-system", got.System)
	}
	if got.Model != "research-model" {
		t.Fatalf("model = %q, want research-model", got.Model)
	}
	toolNames := make([]string, 0, len(got.Tools))
	for _, d := range got.Tools {
		toolNames = append(toolNames, d.Name)
	}
	if !slices.Contains(toolNames, "read_file") || !slices.Contains(toolNames, "search") {
		t.Fatalf("tools = %v, want read_file+search from profile", toolNames)
	}
	if slices.Contains(toolNames, "bash") {
		t.Fatalf("tools include bash from default agent: %v", toolNames)
	}
	if len(defaultProv.reqs) != 0 {
		t.Fatal("default agent should not have been used")
	}
	if !strings.Contains(logs.String(), "profile=researcher") {
		t.Errorf("logs missing profile: %s", logs.String())
	}
}

func TestRunnerUnknownProfileErrors(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	runner := &Runner{
		Agent:           &agent.Agent{Provider: echoProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		AgentsByProfile: map[string]*agent.Agent{}, // non-nil: unknown must error, not fall back
		Sessions:        session.New(st),
	}
	_, err := runner.Run(ctx, Job{Name: "x", Prompt: "p", Profile: "missing"})
	if err == nil || !strings.Contains(err.Error(), `cron: unknown profile "missing"`) {
		t.Fatalf("err = %v, want unknown profile", err)
	}
}

func TestRunnerEmptyProfileUsesDefaultAgent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defaultProv := &recordingProvider{}
	runner := &Runner{
		Agent: &agent.Agent{
			Provider: defaultProv,
			Tools:    tool.NewRegistry(),
			System:   "default-cron-system",
			Model:    "m",
		},
		AgentsByProfile: map[string]*agent.Agent{
			"researcher": {
				Provider: &recordingProvider{},
				Tools:    tool.NewRegistry(),
				System:   "other",
				Model:    "other",
			},
		},
		Sessions: session.New(st),
	}
	if _, err := runner.Run(ctx, Job{Name: "x", Prompt: "p"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if defaultProv.first().System != "default-cron-system" {
		t.Fatalf("system = %q", defaultProv.first().System)
	}
}

func TestRunnerRecordsCronRunAndLogsSessionAndJobIDs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	var logs bytes.Buffer
	runner := &Runner{
		Agent:         &agent.Agent{Provider: echoProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		Sessions:      session.New(st),
		Observability: observability.New(st, nil),
		Log:           slog.New(slog.NewTextHandler(&logs, nil)),
	}
	job := Job{ID: "job-123", Name: "test", Prompt: "check the thing"}

	if _, err := runner.Run(ctx, job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snapshot, err := runner.Observability.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].Source != "cron" || snapshot.Recent[0].Outcome != "ok" || snapshot.Recent[0].SessionID == "" {
		t.Fatalf("recent runs = %+v", snapshot.Recent)
	}
	if !strings.Contains(logs.String(), "session_id="+snapshot.Recent[0].SessionID) || !strings.Contains(logs.String(), "job_id="+job.ID) {
		t.Errorf("logs missing run context: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "profile=main") || !strings.Contains(logs.String(), `msg="cron run finished"`) {
		t.Errorf("logs missing profile/end event: %s", logs.String())
	}
}

func TestSchedulerReconcilesJobChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := newTestStore(t)
	jobs := NewStore(st)
	sched := &Scheduler{
		Store: jobs,
		Runner: &Runner{
			Agent:    &agent.Agent{Provider: echoProvider{}, Tools: tool.NewRegistry(), Model: "m"},
			Sessions: session.New(st),
		},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reconcile: 10 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// A job added after Run has started is picked up by reconciliation.
	j, err := jobs.Add(ctx, "later", "0 9 * * *", "summarize", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFor(t, "job registered", func() bool {
		return slices.Contains(sched.registeredIDs(), j.ID)
	})

	// Removing it drops the cron entry on a later pass.
	if err := jobs.Remove(ctx, j.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitFor(t, "job deregistered", func() bool {
		return !slices.Contains(sched.registeredIDs(), j.ID)
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestAcceptanceIssue10SchedulerCancellationDrainsInFlightJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := newTestStore(t)
	jobs := NewStore(st)
	j, err := jobs.Add(ctx, "shutdown", "0 9 * * *", "finish safely", "")
	if err != nil {
		t.Fatal(err)
	}
	// A seconds parser makes the real cron fire promptly without changing the
	// production five-field parser.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET cron='*/1 * * * * *' WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	p := &shutdownBlockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	sched := &Scheduler{
		Store:  jobs,
		Runner: &Runner{Agent: &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}, Sessions: session.New(st)},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Parser: cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("cron job did not start")
	}
	cancel() // models SIGTERM
	select {
	case err := <-done:
		t.Fatalf("Scheduler.Run returned before accepted cron work drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(p.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Scheduler.Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Scheduler.Run did not return after cron job drained")
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestParseTarget(t *testing.T) {
	if ch, id, ok := ParseTarget("telegram:900"); !ok || ch != "telegram" || id != "900" {
		t.Errorf("ParseTarget = %q %q %v", ch, id, ok)
	}
	if _, _, ok := ParseTarget("notarget"); ok {
		t.Error("bare string parsed as target")
	}
	if _, _, ok := ParseTarget(""); ok {
		t.Error("empty parsed as target")
	}
}
