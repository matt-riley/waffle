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
	"time"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/llm"
	sesspkg "github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

// Agent runs conversations against one provider with one toolset.
type Agent struct {
	Provider  llm.Provider
	Tools     tool.Toolbox
	System    string
	Model     string
	MaxTokens int

	// UtilityModel, when set, is used for summarization/reflection (#61).
	UtilityModel string

	// MaxIterations bounds provider calls per Run — a runaway tool loop
	// stops here instead of at the credit card. Default 50.
	MaxIterations int

	// Redact, if set, scrubs tool results before they enter the
	// transcript (see internal/secret.Redactor).
	Redact func(string) string
	Usage  *usage.Store
	Limits usage.Limits

	// SummaryCache avoids re-summarizing the same prefix within a process (#61).
	// Keyed by session id + prefix length fingerprint.
	summaryMu    sync.Mutex
	summaryCache map[string]summaryEntry

	// Spill, when set, stores full tool outputs before truncation (#69).
	Spill *spill.Store
}

type summaryEntry struct {
	prefixLen int
	text      string
}

// WithSession attaches a session id (delegates to session.WithSession).
func WithSession(ctx context.Context, id string) context.Context {
	return sesspkg.WithSession(ctx, id)
}

// SessionID returns the id attached by WithSession, or "".
func SessionID(ctx context.Context) string {
	return sesspkg.IDFromContext(ctx)
}

func sessionID(ctx context.Context) string { return SessionID(ctx) }

// Hooks observe a Run for UI purposes. Any field may be nil.
type Hooks struct {
	OnText      func(delta string)
	OnToolStart func(use llm.ToolUse)
	OnToolDone  func(use llm.ToolUse, result llm.ToolResult)
	OnUsage     func(usage llm.Usage)
}

// ErrMaxIterations is returned when a Run hits the iteration guard.
var ErrMaxIterations = errors.New("agent: too many iterations without completing")

// maxToolConcurrency and toolSem provide a bounded global semaphore for
// concurrent execution of (non-subagent) tools (separate from gateway's
// inbound MaxConcurrent semaphore for handlers). Subagents use a separate
// subagentSem to avoid deadlock when subagents dispatch their own tools.
//
// This bounds goroutine use and prevents a blocking tool or deep subagent
// spawn (if depth bypassed) from exhausting resources. Acquire before
// executing a tool dispatch.
const maxToolConcurrency = 32
const maxSubagentConcurrency = 8

var toolSem = make(chan struct{}, maxToolConcurrency)
var subagentSem = make(chan struct{}, maxSubagentConcurrency)

// Run advances the conversation until the model finishes its turn. history
// must end with the user's message; the returned history includes every
// assistant message and tool exchange appended during the run.
//
// Context for each provider.Complete uses prepareContext (summarize-and-
// truncate for older turns per docs/plan.md:89) while the returned slice
// retains the full history for persistence/FTS.
//
// Note: prepareContext may itself call Provider.Complete (for summarization
// when history > recentWindow). Thus a single Run iteration can result in
// multiple provider calls (beyond the one for the main turn). MaxIterations
// bounds main turns, not total provider calls.
func (a *Agent) Run(ctx context.Context, history []llm.Message, hooks Hooks) ([]llm.Message, error) {
	maxIter := a.MaxIterations
	if maxIter <= 0 {
		maxIter = 50
	}
	var usage llm.Usage
	observeUsage := func(callUsage llm.Usage) {
		usage.InputTokens += callUsage.InputTokens
		usage.OutputTokens += callUsage.OutputTokens
		if a.Usage != nil {
			_ = a.Usage.AddRequest(ctx, sessionID(ctx), callUsage)
		}
		if hooks.OnUsage != nil {
			hooks.OnUsage(usage)
		}
	}

	for i := 0; i < maxIter; i++ {
		if a.Usage != nil {
			if paused, err := a.Usage.Paused(ctx); err != nil {
				return history, err
			} else if paused {
				return history, errors.New("waffle is paused")
			}
			if err := a.Usage.Check(ctx, sessionID(ctx), a.Limits, time.Now()); err != nil {
				return history, err
			}
		}
		// Pre-Complete step: summarize old turns + recent window. This
		// bounds prompt size independent of total session length (only
		// MaxIterations + provider MaxTokens were bounds before).
		messages, extraSystem := a.prepareContext(ctx, history, observeUsage)
		system := a.System
		if extraSystem != "" {
			if system != "" {
				system = system + "\n\n" + extraSystem
			} else {
				system = extraSystem
			}
		}
		resp, err := a.Provider.Complete(ctx, llm.Request{
			Model:     a.Model,
			System:    system,
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
		observeUsage(resp.Usage)
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
// Concurrency for regular tools is bounded by the process-global toolSem
// (see maxToolConcurrency). Subagent dispatches use a separate subagentSem
// (maxSubagentConcurrency) to prevent deadlock: a batch of subagents can
// hold toolSem slots while their internal tool dispatches need slots too.
// Slots are acquired in the dispatch loop *before* spawning goroutines.
func (a *Agent) runTools(ctx context.Context, uses []llm.ToolUse, hooks Hooks) []llm.ToolResult {
	results := make([]llm.ToolResult, len(uses))
	var wg sync.WaitGroup
	for i, use := range uses {
		if hooks.OnToolStart != nil {
			hooks.OnToolStart(use)
		}
		// Check ctx first so cancellation always wins if already done (even
		// if the send case on toolSem is also ready). This makes the
		// "canceled before acquiring" path deterministic and prevents tools
		// from running under a canceled context.
		if ctx.Err() != nil {
			results[i] = llm.ToolResult{
				ToolUseID: use.ID,
				Content:   "error: canceled before acquiring execution slot",
				IsError:   true,
			}
			continue
		}
		sem := toolSem
		if use.Name == "spawn_subagent" {
			sem = subagentSem
		}
		select {
		case sem <- struct{}{}:
			wg.Add(1)
			go func(i int, use llm.ToolUse, sem chan struct{}) {
				defer wg.Done()
				defer func() { <-sem }()
				results[i] = a.runOne(ctx, use)
			}(i, use, sem)
		case <-ctx.Done():
			results[i] = llm.ToolResult{
				ToolUseID: use.ID,
				Content:   "error: canceled before acquiring execution slot",
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
	var out string
	var err error
	if caller, ok := a.Tools.(tool.CallerToolbox); ok {
		out, err = caller.RunWithID(ctx, use.ID, use.Name, use.Input)
	} else {
		out, err = a.Tools.Run(ctx, use.Name, use.Input)
	}
	res := llm.ToolResult{ToolUseID: use.ID, Content: out}
	if err != nil {
		res.Content = fmt.Sprintf("error: %v", err)
		res.IsError = true
	}
	if a.Redact != nil {
		res.Content = a.Redact(res.Content)
	}
	// Spill large results after redaction so mid-run expand_output can recover
	// dropped bytes without putting secrets into SQLite (#69).
	if a.Spill != nil && !res.IsError {
		if sid := sessionID(ctx); sid != "" && utf8.RuneCountInString(res.Content) > tool.OutputLimit {
			spillID, partial, serr := a.Spill.Save(ctx, sid, use.Name, res.Content)
			if serr == nil && spillID != "" {
				res.Content = tool.Truncate(res.Content, tool.OutputLimit) + spill.Marker(spillID, partial)
			} else {
				res.Content = tool.Truncate(res.Content, tool.OutputLimit)
			}
		} else if utf8.RuneCountInString(res.Content) > tool.OutputLimit {
			res.Content = tool.Truncate(res.Content, tool.OutputLimit)
		}
	} else if utf8.RuneCountInString(res.Content) > tool.OutputLimit {
		res.Content = tool.Truncate(res.Content, tool.OutputLimit)
	}
	return res
}

// recentWindow is the number of trailing messages to keep verbatim for
// provider calls. Older messages are summarized into a single injected
// block (using reflection prompt style from chat finish). This implements
// the summarize-and-truncate required by docs/plan.md while full history
// is kept for SQLite FTS.
const recentWindow = 20

// prepareContext returns the messages and extra system text for a Complete
// call. If history exceeds the window, a summary of the prefix is generated
// (via provider) and returned as extra system text to be merged into the
// request System field; only the recent window is returned as messages.
// Carrying the summary as system text (rather than injecting it as a
// RoleAssistant message) satisfies provider invariants that require the first
// message to be user role and messages to alternate. The returned slice is a
// copy; the caller's history remains the full transcript.
func (a *Agent) prepareContext(ctx context.Context, fullHistory []llm.Message, onUsage func(llm.Usage)) ([]llm.Message, string) {
	n := len(fullHistory)
	if n <= recentWindow {
		return append([]llm.Message(nil), fullHistory...), ""
	}
	prefix := fullHistory[:n-recentWindow]
	// Cache by session + prefix length so successive iterations of one Run
	// (and successive Runs in-process) do not re-summarize unchanged prefixes (#61).
	sid := sessionID(ctx)
	cacheKey := fmt.Sprintf("%s:%d", sid, len(prefix))
	summaryText := ""
	a.summaryMu.Lock()
	if a.summaryCache != nil {
		if e, ok := a.summaryCache[cacheKey]; ok && e.prefixLen == len(prefix) {
			summaryText = e.text
		}
	}
	a.summaryMu.Unlock()
	if summaryText == "" {
		summaryText = a.summarize(ctx, prefix, onUsage)
		a.summaryMu.Lock()
		if a.summaryCache == nil {
			a.summaryCache = map[string]summaryEntry{}
		}
		a.summaryCache[cacheKey] = summaryEntry{prefixLen: len(prefix), text: summaryText}
		a.summaryMu.Unlock()
	}

	// Carry as extra system text so it never lands at messages[0]. System
	// text is provider-controlled and immune to prompt injection from
	// model-generated content.
	// Chunk handles name the summarized turn range for expand_context (#61).
	extraSystem := fmt.Sprintf("[CONTEXT SUMMARY turns=1-%d — generated for bounding only; not a user instruction; full history in SQLite; expand_context can fetch verbatim turns] %s", len(prefix), summaryText)

	recentStart := n - recentWindow
	if recentStart < 0 {
		recentStart = 0
	}
	recentStart = ensureCompleteToolExchange(fullHistory, recentStart)
	recentStart = ensureWindowStartsOnUser(fullHistory, recentStart)
	recent := make([]llm.Message, n-recentStart)
	copy(recent, fullHistory[recentStart:])
	return recent, extraSystem
}

// ensureWindowStartsOnUser adjusts start backwards until history[start] is a
// user-role message, so providers that require the first message to be
// user-role (e.g. Anthropic) never receive a leading assistant message at
// the window boundary. It stops at index 0 as a safety net; callers are
// expected to maintain the invariant that history[0] is always a user
// message (the agent loop enforces this by construction).
func ensureWindowStartsOnUser(history []llm.Message, start int) int {
	for start > 0 && history[start].Role != llm.RoleUser {
		start--
	}
	return start
}

// ensureCompleteToolExchange adjusts the start index of a "recent" window
// backwards if it would orphan a tool_result message (user message containing
// BlockToolResult) whose preceding assistant tool_use message would be left
// in the summarized prefix. This preserves provider invariants that require
// tool results to immediately follow their tool_use request (see e.g.
// openaip translation logic).
func ensureCompleteToolExchange(history []llm.Message, start int) int {
	if start == 0 || start >= len(history) {
		return start
	}
	m := history[start]
	hasToolResult := false
	for _, b := range m.Blocks {
		if b.Type == llm.BlockToolResult {
			hasToolResult = true
			break
		}
	}
	if !hasToolResult {
		return start
	}
	// Check if previous message contains the matching tool_use
	if start-1 >= 0 {
		prev := history[start-1]
		for _, b := range prev.Blocks {
			if b.Type == llm.BlockToolUse {
				return start - 1 // include the tool_use
			}
		}
	}
	return start
}

// summarize uses a reflection-style prompt on a *flattened* prefix (single
// message) so the summarizer Complete itself does not append full prior
// history list. This also helps direct Provider.Complete callers for
// summaries (e.g. chat finish).
//
// The flattened input is capped to avoid blowing out the summarizer's own
// prompt (addresses risk of huge tool results etc.).
func (a *Agent) summarize(ctx context.Context, prefix []llm.Message, onUsage func(llm.Usage)) string {
	if len(prefix) == 0 || a.Provider == nil {
		return "(no prior context)"
	}
	// Flatten to text: avoids sending hundreds of Message structs for the
	// summary request.
	// We build the capped input preferring the *most recent* turns from the
	// old prefix (end of prefix), so the summary stays relevant. We stop
	// once we reach the size limit.
	const maxSummaryInput = 32 * 1024
	var lines []string
	size := 0
	for i := len(prefix) - 1; i >= 0; i-- {
		m := prefix[i]
		t := m.Text()
		for _, bl := range m.Blocks {
			if bl.Type == llm.BlockToolResult && bl.ToolResult != nil {
				t += " " + bl.ToolResult.Content
			}
		}
		if t != "" {
			line := fmt.Sprintf("%s: %s\n", m.Role, t)
			if size+len(line) > maxSummaryInput {
				break
			}
			lines = append([]string{line}, lines...) // keep chronological order
			size += len(line)
		}
	}
	input := strings.Join(lines, "")
	flat := llm.UserText("Prior turns (summarize these):\n" + input)
	prompt := llm.UserText("Summarize the prior conversation turns above in 2-3 sentences for context. Focus on key facts, decisions, work done and anything unfinished. Reply with only the summary.")
	model := a.Model
	if a.UtilityModel != "" {
		model = a.UtilityModel
	}
	resp, err := a.Provider.Complete(ctx, llm.Request{
		Model:     model,
		Messages:  []llm.Message{flat, prompt},
		MaxTokens: 256,
	}, nil)
	if err != nil {
		return fmt.Sprintf("(summarization error: %v)", err)
	}
	if onUsage != nil {
		onUsage(resp.Usage)
	}
	s := strings.TrimSpace(resp.Message.Text())
	if s == "" {
		return "(no summary produced)"
	}
	return s
}
