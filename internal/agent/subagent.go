package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// SubagentTool spawns a fresh agent for an isolated task and returns its
// final answer (docs/plan.md, "subagents ... reporting back to a parent").
// The subagent shares the parent's provider, model, and toolbox but starts
// with a clean history, so it doesn't inherit or pollute the conversation.
//
// Tool dispatch (including subagent spawning) is concurrent via goroutines
// but bounded by the process-global package-level toolSem in the agent
// package (independent by contract, see Tool docs). Depth is
// belt-and-suspenders; sub-toolbox normally omits spawn_subagent.
type SubagentTool struct {
	Provider  llm.Provider
	Tools     tool.Toolbox
	Model     string
	MaxTokens int
	Redact    func(string) string
	// Depth guards against runaway recursion; a subagent's toolbox should
	// omit spawn_subagent (enforced by Restrict in buildAgent), but this is
	// the belt-and-suspenders bound. Execution slots are also bounded.
	Depth int
}

func (t SubagentTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate a self-contained subtask to a fresh agent with its own context, and get back its final answer. Use for parallel or independent work (research a topic, analyze a file) — issue several in one turn to run them concurrently. State the task fully; the subagent sees none of this conversation.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task": {"type": "string", "description": "The complete, self-contained instruction for the subagent"}
			},
			"required": ["task"]
		}`),
	}
}

func (t SubagentTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Depth >= 3 {
		return "", fmt.Errorf("subagent depth limit reached")
	}
	var in struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}

	sub := &Agent{
		Provider:      t.Provider,
		Tools:         t.Tools,
		System:        "You are a waffle subagent handling one self-contained task. Do the work with your tools and end with a concise report of what you found or did. You have no access to the parent conversation.",
		Model:         t.Model,
		MaxTokens:     t.MaxTokens,
		Redact:        t.Redact,
		MaxIterations: 30,
	}
	history, err := sub.Run(ctx, []llm.Message{llm.UserText(in.Task)}, Hooks{})
	if err != nil {
		return "", err
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleAssistant {
			return history[i].Text(), nil
		}
	}
	return "(subagent produced no answer)", nil
}
