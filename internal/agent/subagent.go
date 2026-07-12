package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/workset"
)

// SubagentTool spawns a fresh agent for an isolated task and returns its
// final answer (docs/plan.md, "subagents ... reporting back to a parent").
// The subagent shares the parent's provider, model, and toolbox but starts
// with a clean history, so it doesn't inherit or pollute the conversation.
//
// Tool dispatch (including subagent spawning) is concurrent via goroutines
// but bounded (regular tools via toolSem, subagent dispatches via separate
// subagentSem to avoid deadlock on nested tool use). See agent package.
// Depth is belt-and-suspenders; sub-toolbox normally omits spawn_subagent.
//
// Nested spawn_subagent is omitted from child toolboxes. If that ever
// changes, children must inherit the same read-only working-set broadcast
// snapshot — never a widened mutation authority (#68).
type SubagentTool struct {
	Provider  llm.Provider
	Tools     tool.Toolbox
	Model     string
	MaxTokens int
	Redact    func(string) string
	// Depth guards against runaway recursion; a subagent's toolbox is
	// constructed in buildAgent without including SubagentTool (Restrict
	// only applies allow/deny policy to the tools that *are* present).
	// This is the belt-and-suspenders bound. Execution slots are also bounded.
	Depth int
	// WorkingSetBroadcast is optional rendered parent working set (#68).
	// Empty means no broadcast (byte-identical legacy system prompt).
	WorkingSetBroadcast string
	// BroadcastWorkingSet can force-disable even when WorkingSetBroadcast is set.
	BroadcastWorkingSet bool
	// Spill is optional tool-output spill store for the child agent (#69).
	Spill *spill.Store
}

func (t SubagentTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate a self-contained subtask to a fresh agent with its own context, and get back a structured handoff. Prefer a full work packet (task + optional owned_paths, acceptance_criteria, verify_commands). Legacy {\"task\":\"...\"} still works. Optional profile selects a named agent profile when configured.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task": {"type": "string", "description": "The complete, self-contained instruction for the subagent"},
				"role": {"type": "string"},
				"context_refs": {"type": "array", "items": {"type": "string"}},
				"owned_paths": {"type": "array", "items": {"type": "string"}},
				"acceptance_criteria": {"type": "array", "items": {"type": "string"}},
				"verify_commands": {"type": "array", "items": {"type": "string"}},
				"read_only": {"type": "boolean"},
				"profile": {"type": "string", "description": "optional named agent profile (#71)"}
			},
			"required": ["task"]
		}`),
	}
}

func (t SubagentTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Depth >= 3 {
		return "", fmt.Errorf("subagent depth limit reached")
	}
	var p WorkPacket
	if err := json.Unmarshal(input, &p); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(p.Task) == "" {
		return "", fmt.Errorf("task is required")
	}

	sys := "You are a waffle subagent handling one self-contained task. Do the work with your tools and end with a concise report of what you found or did. You have no access to the parent conversation."
	if t.BroadcastWorkingSet && t.WorkingSetBroadcast != "" {
		sys += "\n\n" + t.WorkingSetBroadcast
		sys += "\nThe working set above is read-only. To suggest changes, include proposals in your JSON handoff; they are NOT applied automatically."
	}
	sys += "\n\n" + FramePacket(p)

	// Subagent toolboxes must never include workspace_update (#68).
	childTools := t.Tools
	if childTools != nil {
		childTools = tool.Restrict(childTools, tool.Policy{Deny: []string{"workspace_update", "spawn_subagent"}})
	}

	sub := &Agent{
		Provider:      t.Provider,
		Tools:         childTools,
		System:        sys,
		Model:         t.Model,
		MaxTokens:     t.MaxTokens,
		Redact:        t.Redact,
		Spill:         t.Spill,
		MaxIterations: 30,
	}
	history, err := sub.Run(ctx, []llm.Message{llm.UserText(p.Task)}, Hooks{})
	if err != nil {
		return FormatHandoffResult(Handoff{Status: "failed", Summary: err.Error()}), nil
	}
	var text string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleAssistant {
			text = history[i].Text()
			break
		}
	}
	if text == "" {
		return FormatHandoffResult(Handoff{Status: "failed", Summary: "subagent produced no answer"}), nil
	}
	h, err := ParseHandoff(text)
	if err != nil {
		// One repair attempt: ask model is not available here; treat prose as partial.
		h = Handoff{Status: "partial", Summary: text, Reasons: []string{"malformed handoff; treated as prose summary"}}
	} else {
		h = NormalizeHandoff(h, p)
	}
	// Ensure proposals cannot be confused with applied state.
	_ = workset.Proposal{} // keep import if proposals empty
	return FormatHandoffResult(h), nil
}
