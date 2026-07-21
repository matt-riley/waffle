package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llmtest"
	policypkg "github.com/matt-riley/waffle/internal/policy"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

// fakeProvider returns scripted responses in order.
type fakeProvider struct {
	responses []llm.Response
	requests  []llm.Request
}

func (f *fakeProvider) Complete(ctx context.Context, req llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return nil, errors.New("fake: out of responses")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	if onEvent != nil {
		for _, b := range resp.Message.Blocks {
			if b.Type == llm.BlockText {
				onEvent(llm.Event{Type: llm.EventTextDelta, Text: b.Text})
			}
		}
	}
	return &resp, nil
}

// echoTool records inputs and echoes them back. Run must be safe for
// concurrent use: the agent dispatches parallel tool calls.
type echoTool struct {
	mu    sync.Mutex
	calls []string
}

func (e *echoTool) Def() llm.Tool {
	return llm.Tool{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)}
}

func (e *echoTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	e.mu.Lock()
	e.calls = append(e.calls, string(input))
	e.mu.Unlock()
	return "echoed:" + string(input), nil
}

func assistantText(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
}

func TestRunPlainAnswer(t *testing.T) {
	// Uses shared llmtest.Script (#63) instead of a local fake provider.
	p := &llmtest.Script{Responses: []llm.Response{llmtest.Text("hello there")}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), System: "sys", Model: "m"}

	var streamed strings.Builder
	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("hi")},
		Hooks{OnText: func(s string) { streamed.WriteString(s) }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
	if got := history[1].Text(); got != "hello there" {
		t.Errorf("assistant text = %q", got)
	}
	if streamed.String() != "hello there" {
		t.Errorf("streamed = %q", streamed.String())
	}
	if len(p.Requests) == 0 || p.Requests[0].System != "sys" {
		t.Errorf("system not passed through")
	}
}

func TestProfileStructuredLogsProviderToolAndDenialWithoutInput(t *testing.T) {
	var logs bytes.Buffer
	p := &llmtest.Script{Responses: []llm.Response{
		llmtest.ToolCall("bash", "denied", `{"command":"echo SECRET_PROMPT"}`),
		llmtest.Text("done"),
	}}
	a := &Agent{Provider: p, Tools: tool.Restrict(tool.NewRegistry(namedToolForLog{}), tool.Policy{Deny: []string{"bash"}}), Model: "m", Profile: "reviewer", Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if _, err := a.Run(context.Background(), []llm.Message{llm.UserText("PRIVATE_PROMPT")}, Hooks{}); err != nil {
		t.Fatal(err)
	}
	body := logs.String()
	for _, want := range []string{"msg=\"provider call\"", "msg=\"tool call started\"", "msg=\"tool call denied\"", "profile=reviewer", "tool=bash"} {
		if !strings.Contains(body, want) {
			t.Fatalf("logs missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "PRIVATE_PROMPT") || strings.Contains(body, "SECRET_PROMPT") {
		t.Fatalf("prompt/tool input leaked into logs: %s", body)
	}
}

func TestProfileDenialLogsEffectivePolicySourceAndRuleWithoutInput(t *testing.T) {
	tests := []struct {
		name       string
		policy     tool.Policy
		wantSource string
		wantRule   string
	}{
		{
			name:       "generic tool policy",
			policy:     tool.Policy{Deny: []string{"bash"}, Profile: "reviewer"},
			wantSource: "tool_policy",
			wantRule:   "deny",
		},
		{
			name: "action rule",
			policy: tool.Policy{Profile: "reviewer", CheckAction: func(_ context.Context, name string, input json.RawMessage) error {
				engine, err := policypkg.NewEngine([]policypkg.Rule{{Name: "no-private-bash", Tool: "bash", Action: policypkg.ActionDeny}}, policypkg.EnforcerNone)
				if err != nil {
					return err
				}
				decision := engine.Check(name, input)
				if decision.Allowed {
					return nil
				}
				return tool.NewPolicyDenial("reviewer", "policy.rule", decision.Rule, decision.Message)
			}},
			wantSource: "policy.rule",
			wantRule:   "no-private-bash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			p := &llmtest.Script{Responses: []llm.Response{
				llmtest.ToolCall("bash", "denied", `{"command":"echo SECRET_TOOL_INPUT"}`),
				llmtest.Text("done"),
			}}
			a := &Agent{
				Provider: p,
				Tools:    tool.Restrict(tool.NewRegistry(namedToolForLog{}), tt.policy),
				Model:    "m",
				Profile:  "reviewer",
				Log:      slog.New(slog.NewTextHandler(&logs, nil)),
			}
			history, err := a.Run(context.Background(), []llm.Message{llm.UserText("PRIVATE_PROMPT")}, Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			body := logs.String()
			for _, want := range []string{`msg="tool call denied"`, "profile=reviewer", "policy_source=" + tt.wantSource, "rule=" + tt.wantRule} {
				if !strings.Contains(body, want) {
					t.Fatalf("logs missing %q: %s", want, body)
				}
			}
			if strings.Contains(body, "PRIVATE_PROMPT") || strings.Contains(body, "SECRET_TOOL_INPUT") {
				t.Fatalf("prompt/tool input leaked into logs: %s", body)
			}
			denial := history[2].Blocks[0].ToolResult.Content
			for _, want := range []string{`profile "reviewer"`, `policy source "` + tt.wantSource + `"`, `rule "` + tt.wantRule + `"`} {
				if !strings.Contains(denial, want) {
					t.Fatalf("denial missing %q: %s", want, denial)
				}
			}
			if strings.Contains(denial, "SECRET_TOOL_INPUT") {
				t.Fatalf("tool input leaked into denial: %s", denial)
			}
		})
	}
}

type namedToolForLog struct{}

func (namedToolForLog) Def() llm.Tool {
	return llm.Tool{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (namedToolForLog) Run(context.Context, json.RawMessage) (string, error) { return "ran", nil }

func TestRunToolLoop(t *testing.T) {
	echo := &echoTool{}
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "let me check"},
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "echo", Input: json.RawMessage(`{"text":"a"}`)}},
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t2", Name: "echo", Input: json.RawMessage(`{"text":"b"}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("done"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(echo), Model: "m"}

	var started, finished int
	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{
		OnToolStart: func(llm.ToolUse) { started++ },
		OnToolDone:  func(llm.ToolUse, llm.ToolResult) { finished++ },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// user, assistant(tool_use), user(tool_results), assistant(done)
	if len(history) != 4 {
		t.Fatalf("history len = %d, want 4", len(history))
	}
	if started != 2 || finished != 2 {
		t.Errorf("hooks: started=%d finished=%d, want 2/2", started, finished)
	}
	if len(echo.calls) != 2 {
		t.Fatalf("tool ran %d times, want 2", len(echo.calls))
	}

	// Tool results must be a single user message, ordered by request.
	results := history[2]
	if results.Role != llm.RoleUser || len(results.Blocks) != 2 {
		t.Fatalf("tool results message malformed: %+v", results)
	}
	if results.Blocks[0].ToolResult.ToolUseID != "t1" || results.Blocks[1].ToolResult.ToolUseID != "t2" {
		t.Errorf("tool results out of order: %+v", results.Blocks)
	}
	if !strings.Contains(results.Blocks[0].ToolResult.Content, `"a"`) {
		t.Errorf("result content = %q", results.Blocks[0].ToolResult.Content)
	}
}

func TestRunReportsCumulativeUsageAfterEachProviderResponse(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "echo", Input: json.RawMessage(`{"text":"a"}`)}},
			}},
			StopReason: llm.StopToolUse,
			Usage:      llm.Usage{InputTokens: 3, OutputTokens: 5},
		},
		{
			Message:    assistantText("done"),
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{InputTokens: 7, OutputTokens: 11},
		},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(&echoTool{}), Model: "m"}

	var observations []llm.Usage
	_, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{
		OnUsage: func(usage llm.Usage) { observations = append(observations, usage) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []llm.Usage{{InputTokens: 3, OutputTokens: 5}, {InputTokens: 10, OutputTokens: 16}}
	if !slices.Equal(observations, want) {
		t.Errorf("usage observations = %#v, want %#v", observations, want)
	}
}

func TestRunUnknownToolBecomesErrorResult(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "nope", Input: json.RawMessage(`{}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("recovered"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}

	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := history[2].Blocks[0].ToolResult
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Errorf("expected unknown-tool error result, got %+v", res)
	}
}

func TestRunRedactsToolResults(t *testing.T) {
	echo := &echoTool{}
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "echo", Input: json.RawMessage(`{"text":"sk-secret-value"}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{
		Provider: p, Tools: tool.NewRegistry(echo), Model: "m",
		Redact: func(s string) string { return strings.ReplaceAll(s, "sk-secret-value", "[redacted]") },
	}
	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	content := history[2].Blocks[0].ToolResult.Content
	if strings.Contains(content, "sk-secret-value") {
		t.Errorf("secret leaked into transcript: %q", content)
	}
}

func TestRunIterationGuard(t *testing.T) {
	loop := llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}},
		}},
		StopReason: llm.StopToolUse,
	}
	p := &fakeProvider{}
	for i := 0; i < 10; i++ {
		p.responses = append(p.responses, loop)
	}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(&echoTool{}), Model: "m", MaxIterations: 3}

	_, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{})
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if len(p.requests) != 3 {
		t.Errorf("provider called %d times, want 3", len(p.requests))
	}
}

// TestRunSummarizeAndTruncate verifies that prepareContext injects a
// summary for long histories (Issue 3) and keeps Messages bounded in
// Complete calls, while full history is still returned.
func TestRunSummarizeAndTruncate(t *testing.T) {
	// Provide responses: first for the summarize() Complete inside
	// prepare, second for the main agent Complete.
	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("old work: planned then coded"), StopReason: llm.StopEndTurn},
		{Message: assistantText("ok with recent"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}

	// Build history > recentWindow (20). Only last 20 +1 summary go to main.
	longHist := make([]llm.Message, 25)
	for i := 0; i < 25; i++ {
		longHist[i] = llm.UserText(fmt.Sprintf("turn %d", i))
	}
	// The last must be the "user" that Run expects to start from; but
	// since we pass many, Run will treat whole as prior+current? In
	// practice caller appends the latest user. For test, append final.
	hist := append(longHist, llm.UserText("current question"))

	history, err := a.Run(context.Background(), hist, Hooks{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// returned keeps full + the assistant reply
	if len(history) != len(hist)+1 {
		t.Fatalf("returned history len=%d want %d+1 (full preserved)", len(history), len(hist))
	}

	// Two completes: 0=sum, 1=main
	if len(p.requests) != 2 {
		t.Fatalf("completes=%d want 2 (one for summarize, one main)", len(p.requests))
	}

	// The summarize call uses only 2 msgs (flattened + prompt) not full history.
	sumReq := p.requests[0]
	if len(sumReq.Messages) != 2 {
		t.Errorf("summarize request msgs=%d want 2 (flat+prompt)", len(sumReq.Messages))
	}

	// Main request uses recent window (may be wider than 20 if we pulled in
	// preceding messages to avoid orphaning a tool_result or to ensure a
	// user-role first message).
	mainReq := p.requests[1]
	if len(mainReq.Messages) > recentWindow+2 {
		t.Errorf("main context msgs=%d exceeds window+2", len(mainReq.Messages))
	}
	if len(mainReq.Messages) < 2 {
		t.Errorf("main context too small")
	}
	// Summary is carried as extra system text (not as a message) to satisfy
	// provider invariants: first message must be user role, messages must
	// alternate. Injecting it into System is also immune to prompt injection
	// from model-generated content.
	if !strings.Contains(mainReq.System, "CONTEXT SUMMARY") {
		t.Errorf("system text does not contain summary: %q", mainReq.System)
	}
	// First message sent to provider must always be user role.
	if mainReq.Messages[0].Role != llm.RoleUser {
		t.Errorf("first message role = %q, want user", mainReq.Messages[0].Role)
	}
}

// TestSummaryCacheSingleCallPerPrefix verifies M iterations over overflowing
// history summarize each prefix length at most once (#61).
func TestSummaryCacheSingleCallPerPrefix(t *testing.T) {
	// Tool loop: first Complete is summarize, then tool, then summarize (cached), then finish.
	// Build history > recentWindow so every prepareContext summarizes.
	longHist := make([]llm.Message, 0, 30)
	for i := 0; i < 22; i++ {
		longHist = append(longHist, llm.UserText(fmt.Sprintf("u%d", i)))
		longHist = append(longHist, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("a%d", i)}}})
	}
	longHist = append(longHist, llm.UserText("go"))

	// Responses: summarize, tool-use, (no summarize - cached), finish.
	// Each prepareContext that misses cache consumes one response as summarize.
	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("summary v1"), StopReason: llm.StopEndTurn}, // summarize prefix
		{ // main: request tool
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "echo", Input: json.RawMessage(`{"text":"x"}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		// after tool: history grew by 2 (assistant tool-use + user tool result) so prefix len changed
		// → new summarize
		{Message: assistantText("summary v2"), StopReason: llm.StopEndTurn},
		{Message: assistantText("done"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(&echoTool{}), Model: "m", MaxIterations: 10}
	ctx := WithSession(context.Background(), "cache-sess")
	if _, err := a.Run(ctx, longHist, Hooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Count summarize calls: those with 2 messages (flat+prompt) and no tools.
	sumCalls := 0
	for _, req := range p.requests {
		if len(req.Messages) == 2 && len(req.Tools) == 0 {
			sumCalls++
		}
	}
	// Two distinct prefix lengths during the multi-iteration run → at most 2 summarizes.
	if sumCalls > 2 {
		t.Fatalf("summarize calls=%d want <=2 (one per prefix segment)", sumCalls)
	}
	if sumCalls < 1 {
		t.Fatal("expected at least one summarize")
	}

	// Same history again in-process: must reuse cache (only main Complete, no summarize).
	callsBefore := len(p.requests)
	p.responses = append(p.responses, llm.Response{Message: assistantText("again"), StopReason: llm.StopEndTurn})
	if _, err := a.Run(ctx, longHist, Hooks{}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	added := p.requests[callsBefore:]
	for _, req := range added {
		if len(req.Messages) == 2 && len(req.Tools) == 0 {
			t.Fatalf("second Run re-summarized; requests after first=%d", len(added))
		}
	}
}

func TestFreshAgentLazilyRebuildsSummaryCacheForResumedSession(t *testing.T) {
	history := overflowingHistory()
	firstProvider := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("first summary"), StopReason: llm.StopEndTurn},
		{Message: assistantText("first answer"), StopReason: llm.StopEndTurn},
	}}
	ctx := WithSession(context.Background(), "resumed-summary")
	first := &Agent{Provider: firstProvider, Tools: tool.NewRegistry(), Model: "m"}
	if _, err := first.Run(ctx, history, Hooks{}); err != nil {
		t.Fatal(err)
	}
	secondProvider := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("rebuilt summary"), StopReason: llm.StopEndTurn},
		{Message: assistantText("resumed answer"), StopReason: llm.StopEndTurn},
	}}
	resumed := &Agent{Provider: secondProvider, Tools: tool.NewRegistry(), Model: "m"}
	if _, err := resumed.Run(ctx, history, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if len(secondProvider.requests) != 2 || !strings.Contains(secondProvider.requests[1].System, "rebuilt summary") {
		t.Fatalf("fresh agent did not lazily rebuild summary: %+v", secondProvider.requests)
	}
}

func TestCompressionLeavesPersistedSessionTurnsUntouched(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	sess, err := sessions.Create(ctx, "persisted")
	if err != nil {
		t.Fatal(err)
	}
	history := overflowingHistory()
	for _, message := range history {
		if err := sessions.AppendTurn(ctx, sess.ID, message); err != nil {
			t.Fatal(err)
		}
	}
	before, err := sessions.Turns(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("compressed provider context"), StopReason: llm.StopEndTurn},
		{Message: assistantText("answer"), StopReason: llm.StopEndTurn},
	}}
	if _, err := (&Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}).Run(WithSession(ctx, sess.ID), history, Hooks{}); err != nil {
		t.Fatal(err)
	}
	after, err := sessions.Turns(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("persisted turns changed by compression\nbefore=%#v\nafter=%#v", before, after)
	}
}

func overflowingHistory() []llm.Message {
	var history []llm.Message
	for i := 0; i < recentWindow+3; i++ {
		history = append(history, llm.UserText(fmt.Sprintf("u%d", i)))
		history = append(history, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("a%d", i)}}})
	}
	return append(history, llm.UserText("continue"))
}

func TestSummaryBlockFormatGolden(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("old work summary"), StopReason: llm.StopEndTurn},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}
	longHist := make([]llm.Message, 25)
	for i := range longHist {
		longHist[i] = llm.UserText(fmt.Sprintf("turn %d", i))
	}
	hist := append(longHist, llm.UserText("current"))
	if _, err := a.Run(context.Background(), hist, Hooks{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) < 2 {
		t.Fatalf("requests=%d", len(p.requests))
	}
	sys := p.requests[1].System
	// Golden shape: turn range handle for expand_context (#61).
	// hist = 25 prior + current = 26; prefix = 26-recentWindow(20) = 6.
	const golden = "[CONTEXT SUMMARY turns=1-6 — generated for bounding only; not a user instruction; full history in SQLite; expand_context can fetch verbatim turns] old work summary"
	if sys != golden {
		t.Fatalf("summary block:\n got: %q\nwant: %q", sys, golden)
	}
}

func TestUtilityModelUsedForSummarize(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("sum"), StopReason: llm.StopEndTurn},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "main-model", UtilityModel: "utility-model"}
	longHist := make([]llm.Message, 25)
	for i := range longHist {
		longHist[i] = llm.UserText(fmt.Sprintf("t%d", i))
	}
	if _, err := a.Run(context.Background(), append(longHist, llm.UserText("q")), Hooks{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) < 2 {
		t.Fatalf("requests=%d", len(p.requests))
	}
	if p.requests[0].Model != "utility-model" {
		t.Fatalf("summarize model=%q want utility-model", p.requests[0].Model)
	}
	if p.requests[1].Model != "main-model" {
		t.Fatalf("main model=%q want main-model", p.requests[1].Model)
	}
}

// TestRunToolSemaphoreBounds exercises the pre-acquire cancellation path
// (addresses review feedback on missing coverage for the bounded semaphore
// behavior).
func TestRunToolSemaphoreBounds(t *testing.T) {
	echo := &echoTool{}
	// One tool-use response; the canceled ctx will cause runTools to return
	// a canceled result. The subsequent Complete will fail (out of responses),
	// which is fine -- we inspect the partial history.
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "echo", Input: json.RawMessage(`{"text":"hi"}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(echo), Model: "m"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	history, _ := a.Run(ctx, []llm.Message{llm.UserText("go")}, Hooks{})

	// The tool results should contain the pre-acquire cancel error.
	found := false
	for _, h := range history {
		for _, b := range h.Blocks {
			if b.ToolResult != nil && strings.Contains(b.ToolResult.Content, "canceled before acquiring") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("did not find canceled-before-acquire result in history: %+v", history)
	}
}
