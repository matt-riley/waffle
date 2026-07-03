package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// oneShotProvider answers every request with fixed text and ends the turn.
type oneShotProvider struct{ reply string }

func (p oneShotProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	// Confirm the subagent starts with a clean, single-message history.
	if len(req.Messages) != 1 {
		return &llm.Response{
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "unexpected history"}}},
			StopReason: llm.StopEndTurn,
		}, nil
	}
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: p.reply + ": " + req.Messages[0].Text()}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func TestSubagentTool(t *testing.T) {
	tl := SubagentTool{
		Provider: oneShotProvider{reply: "subagent"},
		Tools:    tool.NewRegistry(),
		Model:    "m",
	}
	if tl.Def().Name != "spawn_subagent" {
		t.Fatalf("name = %q", tl.Def().Name)
	}

	out, err := tl.Run(context.Background(), json.RawMessage(`{"task":"research the topic"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "subagent: research the topic" {
		t.Errorf("out = %q", out)
	}
}

func TestSubagentDepthLimit(t *testing.T) {
	tl := SubagentTool{Provider: oneShotProvider{}, Tools: tool.NewRegistry(), Model: "m", Depth: 3}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"x"}`)); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Errorf("err = %v, want depth limit", err)
	}
}
