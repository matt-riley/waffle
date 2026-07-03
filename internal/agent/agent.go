// Package agent implements waffle's agent loop (docs/plan.md, "Agent
// loop"): assemble context, call the provider, dispatch tool calls, repeat
// until the model stops asking for tools.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// Agent runs conversations against one provider with one toolset.
type Agent struct {
	Provider  llm.Provider
	Tools     *tool.Registry
	System    string
	Model     string
	MaxTokens int

	// MaxIterations bounds provider calls per Run — a runaway tool loop
	// stops here instead of at the credit card. Default 50.
	MaxIterations int

	// Redact, if set, scrubs tool results before they enter the
	// transcript (see internal/secret.Redactor).
	Redact func(string) string
}

// Hooks observe a Run for UI purposes. Any field may be nil.
type Hooks struct {
	OnText      func(delta string)
	OnToolStart func(use llm.ToolUse)
	OnToolDone  func(use llm.ToolUse, result llm.ToolResult)
}

// ErrMaxIterations is returned when a Run hits the iteration guard.
var ErrMaxIterations = errors.New("agent: too many iterations without completing")

// Run advances the conversation until the model finishes its turn. history
// must end with the user's message; the returned history includes every
// assistant message and tool exchange appended during the run.
func (a *Agent) Run(ctx context.Context, history []llm.Message, hooks Hooks) ([]llm.Message, error) {
	maxIter := a.MaxIterations
	if maxIter <= 0 {
		maxIter = 50
	}

	for i := 0; i < maxIter; i++ {
		resp, err := a.Provider.Complete(ctx, llm.Request{
			Model:     a.Model,
			System:    a.System,
			Messages:  history,
			Tools:     a.Tools.Defs(),
			MaxTokens: a.MaxTokens,
		}, func(e llm.Event) {
			if e.Type == llm.EventTextDelta && hooks.OnText != nil {
				hooks.OnText(e.Text)
			}
		})
		if err != nil {
			return history, err
		}
		history = append(history, resp.Message)

		if resp.StopReason == llm.StopRefusal {
			return history, errors.New("agent: the model declined this request (refusal)")
		}
		uses := resp.ToolUses()
		if resp.StopReason != llm.StopToolUse || len(uses) == 0 {
			return history, nil
		}

		results := a.runTools(ctx, uses, hooks)
		blocks := make([]llm.Block, len(results))
		for j, res := range results {
			blocks[j] = llm.Block{Type: llm.BlockToolResult, ToolResult: &res}
		}
		history = append(history, llm.Message{Role: llm.RoleUser, Blocks: blocks})
	}
	return history, ErrMaxIterations
}

// runTools executes tool calls in parallel (independent by contract) and
// returns results in request order, as the API requires.
func (a *Agent) runTools(ctx context.Context, uses []llm.ToolUse, hooks Hooks) []llm.ToolResult {
	results := make([]llm.ToolResult, len(uses))
	var wg sync.WaitGroup
	for i, use := range uses {
		if hooks.OnToolStart != nil {
			hooks.OnToolStart(use)
		}
		wg.Add(1)
		go func(i int, use llm.ToolUse) {
			defer wg.Done()
			results[i] = a.runOne(ctx, use)
		}(i, use)
	}
	wg.Wait()
	if hooks.OnToolDone != nil {
		for i, use := range uses {
			hooks.OnToolDone(use, results[i])
		}
	}
	return results
}

func (a *Agent) runOne(ctx context.Context, use llm.ToolUse) llm.ToolResult {
	out, err := a.Tools.Run(ctx, use.Name, use.Input)
	res := llm.ToolResult{ToolUseID: use.ID, Content: out}
	if err != nil {
		res.Content = fmt.Sprintf("error: %v", err)
		res.IsError = true
	}
	if a.Redact != nil {
		res.Content = a.Redact(res.Content)
	}
	return res
}
