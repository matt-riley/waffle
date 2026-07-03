package schedule

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/llm"
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
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
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
