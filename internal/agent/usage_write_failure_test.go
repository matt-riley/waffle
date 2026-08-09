package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

// cancelOnCompleteProvider cancels the run context the moment it is asked to
// Complete, then still returns a valid end-turn response. The agent's usage
// write then runs against the canceled context, which fails — isolating the
// AddRequest error path from Check/Paused (#292).
type cancelOnCompleteProvider struct {
	cancel func()
}

func (p cancelOnCompleteProvider) Complete(_ context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.cancel()
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}},
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 3, OutputTokens: 5},
	}, nil
}

func TestAgentFailsClosedWhenUsageWriteFailsWithLimits(t *testing.T) {
	u := openUsageStore(t)
	ctx := WithSession(context.Background(), "parent-session")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := &Agent{
		Provider: cancelOnCompleteProvider{cancel: cancel},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		Usage:    u,
		Limits:   usage.Limits{TokensPerDay: 1_000_000},
	}
	_, err := a.Run(ctx, []llm.Message{llm.UserText("hi")}, Hooks{})
	if err == nil || !strings.Contains(err.Error(), "record usage") {
		t.Fatalf("Run error = %v, want the usage write failure propagated (fail closed)", err)
	}
}

func TestAgentLogsUsageWriteFailureWithoutLimits(t *testing.T) {
	u := openUsageStore(t)
	ctx := WithSession(context.Background(), "parent-session")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logs bytes.Buffer
	a := &Agent{
		Provider: cancelOnCompleteProvider{cancel: cancel},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		Usage:    u,
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
	}
	_, err := a.Run(ctx, []llm.Message{llm.UserText("hi")}, Hooks{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(logs.String(), "usage write failed") {
		t.Fatalf("missing usage-failure log: %s", logs.String())
	}
}
