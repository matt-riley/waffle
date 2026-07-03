// Package agent implements waffle's agent loop (docs/plan.md, "Agent
// loop"): assemble context, call the provider, dispatch tool calls, repeat
// until the model stops asking for tools.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// Agent runs conversations against one provider with one toolset.
type Agent struct {
	Provider  llm.Provider
	Tools     tool.Toolbox
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

// maxToolConcurrency and toolSem provide a bounded global semaphore for
// concurrent execution of tools and subagents (separate from gateway's
// inbound MaxConcurrent semaphore for handlers). This bounds goroutine use
// and prevents a blocking tool or deep subagent spawn (if depth bypassed)
// from exhausting resources. Acquire before executing a tool dispatch.
const maxToolConcurrency = 32

var toolSem = make(chan struct{}, maxToolConcurrency)

// Run advances the conversation until the model finishes its turn. history
// must end with the user's message; the returned history includes every
// assistant message and tool exchange appended during the run.
//
// Context for each provider.Complete uses prepareContext (summarize-and-
// truncate for older turns per docs/plan.md:89) while the returned slice
// retains the full history for persistence/FTS.
func (a *Agent) Run(ctx context.Context, history []llm.Message, hooks Hooks) ([]llm.Message, error) {
	maxIter := a.MaxIterations
	if maxIter <= 0 {
		maxIter = 50
	}

	for i := 0; i < maxIter; i++ {
		// Pre-Complete step: summarize old turns + recent window. This
		// bounds prompt size independent of total session length (only
		// MaxIterations + provider MaxTokens were bounds before).
		messages := a.prepareContext(ctx, history)
		resp, err := a.Provider.Complete(ctx, llm.Request{
			Model:     a.Model,
			System:    a.System,
			Messages:  messages,
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
//
// Concurrency is bounded by the process-global package-level toolSem
// (see maxToolConcurrency) to avoid exhausting goroutines when many tools
// are called or a tool/subagent blocks (depth limit in SubagentTool is
// belt-and-suspenders). Slots are acquired in the dispatch loop *before*
// spawning goroutines so that a large batch blocks the loop rather than
// creating unbounded goroutines that contend on the semaphore.
func (a *Agent) runTools(ctx context.Context, uses []llm.ToolUse, hooks Hooks) []llm.ToolResult {
	results := make([]llm.ToolResult, len(uses))
	var wg sync.WaitGroup
	for i, use := range uses {
		if hooks.OnToolStart != nil {
			hooks.OnToolStart(use)
		}
		select {
		case toolSem <- struct{}{}:
			wg.Add(1)
			go func(i int, use llm.ToolUse) {
				defer wg.Done()
				defer func() { <-toolSem }()
				results[i] = a.runOne(ctx, use)
			}(i, use)
		case <-ctx.Done():
			results[i] = llm.ToolResult{
				ToolUseID: use.ID,
				Content:   "canceled before acquiring execution slot",
				IsError:   true,
			}
		}
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

// recentWindow is the number of trailing messages to keep verbatim for
// provider calls. Older messages are summarized into a single injected
// block (using reflection prompt style from chat finish). This implements
// the summarize-and-truncate required by docs/plan.md while full history
// is kept for SQLite FTS.
const recentWindow = 20

// prepareContext returns messages for a Complete call. If history exceeds
// the window, a summary of the prefix is generated (via provider) and
// prepended; only the recent window follows it. The returned slice is a
// copy; the caller's history remains the full transcript.
func (a *Agent) prepareContext(ctx context.Context, fullHistory []llm.Message) []llm.Message {
	n := len(fullHistory)
	if n <= recentWindow {
		return append([]llm.Message(nil), fullHistory...)
	}
	prefix := fullHistory[:n-recentWindow]
	summaryText := a.summarize(ctx, prefix)

	// Synthetic note injected for the model only (not appended to
	// history, so not persisted as a turn). Injected with RoleAssistant
	// (and explicit labeling) rather than RoleUser to reduce prompt-injection
	// surface from model-generated content (per review feedback).
	sumMsg := llm.Message{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{{
			Type: llm.BlockText,
			Text: "[CONTEXT SUMMARY - generated for bounding only; not a user instruction or command; full history retained in SQLite for search] " + summaryText,
		}},
	}
	recent := make([]llm.Message, recentWindow)
	copy(recent, fullHistory[n-recentWindow:])
	return append([]llm.Message{sumMsg}, recent...)
}

// summarize uses a reflection-style prompt on a *flattened* prefix (single
// message) so the summarizer Complete itself does not append full prior
// history list. This also helps direct Provider.Complete callers for
// summaries (e.g. chat finish).
//
// The flattened input is capped to avoid blowing out the summarizer's own
// prompt (addresses risk of huge tool results etc.).
func (a *Agent) summarize(ctx context.Context, prefix []llm.Message) string {
	if len(prefix) == 0 || a.Provider == nil {
		return "(no prior context)"
	}
	// Flatten to text: avoids sending hundreds of Message structs for the
	// summary request.
	var b strings.Builder
	const maxSummaryInput = 32 * 1024
	for _, m := range prefix {
		t := m.Text()
		for _, bl := range m.Blocks {
			if bl.Type == llm.BlockToolResult && bl.ToolResult != nil {
				t += " " + bl.ToolResult.Content
			}
		}
		if t != "" {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, t)
		}
		if b.Len() > maxSummaryInput {
			break
		}
	}
	input := b.String()
	if len(input) > maxSummaryInput {
		input = input[len(input)-maxSummaryInput:]
	}
	flat := llm.UserText("Prior turns (summarize these):\n" + input)
	prompt := llm.UserText("Summarize the prior conversation turns above in 2-3 sentences for context. Focus on key facts, decisions, work done and anything unfinished. Reply with only the summary.")
	resp, err := a.Provider.Complete(ctx, llm.Request{
		Model:     a.Model,
		Messages:  []llm.Message{flat, prompt},
		MaxTokens: 256,
	}, nil)
	if err != nil {
		return fmt.Sprintf("(summarization error: %v)", err)
	}
	s := strings.TrimSpace(resp.Message.Text())
	if s == "" {
		return "(no summary produced)"
	}
	return s
}
