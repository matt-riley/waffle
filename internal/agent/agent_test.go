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
	"github.com/matt-riley/waffle/internal/memory"
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
			Usage:      llm.Usage{InputTokens: 3, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30},
		},
		{
			Message:    assistantText("done"),
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{InputTokens: 7, OutputTokens: 11, CacheReadInputTokens: 40},
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
	want := []llm.Usage{
		{InputTokens: 3, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30},
		{InputTokens: 10, OutputTokens: 16, CacheCreationInputTokens: 20, CacheReadInputTokens: 70},
	}
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

// TestRunLabelsMediaUntrustedReachingModel pins the untrusted-input posture
// for media content: a user message carrying an image or document reaches
// the model with the untrusted framing text block inserted before the media
// block — the same data-never-instructions posture tool output and fetched
// content carry. The persisted history the caller receives is NOT mutated.
func TestRunLabelsMediaUntrustedReachingModel(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := llm.NewDocumentBlock("application/pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		profile string
		blocks  []llm.Block
	}{
		{"main tier", "main", []llm.Block{{Type: llm.BlockText, Text: "what is this?"}, img}},
		{"cron tier", "cron", []llm.Block{{Type: llm.BlockText, Text: "what is this?"}, img, doc}},
		{"issue tier", "issue", []llm.Block{img}},
		{"group tier", "group", []llm.Block{{Type: llm.BlockText, Text: "check"}, img}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProvider{responses: []llm.Response{{Message: assistantText("ok"), StopReason: llm.StopEndTurn}}}
			a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m", Profile: tc.profile}
			incoming := llm.Message{Role: llm.RoleUser, Blocks: append([]llm.Block(nil), tc.blocks...)}
			history, err := a.Run(context.Background(), []llm.Message{incoming}, Hooks{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(p.requests) != 1 {
				t.Fatalf("provider calls = %d, want 1", len(p.requests))
			}
			sent := p.requests[0].Messages[0]
			labelled := false
			for _, b := range sent.Blocks {
				if b.Type == llm.BlockText && strings.Contains(b.Text, llm.UntrustedMediaLabel) {
					labelled = true
				}
			}
			if !labelled {
				t.Fatalf("untrusted label did not reach the model: %+v", sent.Blocks)
			}
			// The label must precede the media block, not trail it.
			for i, b := range sent.Blocks {
				if b.Type == llm.BlockImage || b.Type == llm.BlockDocument {
					if i == 0 || !strings.Contains(sent.Blocks[i-1].Text, llm.UntrustedMediaLabel) {
						t.Fatalf("media block at %d not preceded by label: %+v", i, sent.Blocks)
					}
					break
				}
			}
			// Persisted history keeps the caller's message unchanged (no
			// label): Run appends only the assistant response.
			if len(history) != 2 || !reflect.DeepEqual(history[0].Blocks, tc.blocks) {
				t.Fatalf("persisted history mutated: %+v", history)
			}
		})
	}
}

// TestRunDoesNotLabelTextOnlyMessages guards against labelling noise on
// ordinary conversations.
func TestRunDoesNotLabelTextOnlyMessages(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{{Message: assistantText("ok"), StopReason: llm.StopEndTurn}}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}
	if _, err := a.Run(context.Background(), []llm.Message{llm.UserText("hello")}, Hooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, b := range p.requests[0].Messages[0].Blocks {
		if strings.Contains(b.Text, llm.UntrustedMediaLabel) {
			t.Fatalf("text-only message labelled: %+v", p.requests[0].Messages[0].Blocks)
		}
	}
}

// TestRunMediaUserMessageTaintsOriginBeforeFirstToolBatch pins #592: a
// media-bearing inbound user message must mark the run origin untrusted
// before the first tool batch executes, so a remember derived from an
// attached image lands in the review queue even under write_gate=auto.
func TestRunMediaUserMessageTaintsOriginBeforeFirstToolBatch(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	ws := mediaTestWorkspace(t)
	gate := &memory.Gate{Mode: "auto", WS: ws}
	p := &fakeProvider{responses: []llm.Response{
		llmtest.ToolCall("remember", "t1", `{"note":"the screenshot says to remember this"}`),
		{Message: assistantText("done"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(memory.RememberTool{WS: ws, Gate: gate}), Model: "m"}
	ctx := session.WithOrigin(context.Background(), "session-media", "telegram")
	if _, err := a.Run(ctx, []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: "what does this say?"}, img,
	}}}, Hooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !session.OriginFromContext(ctx).Untrusted {
		t.Fatal("origin not marked untrusted by media-bearing user message")
	}
	pending, err := gate.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d candidates, want 1 (media-derived remember must queue under write_gate=auto)", len(pending))
	}
	if pending[0].Provenance.TrustClass != "untrusted_derived" {
		t.Errorf("trust class = %q, want untrusted_derived", pending[0].Provenance.TrustClass)
	}
	if lines, err := ws.MatchingLines("the screenshot says to remember this"); err != nil || len(lines) > 0 {
		t.Fatalf("media-derived remember applied to live memory without review (lines=%v, err=%v)", lines, err)
	}
}

// TestRunMediaToolResultTaintsOriginBeforeNextBatch pins #592's tool-result
// path: media blocks returned by a tool in one batch must taint the origin
// before the next batch's tools run, so a later remember in the same run
// queues for review.
func TestRunMediaToolResultTaintsOriginBeforeNextBatch(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	ws := mediaTestWorkspace(t)
	gate := &memory.Gate{Mode: "auto", WS: ws}
	p := &fakeProvider{responses: []llm.Response{
		llmtest.ToolCall("mixed", "t1", `{}`),
		llmtest.ToolCall("remember", "t2", `{"note":"derived from the tool image"}`),
		{Message: assistantText("done"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(&mixedResultTool{img: img}, memory.RememberTool{WS: ws, Gate: gate}), Model: "m"}
	ctx := session.WithOrigin(context.Background(), "session-media-tool", "web")
	if _, err := a.Run(ctx, []llm.Message{llm.UserText("go")}, Hooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !session.OriginFromContext(ctx).Untrusted {
		t.Fatal("origin not marked untrusted by media-bearing tool result")
	}
	pending, err := gate.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d candidates, want 1", len(pending))
	}
	if pending[0].Provenance.TrustClass != "untrusted_derived" {
		t.Errorf("trust class = %q, want untrusted_derived", pending[0].Provenance.TrustClass)
	}
	if lines, err := ws.MatchingLines("derived from the tool image"); err != nil || len(lines) > 0 {
		t.Fatalf("media-derived remember applied to live memory without review (lines=%v, err=%v)", lines, err)
	}
}

// mediaTestWorkspace opens a throwaway memory workspace in a temp dir.
func mediaTestWorkspace(t *testing.T) memory.Workspace {
	t.Helper()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return ws
}

// TestRunRedactsTextPartsOfMixedToolResult pins that secret redaction still
// runs on the text parts of a block-carrying tool result (and that media
// payloads are not touched).
func TestRunRedactsTextPartsOfMixedToolResult(t *testing.T) {
	secret := "sk-super-secret-value"
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "mixed", Input: json.RawMessage(`{}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	echo := &mixedResultTool{img: img, secret: secret}
	a := &Agent{
		Provider: p, Tools: tool.NewRegistry(echo), Model: "m",
		Redact: func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") },
	}
	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := history[2].Blocks[0].ToolResult
	if tr == nil || len(tr.Blocks) != 2 {
		t.Fatalf("tool result = %+v", tr)
	}
	if strings.Contains(tr.Blocks[0].Text, secret) {
		t.Fatalf("secret leaked into text part: %q", tr.Blocks[0].Text)
	}
	if !strings.Contains(tr.Blocks[0].Text, "[redacted]") {
		t.Fatalf("text part not redacted: %q", tr.Blocks[0].Text)
	}
	// Media payload is binary data, not text: left untouched.
	if tr.Blocks[1].Source == nil || tr.Blocks[1].Source.Data != img.Source.Data {
		t.Fatalf("media payload mangled: %+v", tr.Blocks[1])
	}
}

// mixedResultTool returns a tool result carrying a text block (with a secret
// in it) and an image block.
type mixedResultTool struct {
	img    llm.Block
	secret string
}

func (m *mixedResultTool) Def() llm.Tool {
	return llm.Tool{Name: "mixed", Description: "mixed", InputSchema: json.RawMessage(`{}`)}
}

func (m *mixedResultTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	return "", nil
}

// RunBlocks implements tool.BlockTool: the text body carries a secret, and
// an image block rides along.
func (m *mixedResultTool) RunBlocks(ctx context.Context, input json.RawMessage) (string, []llm.Block, error) {
	return "mixed output", []llm.Block{
		{Type: llm.BlockText, Text: "chart for " + m.secret},
		m.img,
	}, nil
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
	// Summary is carried as SystemExtra (not merged into System, and not as
	// a message) so the stable System prefix keeps its prompt-cache
	// breakpoint across calls (#247). Provider invariants still hold: first
	// message must be user role, messages must alternate.
	if !strings.Contains(mainReq.SystemExtra, "CONTEXT SUMMARY") {
		t.Errorf("SystemExtra does not contain summary: %q", mainReq.SystemExtra)
	}
	if mainReq.System != "" {
		t.Errorf("System = %q, want empty (no stable system prompt in this test)", mainReq.System)
	}
	// First message sent to provider must always be user role.
	if mainReq.Messages[0].Role != llm.RoleUser {
		t.Errorf("first message role = %q, want user", mainReq.Messages[0].Role)
	}
}

// TestRunSplitsSummaryIntoSystemExtra pins finding 2 of the #247 review at
// the agent layer: the per-run context summary rides in Request.SystemExtra
// while Request.System stays the agent's stable system prompt, so providers
// with prompt-cache breakpoints (anthropicp) can keep the system bytes
// identical across calls.
func TestRunSplitsSummaryIntoSystemExtra(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("old work: planned then coded"), StopReason: llm.StopEndTurn},
		{Message: assistantText("ok with recent"), StopReason: llm.StopEndTurn},
	}}
	const stableSystem = "you are waffle, a personal assistant with a long stable system prompt"
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m", System: stableSystem}

	longHist := make([]llm.Message, 25)
	for i := 0; i < 25; i++ {
		longHist[i] = llm.UserText(fmt.Sprintf("turn %d", i))
	}
	hist := append(longHist, llm.UserText("current question"))
	if _, err := a.Run(context.Background(), hist, Hooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("completes=%d want 2 (one for summarize, one main)", len(p.requests))
	}
	mainReq := p.requests[1]
	if mainReq.System != stableSystem {
		t.Errorf("main System = %q, want the stable system prompt unchanged", mainReq.System)
	}
	if !strings.Contains(mainReq.SystemExtra, "CONTEXT SUMMARY") {
		t.Errorf("main SystemExtra does not carry the summary: %q", mainReq.SystemExtra)
	}
	// Without a summary the extra field stays empty and System is untouched.
	short := &Agent{Provider: &fakeProvider{responses: []llm.Response{
		{Message: assistantText("plain answer"), StopReason: llm.StopEndTurn},
	}}, Tools: tool.NewRegistry(), Model: "m", System: stableSystem}
	if _, err := short.Run(context.Background(), []llm.Message{llm.UserText("hi")}, Hooks{}); err != nil {
		t.Fatalf("short Run: %v", err)
	}
	if got := short.Provider.(*fakeProvider).requests[0]; got.System != stableSystem || got.SystemExtra != "" {
		t.Errorf("short request = System %q SystemExtra %q, want stable system and no extra", got.System, got.SystemExtra)
	}
}

// TestSummaryCacheSkippedWhenSessionIDEmpty verifies that two unrelated
// runs on the same Agent with no session ID and equal-length prefixes do
// not share a cached summary (#120).
func TestSummaryCacheSkippedWhenSessionIDEmpty(t *testing.T) {
	// Two histories of equal length (so equal prefix lengths) but different content.
	histA := make([]llm.Message, 0, (recentWindow+3)*2+1)
	histB := make([]llm.Message, 0, (recentWindow+3)*2+1)
	for i := 0; i < recentWindow+3; i++ {
		histA = append(histA, llm.UserText(fmt.Sprintf("alpha-%d", i)))
		histA = append(histA, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("alpha-a%d", i)}}})
		histB = append(histB, llm.UserText(fmt.Sprintf("beta-%d", i)))
		histB = append(histB, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("beta-a%d", i)}}})
	}
	histA = append(histA, llm.UserText("question-a"))
	histB = append(histB, llm.UserText("question-b"))
	if len(histA) != len(histB) {
		t.Fatalf("hist lengths differ: %d vs %d", len(histA), len(histB))
	}

	p := &fakeProvider{responses: []llm.Response{
		{Message: assistantText("summary about alpha"), StopReason: llm.StopEndTurn},
		{Message: assistantText("answer-a"), StopReason: llm.StopEndTurn},
		{Message: assistantText("summary about beta"), StopReason: llm.StopEndTurn},
		{Message: assistantText("answer-b"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}
	// No WithSession — empty session id (would collide on ":N" before the fix).
	if _, err := a.Run(context.Background(), histA, Hooks{}); err != nil {
		t.Fatalf("run A: %v", err)
	}
	if _, err := a.Run(context.Background(), histB, Hooks{}); err != nil {
		t.Fatalf("run B: %v", err)
	}

	// Without caching, each run summarizes independently: 2 summarize + 2 main.
	if len(p.requests) != 4 {
		t.Fatalf("requests=%d want 4 (summarize+main per run, no cache share)", len(p.requests))
	}
	// Main request for run B must carry beta summary, not alpha.
	mainB := p.requests[3]
	if !strings.Contains(mainB.SystemExtra, "summary about beta") {
		t.Errorf("run B SystemExtra should contain beta summary, got: %q", mainB.SystemExtra)
	}
	if strings.Contains(mainB.SystemExtra, "summary about alpha") {
		t.Errorf("run B SystemExtra cross-contaminated with alpha summary: %q", mainB.SystemExtra)
	}
	// Cache must remain empty when sid is absent.
	a.summaryMu.Lock()
	cacheLen := len(a.summaryCache)
	a.summaryMu.Unlock()
	if cacheLen != 0 {
		t.Errorf("summaryCache len=%d want 0 when session id empty", cacheLen)
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
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "mixed", Input: json.RawMessage(`{}`)}},
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

func TestSummaryCacheIsBoundedAndEvictsLeastRecentlyUsed(t *testing.T) {
	var summarizeCalls int
	p := &recordingProvider{onComplete: func(llm.Request) llm.Response {
		summarizeCalls++
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message:    assistantText(fmt.Sprintf("summary-%d", summarizeCalls)),
		}
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}
	history := overflowingHistory()
	prepare := func(sid string) string {
		_, system, err := a.prepareContext(WithSession(context.Background(), sid), history, nil)
		if err != nil {
			t.Fatalf("prepareContext(%q): %v", sid, err)
		}
		return system
	}
	cacheLen := func() int {
		a.summaryMu.Lock()
		defer a.summaryMu.Unlock()
		return len(a.summaryCache)
	}

	for i := 0; i < summaryCacheCapacity; i++ {
		prepare(fmt.Sprintf("summary-cache-%d", i))
	}
	if got := cacheLen(); got != summaryCacheCapacity {
		t.Fatalf("cache len=%d, want capacity %d", got, summaryCacheCapacity)
	}
	if summarizeCalls != summaryCacheCapacity {
		t.Fatalf("summarize calls=%d, want %d", summarizeCalls, summaryCacheCapacity)
	}

	// A hit refreshes recency, so this entry must survive the next insertion.
	hitSystem := prepare("summary-cache-0")
	if summarizeCalls != summaryCacheCapacity {
		t.Fatalf("cache hit re-summarized: calls=%d", summarizeCalls)
	}
	prepare("summary-cache-new")
	if got := cacheLen(); got != summaryCacheCapacity {
		t.Fatalf("cache len after insertion=%d, want capacity %d", got, summaryCacheCapacity)
	}
	if summarizeCalls != summaryCacheCapacity+1 {
		t.Fatalf("summarize calls after insertion=%d, want %d", summarizeCalls, summaryCacheCapacity+1)
	}

	// The untouched oldest entry was evicted, while the recently used entry
	// remains and still returns its original summary.
	evictedSystem := prepare("summary-cache-1")
	if summarizeCalls != summaryCacheCapacity+2 {
		t.Fatalf("evicted entry did not re-summarize: calls=%d", summarizeCalls)
	}
	if strings.Contains(evictedSystem, "summary-2") {
		t.Fatalf("evicted entry returned stale cached summary: %q", evictedSystem)
	}
	if got := prepare("summary-cache-0"); got != hitSystem {
		t.Fatalf("recently used entry changed after eviction:\n got: %q\nwant: %q", got, hitSystem)
	}
	if summarizeCalls != summaryCacheCapacity+2 {
		t.Fatalf("recently used entry re-summarized: calls=%d", summarizeCalls)
	}
}

func TestSummaryCacheConcurrentAccessStaysBounded(t *testing.T) {
	p := &recordingProvider{onComplete: func(llm.Request) llm.Response {
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message:    assistantText("concurrent summary"),
		}
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m"}
	history := overflowingHistory()
	const workers = summaryCacheCapacity * 2
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := a.prepareContext(WithSession(context.Background(), fmt.Sprintf("concurrent-%d", i)), history, nil)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a.summaryMu.Lock()
	got := len(a.summaryCache)
	a.summaryMu.Unlock()
	if got != summaryCacheCapacity {
		t.Fatalf("concurrent cache len=%d, want capacity %d", got, summaryCacheCapacity)
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
	if len(secondProvider.requests) != 2 || !strings.Contains(secondProvider.requests[1].SystemExtra, "rebuilt summary") {
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
	sys := p.requests[1].SystemExtra
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

// panicTool panics with a value carrying its input, standing in for any tool
// (MCP glue, policy hook, nil deref) that can fail catastrophically.
type panicTool struct{}

func (panicTool) Def() llm.Tool {
	return llm.Tool{Name: "boom", Description: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (panicTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	panic("boom: SECRET_PANIC_DETAIL")
}

func TestRunToolPanicBecomesErrorResultWithoutCrashing(t *testing.T) {
	var logs bytes.Buffer
	echo := &echoTool{}
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "boom", Input: json.RawMessage(`{}`)}},
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t2", Name: "echo", Input: json.RawMessage(`{"text":"a"}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("recovered"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(panicTool{}, echo), Model: "m", Profile: "main", Log: slog.New(slog.NewTextHandler(&logs, nil))}

	var done []llm.ToolResult
	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("go")}, Hooks{
		OnToolDone: func(_ llm.ToolUse, res llm.ToolResult) { done = append(done, res) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("history len = %d, want 4 (the run must continue past the panic)", len(history))
	}

	results := history[2]
	if len(results.Blocks) != 2 {
		t.Fatalf("tool results malformed: %+v", results)
	}
	panicked := results.Blocks[0].ToolResult
	if !panicked.IsError || panicked.ToolUseID != "t1" {
		t.Errorf("panicking tool result = %+v, want an error result for t1", panicked)
	}
	if !strings.Contains(panicked.Content, "panicked") {
		t.Errorf("panicking tool content = %q, want it to say the tool panicked", panicked.Content)
	}
	if strings.Contains(panicked.Content, "SECRET_PANIC_DETAIL") {
		t.Errorf("panic value leaked to the model: %q", panicked.Content)
	}

	// The sibling call in the same batch must still be dispatched and answered.
	if survivor := results.Blocks[1].ToolResult; survivor.IsError || !strings.Contains(survivor.Content, `"a"`) {
		t.Errorf("sibling tool result = %+v, want the echo result", survivor)
	}
	if len(done) != 2 {
		t.Errorf("OnToolDone fired %d times, want 2", len(done))
	}

	body := logs.String()
	for _, want := range []string{"msg=\"tool call panicked\"", "tool=boom", "SECRET_PANIC_DETAIL", "stack="} {
		if !strings.Contains(body, want) {
			t.Fatalf("logs missing %q: %s", want, body)
		}
	}
}
