package schedule

import (
	"bytes"
	"context"
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

// echoProvider replies with the user's text, verbatim.
type echoProvider struct{}

func (echoProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].Text()
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done: " + last}}},
		StopReason: llm.StopEndTurn,
	}, nil
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
	turns, _ := sessions.Turns(ctx, list[0].ID)
	if len(turns) != 2 {
		t.Errorf("turns = %d, want 2", len(turns))
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
