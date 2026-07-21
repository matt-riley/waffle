package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

// fixedUsageProvider returns a successful handoff with fixed token usage.
type fixedUsageProvider struct {
	reply string
	usage llm.Usage
}

func (p fixedUsageProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	body := "```json\n{\"status\":\"done\",\"summary\":\"" + p.reply + "\"}\n```"
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: body}}},
		StopReason: llm.StopEndTurn,
		Usage:      p.usage,
	}, nil
}

func openUsageStore(t *testing.T) *usage.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return usage.New(st)
}

func dayTokens(t *testing.T, u *usage.Store, session string) (requests, in, out int) {
	t.Helper()
	rows, err := u.List(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Period == "day" {
			requests += r.Requests
			in += r.InputTokens
			out += r.OutputTokens
		}
	}
	return requests, in, out
}

func TestSubagentTightBudgetBlocksRun(t *testing.T) {
	u := openUsageStore(t)
	const parentSession = "parent-budget-block"
	ctx := WithSession(context.Background(), parentSession)

	// Pre-fill past a small daily token budget.
	if err := u.AddRequest(ctx, parentSession, llm.Usage{InputTokens: 10, OutputTokens: 0}); err != nil {
		t.Fatal(err)
	}

	tl := SubagentTool{
		Provider: fixedUsageProvider{reply: "should-not-run", usage: llm.Usage{InputTokens: 5, OutputTokens: 5}},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		Usage:    u,
		Limits:   usage.Limits{TokensPerDay: 10},
	}
	out, err := tl.Run(ctx, json.RawMessage(`{"task":"do work"}`))
	if err != nil {
		t.Fatalf("Run returned error (want failed handoff string): %v", err)
	}
	if !strings.Contains(out, `"status": "failed"`) {
		t.Fatalf("want failed handoff, got %q", out)
	}
	if !strings.Contains(out, "usage limit exceeded") && !strings.Contains(out, "paused") {
		t.Fatalf("summary should reflect budget block, got %q", out)
	}
}

func TestSubagentRecordsParentVisibleUsage(t *testing.T) {
	u := openUsageStore(t)
	const parentSession = "parent-usage-ok"
	ctx := WithSession(context.Background(), parentSession)

	wantIn, wantOut := 11, 17
	tl := SubagentTool{
		Provider: fixedUsageProvider{
			reply: "ok",
			usage: llm.Usage{InputTokens: wantIn, OutputTokens: wantOut},
		},
		Tools:  tool.NewRegistry(),
		Model:  "m",
		Usage:  u,
		Limits: usage.Limits{TokensPerDay: 1_000_000, RequestsPerHour: 1_000},
	}
	out, err := tl.Run(ctx, json.RawMessage(`{"task":"record me"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("out=%q", out)
	}

	requests, in, outTok := dayTokens(t, u, parentSession)
	if requests < 1 {
		t.Fatalf("parent requests=%d, want >= 1", requests)
	}
	if in < wantIn || outTok < wantOut {
		t.Fatalf("parent tokens in=%d out=%d, want at least in=%d out=%d", in, outTok, wantIn, wantOut)
	}
}

func TestSubagentChildSessionChargesParentBudgetKey(t *testing.T) {
	u := openUsageStore(t)
	const parentSession = "parent-with-child"
	ctx := WithSession(context.Background(), parentSession)

	wantIn, wantOut := 3, 4
	tl := SubagentTool{
		Provider: fixedUsageProvider{
			reply: "child",
			usage: llm.Usage{InputTokens: wantIn, OutputTokens: wantOut},
		},
		Tools:  tool.NewRegistry(),
		Model:  "m",
		Usage:  u,
		Limits: usage.Limits{TokensPerDay: 1_000_000},
		NewChildSession: func(ctx context.Context, title string) (string, error) {
			return "child-session-isolated", nil
		},
	}
	if _, err := tl.Run(ctx, json.RawMessage(`{"task":"child work"}`)); err != nil {
		t.Fatal(err)
	}

	// Spend must land on the parent budget key, not the child session id.
	_, in, outTok := dayTokens(t, u, parentSession)
	if in < wantIn || outTok < wantOut {
		t.Fatalf("parent tokens in=%d out=%d, want at least in=%d out=%d", in, outTok, wantIn, wantOut)
	}
	_, childIn, childOut := dayTokens(t, u, "child-session-isolated")
	if childIn != 0 || childOut != 0 {
		t.Fatalf("child session should not hold budget: in=%d out=%d", childIn, childOut)
	}
}

func TestSubagentRepairHandoffRecordsUsage(t *testing.T) {
	u := openUsageStore(t)
	const parentSession = "parent-repair-usage"
	ctx := WithSession(context.Background(), parentSession)

	calls := 0
	p := &recordingProvider{onComplete: func(req llm.Request) llm.Response {
		calls++
		if calls == 1 {
			return llm.Response{
				StopReason: llm.StopEndTurn,
				Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "all done, no json"}}},
				Usage:      llm.Usage{InputTokens: 5, OutputTokens: 7},
			}
		}
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"repaired\"}\n```"},
			}},
			Usage: llm.Usage{InputTokens: 2, OutputTokens: 3},
		}
	}}

	tl := SubagentTool{
		Provider: p,
		Tools:    tool.NewRegistry(),
		Model:    "m",
		Usage:    u,
		Limits:   usage.Limits{TokensPerDay: 1_000_000, RequestsPerHour: 1_000},
	}
	out, err := tl.Run(ctx, json.RawMessage(`{"task":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("expected main + repair Completes, calls=%d", calls)
	}
	if !strings.Contains(out, "repaired") {
		t.Fatalf("out=%q", out)
	}

	requests, in, outTok := dayTokens(t, u, parentSession)
	if requests < 2 {
		t.Fatalf("requests=%d, want >= 2 (main + repair)", requests)
	}
	if in < 5+2 || outTok < 7+3 {
		t.Fatalf("tokens in=%d out=%d, want at least combined main+repair (7/10)", in, outTok)
	}
}
