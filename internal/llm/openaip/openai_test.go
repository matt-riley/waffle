package openaip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

func sseServer(t *testing.T, gotBody *map[string]any, lines ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if gotBody != nil {
			if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
				t.Errorf("decode request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestCompleteText(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body,
		`{"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo!"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`,
	)
	defer srv.Close()

	p := New("test-key", srv.URL+"/v1")
	var streamed strings.Builder
	resp, err := p.Complete(context.Background(), llm.Request{
		Model:    "test-model",
		System:   "be brief",
		Messages: []llm.Message{llm.UserText("hi")},
	}, func(e llm.Event) { streamed.WriteString(e.Text) })
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Text() != "Hello!" || streamed.String() != "Hello!" {
		t.Errorf("text = %q, streamed = %q", resp.Message.Text(), streamed.String())
	}
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// Request translation: system message first, then user.
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("first message = %v", first)
	}
	if body["stream"] != true {
		t.Error("stream not set")
	}
	opts, ok := body["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage=true", body["stream_options"])
	}
}

func TestUsageOnlyFinalChunk(t *testing.T) {
	// Spec-compliant OpenAI backends send usage in a trailing chunk whose
	// choices array is empty (when stream_options.include_usage is set).
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("hi")}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Text() != "Hi" {
		t.Errorf("text = %q", resp.Message.Text())
	}
	if resp.StopReason != llm.StopEndTurn {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want input=7 output=2", resp.Usage)
	}
}

func TestCompleteToolCall(t *testing.T) {
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("list files")}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Fatalf("stop = %q", resp.StopReason)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("tool uses = %d", len(uses))
	}
	if uses[0].ID != "call_1" || uses[0].Name != "bash" || string(uses[0].Input) != `{"command":"ls"}` {
		t.Errorf("tool use = %+v input=%s", uses[0], uses[0].Input)
	}
}

func TestToolResultTranslation(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	defer srv.Close()

	history := []llm.Message{
		llm.UserText("list files"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "call_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "call_1", Content: "a.txt"}},
		}},
	}
	p := New("k", srv.URL+"/v1")
	if _, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: history}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("wire messages = %d, want 3: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)
	calls := asst["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" || fn["arguments"] != `{"command":"ls"}` {
		t.Errorf("assistant tool_calls = %v", calls)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "a.txt" {
		t.Errorf("tool message = %v", toolMsg)
	}
}

func TestHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	_, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("x")}}, nil)
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err = %v, want body surfaced", err)
	}
}

func TestCompleteSizeCap(t *testing.T) {
	orig := maxAccumulatedBytes
	maxAccumulatedBytes = 5 // tiny cap to exercise truncation without huge payloads
	defer func() { maxAccumulatedBytes = orig }()

	// Text accumulation cap.
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"content":"12345"}}]}`,
		`{"choices":[{"delta":{"content":"6789"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	full := resp.Message.Text()
	if !strings.Contains(full, "12345") {
		t.Errorf("expected partial text prefix, got %q", full)
	}
	if strings.Contains(full, "6789") {
		t.Errorf("overflow delta should have been dropped, got %q", full)
	}
	if !strings.Contains(full, "WARNING") || !strings.Contains(full, "truncated") {
		t.Errorf("expected warning block in text, got %q", full)
	}
	// Verify warning is a separate block appended.
	foundWarning := false
	for _, b := range resp.Message.Blocks {
		if b.Type == llm.BlockText && strings.Contains(b.Text, "truncated") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Error("warning block not present in Blocks")
	}

	// Tool call arg accumulation cap (separate server).
	srv2 := sseServer(t, nil,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"a\":\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1234567890\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv2.Close()

	p2 := New("k", srv2.URL+"/v1")
	resp2, err := p2.Complete(context.Background(), llm.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Complete tool: %v", err)
	}
	if resp2.StopReason != llm.StopEndTurn {
		t.Fatalf("expected non-tool stop reason on truncation, got %q", resp2.StopReason)
	}
	uses := resp2.ToolUses()
	if len(uses) != 0 {
		t.Fatalf("expected no tool uses on truncation (to avoid bad JSON input), got %d", len(uses))
	}
	// Warning block present.
	foundW := false
	for _, b := range resp2.Message.Blocks {
		if b.Type == llm.BlockText && strings.Contains(b.Text, "truncated") {
			foundW = true
		}
	}
	if !foundW {
		t.Error("expected warning block for capped tool args")
	}
}

func TestTextCapDoesNotDropToolCall(t *testing.T) {
	orig := maxAccumulatedBytes
	maxAccumulatedBytes = 8
	defer func() { maxAccumulatedBytes = orig }()

	// Text nearly exhausts (and overflows) its budget, but a small complete
	// tool call must still survive with StopToolUse: text and tool call
	// arguments spend separate budgets.
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"content":"1234567890"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Fatalf("stop = %q, want %q (complete tool call must survive text cap)", resp.StopReason, llm.StopToolUse)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("tool uses = %d, want 1", len(uses))
	}
	if uses[0].ID != "c1" || uses[0].Name != "bash" || string(uses[0].Input) != `{"a":1}` {
		t.Errorf("tool use = %+v input=%s", uses[0], uses[0].Input)
	}
	// Text was still truncated and the warning block appended.
	full := resp.Message.Text()
	if !strings.Contains(full, "12345678") || strings.Contains(full, "90") {
		t.Errorf("text = %q, want truncated to 8 bytes", full)
	}
	if !strings.Contains(full, "truncated") {
		t.Errorf("expected truncation warning, got %q", full)
	}
}

func TestCompleteToolCallKeptWhenAnotherTruncated(t *testing.T) {
	orig := maxAccumulatedBytes
	maxAccumulatedBytes = 10
	defer func() { maxAccumulatedBytes = orig }()

	// First tool call completes within budget; the second overflows it.
	// The complete call must be emitted (StopToolUse kept) and the
	// truncated one dropped rather than emitted as corrupt JSON.
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"bash","arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"bash","arguments":"{\"b\":\"123456789\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m"}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Fatalf("stop = %q, want %q (complete call exists)", resp.StopReason, llm.StopToolUse)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("tool uses = %d, want 1 (truncated call dropped)", len(uses))
	}
	if uses[0].ID != "c0" || string(uses[0].Input) != `{"a":1}` {
		t.Errorf("tool use = %+v input=%s", uses[0], uses[0].Input)
	}
	full := resp.Message.Text()
	if !strings.Contains(full, "truncated") {
		t.Errorf("expected truncation warning, got %q", full)
	}
}

// TestCachedTokensSplitFromPromptTokens pins the accounting half of #247
// for OpenAI-compatible endpoints: the final stream chunk's
// prompt_tokens_details.cached_tokens lands in CacheReadInputTokens, and
// prompt_tokens (which includes the cached subset) is reduced so the three
// counters sum to the provider-reported total.
func TestCachedTokensSplitFromPromptTokens(t *testing.T) {
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("hi")}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := llm.Usage{InputTokens: 7, OutputTokens: 5, CacheReadInputTokens: 3, Provider: "openai"}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
	// InputTokens + CacheReadInputTokens must equal the reported prompt
	// total; cached input is never billed at the full input rate.
	if got := resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens; got != 10 {
		t.Fatalf("input counters sum = %d, want 10", got)
	}
}

// TestCachedTokensNeverNegative pins the failure path for a provider that
// over-reports the cached subset: the uncached count clamps at zero instead
// of going negative.
func TestCachedTokensNeverNegative(t *testing.T) {
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"content":"Hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":9}}}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("hi")}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.CacheReadInputTokens != 9 {
		t.Fatalf("usage = %+v, want input 0 cache_read 9", resp.Usage)
	}
}

// TestProviderWithoutUsageYieldsZeroedCounters pins the OpenAI failure path:
// a response with no usage object yields zeroed counters, never a panic.
func TestProviderWithoutUsageYieldsZeroedCounters(t *testing.T) {
	srv := sseServer(t, nil,
		`{"choices":[{"delta":{"content":"Hi"},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	resp, err := p.Complete(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{llm.UserText("hi")}}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage != (llm.Usage{}) {
		t.Fatalf("usage = %+v, want zeroed counters", resp.Usage)
	}
}

// TestSystemExtraMergedIntoSingleSystemMessage pins finding 2 of the #247
// review for OpenAI-compatible providers: they have no cache breakpoints,
// so the translator merges SystemExtra back into the one system message —
// byte-identical to the pre-split combined text — and never loses the
// summary.
func TestSystemExtraMergedIntoSingleSystemMessage(t *testing.T) {
	var body map[string]any
	srv := sseServer(t, &body,
		`{"choices":[{"delta":{"role":"assistant","content":"Hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
	)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	const system = "you are waffle"
	const extra = "[CONTEXT SUMMARY turns=1-2] prior work"
	if _, err := p.Complete(context.Background(), llm.Request{
		Model:       "m",
		System:      system,
		SystemExtra: extra,
		Messages:    []llm.Message{llm.UserText("hi")},
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) < 1 {
		t.Fatal("no messages in request")
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("first message role = %v, want system", sys["role"])
	}
	if got := sys["content"]; got != system+"\n\n"+extra {
		t.Fatalf("system content = %q, want combined %q", got, system+"\n\n"+extra)
	}

	// With an empty stable System the extra text becomes the system message.
	if _, err := p.Complete(context.Background(), llm.Request{
		Model:       "m",
		SystemExtra: extra,
		Messages:    []llm.Message{llm.UserText("hi")},
	}, nil); err != nil {
		t.Fatalf("Complete with SystemExtra only: %v", err)
	}
	msgs = body["messages"].([]any)
	sys = msgs[0].(map[string]any)
	if got := sys["content"]; got != extra {
		t.Fatalf("system content = %q, want extra text %q", got, extra)
	}
}
