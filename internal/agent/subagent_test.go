package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// oneShotProvider answers every request with fixed text and ends the turn.
type oneShotProvider struct{ reply string }

func (p oneShotProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	// Confirm the subagent starts with a clean, single-message history
	// (main turn) or a single repair prompt.
	text := ""
	if len(req.Messages) > 0 {
		text = req.Messages[len(req.Messages)-1].Text()
	}
	// Always emit a valid handoff so legacy tests stay stable after #78.
	body := fmt.Sprintf("```json\n{\"status\":\"done\",\"summary\":%q}\n```", p.reply+": "+text)
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: body}}},
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
	if !strings.Contains(out, "research the topic") || !strings.Contains(out, "done") {
		t.Errorf("out = %q", out)
	}
}

func TestSubagentDepthLimit(t *testing.T) {
	tl := SubagentTool{Provider: oneShotProvider{}, Tools: tool.NewRegistry(), Model: "m", Depth: 3}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"x"}`)); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Errorf("err = %v, want depth limit", err)
	}
}

// captureSystemProvider records the system prompt.
type captureSystemProvider struct {
	system string
}

func (p *captureSystemProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.system = req.System
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: `{"status":"done","summary":"ok"}`}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func TestSubagentWorkingSetBroadcast(t *testing.T) {
	p := &captureSystemProvider{}
	tl := SubagentTool{
		Provider:            p,
		Tools:               tool.NewRegistry(),
		Model:               "m",
		BroadcastWorkingSet: true,
		WorkingSetBroadcast: "<working_set>\n- [goal id=g1 source=user] ship it\n</working_set>\n",
	}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"do work"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.system, "working_set") || !strings.Contains(p.system, "ship it") {
		t.Fatalf("system missing broadcast: %q", p.system)
	}
	if !strings.Contains(p.system, "read-only") {
		t.Fatalf("system missing read-only note: %q", p.system)
	}
}
