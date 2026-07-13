package schedule

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRunnerExecutesAndDelivers(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sessions := session.New(st)
	cap := &captureDeliverer{}
	runner := &Runner{
		Agent:     &agent.Agent{Provider: echoProvider{}, Tools: tool.NewRegistry(), Model: "m"},
		Sessions:  sessions,
		Deliverer: cap,
	}

	reply, err := runner.Run(ctx, Job{Name: "test", Prompt: "check the thing", Deliver: "telegram:900"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "done: check the thing" {
		t.Errorf("reply = %q", reply)
	}
	if cap.target != "telegram:900" || cap.text != reply {
		t.Errorf("delivered %q to %q", cap.text, cap.target)
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

func TestRunnerReservedLearnJobDeliversDigestWithoutAgentDispatch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	cap := &captureDeliverer{}
	p := &recordingProvider{}
	runner := &Runner{
		Agent:     &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"},
		Sessions:  session.New(st),
		Deliverer: cap,
		Learn: func(context.Context) (string, error) {
			return "waffle learn digest\npatterns=2 accepted=1", nil
		},
	}
	reply, err := runner.Run(ctx, Job{Name: "learn-daily", Prompt: "/learn", Deliver: "telegram:900"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != cap.text || cap.target != "telegram:900" || !strings.Contains(reply, "learn digest") {
		t.Fatalf("reply=%q delivered=%q target=%q", reply, cap.text, cap.target)
	}
	if len(p.reqs) != 0 {
		t.Fatalf("reserved learn job reached agent provider: %d calls", len(p.reqs))
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
