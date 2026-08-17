package anthropicp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/matt-riley/waffle/internal/llm"
)

// closeCountingBody wraps an HTTP response body and counts Close calls.
type closeCountingBody struct {
	io.ReadCloser
	closes *atomic.Int32
}

func (b *closeCountingBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

// closeCountingTransport wraps each response body so tests can assert the
// Anthropic stream released the underlying HTTP body.
type closeCountingTransport struct {
	base       http.RoundTripper
	closes     *atomic.Int32
	onResponse func() // optional; called after a successful RoundTrip with body wrap
}

func (t *closeCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.Body != nil {
		resp.Body = &closeCountingBody{ReadCloser: resp.Body, closes: t.closes}
	}
	if t.onResponse != nil {
		t.onResponse()
	}
	return resp, nil
}

// providerWithCloseTracking builds a Provider against srv that counts response
// body Close calls via a custom RoundTripper. Retries are disabled so each
// Complete maps to a single HTTP body.
func providerWithCloseTracking(t *testing.T, srv *httptest.Server) (*Provider, *atomic.Int32) {
	t.Helper()
	var closes atomic.Int32
	return providerWithCloseTrackingTransport(t, srv, &closeCountingTransport{closes: &closes}), &closes
}

func providerWithCloseTrackingTransport(t *testing.T, srv *httptest.Server, transport http.RoundTripper) *Provider {
	t.Helper()
	httpClient := &http.Client{Transport: transport}
	return &Provider{client: anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	)}
}

func minimalOKEvents() []string {
	return []string{
		`{"type":"message_start","message":{"id":"msg_ok","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}
}

func messagesServer(t *testing.T, gotBody *map[string]any, events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("path = %s", r.URL.Path)
		}
		if gotBody != nil {
			if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
				t.Errorf("decode request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			var typ struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(e), &typ); err != nil {
				t.Fatalf("bad fixture %q: %v", e, err)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, e)
		}
	}))
}

var textAndToolEvents = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking"}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" now."}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}`,
	`{"type":"message_stop"}`,
}

func TestCompleteStreamsTextAndParsesToolUse(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, textAndToolEvents)
	defer srv.Close()

	p := New("test-key", srv.URL)
	var streamed strings.Builder
	resp, err := p.Complete(context.Background(), llm.Request{
		System:   "you are waffle",
		Messages: []llm.Message{llm.UserText("list files")},
		Tools: []llm.Tool{{
			Name:        "bash",
			Description: "run a command",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		}},
	}, func(e llm.Event) { streamed.WriteString(e.Text) })
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if streamed.String() != "Checking now." {
		t.Errorf("streamed = %q", streamed.String())
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].ID != "toolu_1" || uses[0].Name != "bash" {
		t.Fatalf("uses = %+v", uses)
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(uses[0].Input, &in); err != nil || in.Command != "ls" {
		t.Errorf("input = %s (%v)", uses[0].Input, err)
	}
	if resp.Usage.InputTokens != 25 || resp.Usage.OutputTokens != 30 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// Request shape: system block, adaptive thinking, tool schema.
	if body["model"] != DefaultModel {
		t.Errorf("model = %v", body["model"])
	}
	system := body["system"].([]any)[0].(map[string]any)
	if system["text"] != "you are waffle" {
		t.Errorf("system = %v", system)
	}
	thinking := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v", thinking)
	}
	tools := body["tools"].([]any)
	tool0 := tools[0].(map[string]any)
	if tool0["name"] != "bash" {
		t.Errorf("tool = %v", tool0)
	}
	schema := tool0["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["properties"].(map[string]any)["command"] == nil {
		t.Errorf("input_schema = %v", schema)
	}
	if got := schema["required"].([]any)[0]; got != "command" {
		t.Errorf("required = %v", schema["required"])
	}
}

// TestSummaryInSystemNotAsMessage is a provider-translation regression for
// issue #8: prepareContext previously injected the context summary as a
// leading RoleAssistant message, which the Anthropic API rejects. After the
// fix, the summary is in the System field and messages[0] must be user role.
func TestSummaryInSystemNotAsMessage(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()

	p := New("k", srv.URL)
	_, err := p.Complete(context.Background(), llm.Request{
		System:   "[CONTEXT SUMMARY - generated for bounding only] prior work done",
		Messages: []llm.Message{llm.UserText("current question")},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Summary must appear in the system block sent to Anthropic.
	system := body["system"].([]any)
	if len(system) == 0 {
		t.Fatal("no system blocks in request")
	}
	sysText := system[0].(map[string]any)["text"].(string)
	if !strings.Contains(sysText, "CONTEXT SUMMARY") {
		t.Errorf("system text %q does not contain CONTEXT SUMMARY", sysText)
	}

	// First message must always be user role — guards against a regression
	// where the summary would be injected as a leading assistant message.
	msgs := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in request")
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("first message role = %q, want \"user\"", first["role"])
	}
}

func TestToolResultRoundTrip(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()

	history := []llm.Message{
		llm.UserText("list files"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockThinking, Text: "I should run ls", Signature: "sig123"},
			{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "toolu_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "toolu_1", Content: "a.txt", IsError: false}},
		}},
	}
	p := New("k", srv.URL)
	resp, err := p.Complete(context.Background(), llm.Request{Messages: history}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Text() != "done" || resp.StopReason != llm.StopEndTurn {
		t.Errorf("resp = %+v", resp)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	// Assistant turn must replay thinking (unchanged) then tool_use.
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	think := blocks[0].(map[string]any)
	if think["type"] != "thinking" || think["thinking"] != "I should run ls" || think["signature"] != "sig123" {
		t.Errorf("thinking block = %v", think)
	}
	toolUse := blocks[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" {
		t.Errorf("tool_use block = %v", toolUse)
	}
	if cmd := toolUse["input"].(map[string]any)["command"]; cmd != "ls" {
		t.Errorf("tool_use input = %v", toolUse["input"])
	}
	// Tool result rides in a user message.
	user := msgs[2].(map[string]any)
	result := user["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_result = %v", result)
	}
}

// TestOrphanedToolResultDropped is a regression for provider 400s caused by
// tool_result blocks whose tool_use is missing from the request (interrupted
// turns and session resume can persist tool results without their tool_use;
// Kimi K3 and other strict providers reject those). The translator must drop
// them instead of failing the turn.
func TestOrphanedToolResultDropped(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()

	history := []llm.Message{
		llm.UserText("list files"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "toolu_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "toolu_1", Content: "a.txt"}},
		}},
		// Orphaned: no assistant tool_use with this id anywhere in history.
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "toolu_orphan", Content: "ghost"}},
		}},
		// Orphaned: empty tool_use_id.
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "", Content: "ghost2"}},
		}},
	}
	p := New("k", srv.URL)
	if _, err := p.Complete(context.Background(), llm.Request{Messages: history}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (orphans dropped): %v", len(msgs), msgs)
	}
	user := msgs[2].(map[string]any)
	content := user["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("user content blocks = %d, want 1", len(content))
	}
	result := content[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_result = %v", result)
	}
}

// TestCompleteClosesStreamBody is a regression for issue #100: Complete must
// call stream.Close() so the HTTP response body is released after every call.
func TestCompleteClosesStreamBody(t *testing.T) {
	srv := messagesServer(t, nil, minimalOKEvents())
	defer srv.Close()

	p, closes := providerWithCloseTracking(t, srv)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Text() != "hi" {
		t.Errorf("text = %q", resp.Message.Text())
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("body Close calls = %d, want 1", got)
	}
}

// TestCompleteClosesStreamBodyOnCancel covers the early-return path when the
// request context is cancelled while the SSE stream is still open.
func TestCompleteClosesStreamBodyOnCancel(t *testing.T) {
	// Gate cancel until RoundTrip has returned a wrapped body so we exercise
	// stream.Close on an established stream, not a pre-body request abort.
	bodyReady := make(chan struct{})
	var closes atomic.Int32
	transport := &closeCountingTransport{
		closes: &closes,
		onResponse: func() {
			select {
			case <-bodyReady:
			default:
				close(bodyReady)
			}
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not a Flusher")
		}
		// Emit message_start so the stream is established, then block until
		// the client cancels so Next() surfaces a context error.
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n",
			`{"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := providerWithCloseTrackingTransport(t, srv, transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-bodyReady:
			// Let the stream start consuming before cancelling.
			time.Sleep(20 * time.Millisecond)
			cancel()
		case <-time.After(5 * time.Second):
			cancel()
		}
	}()

	_, err := p.Complete(ctx, llm.Request{
		Messages: []llm.Message{llm.UserText("hang")},
	}, nil)
	if err == nil {
		t.Fatal("Complete: expected error after cancel, got nil")
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("body Close calls = %d, want 1 after cancel", got)
	}
}

// TestCompleteClosesStreamBodyOnHTTPError covers the path where NewStreaming
// receives a non-2xx response. The SDK still attaches a body that must be
// released via stream.Close() (or the request executor's own cleanup).
func TestCompleteClosesStreamBodyOnHTTPError(t *testing.T) {
	// 400 is non-retryable; with MaxRetries(0) we still get a single attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"boom"}}`)
	}))
	defer srv.Close()

	p, closes := providerWithCloseTracking(t, srv)
	_, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("fail")},
	}, nil)
	if err == nil {
		t.Fatal("Complete: expected HTTP error, got nil")
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("body Close calls = %d, want 1 after HTTP error", got)
	}
}

// TestCompleteClosesStreamBodyRepeated asserts that sequential Completes each
// close exactly once (no growth of unclosed bodies).
func TestCompleteClosesStreamBodyRepeated(t *testing.T) {
	srv := messagesServer(t, nil, minimalOKEvents())
	defer srv.Close()

	p, closes := providerWithCloseTracking(t, srv)
	const n = 20
	for i := 0; i < n; i++ {
		if _, err := p.Complete(context.Background(), llm.Request{
			Messages: []llm.Message{llm.UserText("hi")},
		}, nil); err != nil {
			t.Fatalf("Complete #%d: %v", i, err)
		}
	}
	if got := closes.Load(); got != int32(n) {
		t.Errorf("body Close calls = %d, want %d", got, n)
	}
}

// TestPromptCachingBreakpointsOnStablePrefix pins the request-side caching
// contract (#247): cache_control breakpoints land on the system block and
// the last tool definition — the spans that are byte-identical across calls
// in a session — and never after message content, which varies per turn.
// Two calls with an identical prefix emit byte-identical breakpoints.
func TestPromptCachingBreakpointsOnStablePrefix(t *testing.T) {
	// ~1100 tokens of stable system text and ~1100 tokens of tool schema
	// text each, comfortably above the 1024-token minimum cacheable length.
	system := strings.Repeat("waffle is a personal assistant with a stable system prompt. ", 90)
	bigSchema := func(prefix string) json.RawMessage {
		props := make(map[string]any)
		for i := range 40 {
			props[fmt.Sprintf("%s_field_%d", prefix, i)] = map[string]any{
				"type":        "string",
				"description": strings.Repeat("a long description that costs tokens ", 12),
			}
		}
		raw, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": []string{prefix + "_field_0"}})
		return raw
	}
	req := func(msg string) llm.Request {
		return llm.Request{
			System:   system,
			Messages: []llm.Message{llm.UserText(msg)},
			Tools: []llm.Tool{
				{Name: "alpha", Description: "first tool", InputSchema: bigSchema("alpha")},
				{Name: "omega", Description: "last tool", InputSchema: bigSchema("omega")},
			},
		}
	}
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer srv.Close()
	p := New("k", srv.URL)
	for _, msg := range []string{"first turn", "second turn"} {
		if _, err := p.Complete(context.Background(), req(msg), nil); err != nil {
			t.Fatalf("Complete(%q): %v", msg, err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("captured %d request bodies, want 2", len(bodies))
	}
	checkBody := func(body map[string]any) {
		t.Helper()
		sysBlocks := body["system"].([]any)
		if len(sysBlocks) != 1 {
			t.Fatalf("system blocks = %d", len(sysBlocks))
		}
		sys := sysBlocks[0].(map[string]any)
		cc, ok := sys["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" {
			t.Fatalf("system cache_control = %v, want ephemeral breakpoint", sys["cache_control"])
		}
		tools := body["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("tools = %d", len(tools))
		}
		if _, ok := tools[0].(map[string]any)["cache_control"]; ok {
			t.Fatal("breakpoint on non-final tool: the prefix up to it varies once a later tool exists")
		}
		last := tools[1].(map[string]any)
		cc, ok = last["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" {
			t.Fatalf("last tool cache_control = %v, want ephemeral breakpoint", last["cache_control"])
		}
		for i, m := range body["messages"].([]any) {
			if _, ok := m.(map[string]any)["cache_control"]; ok {
				t.Fatalf("cache_control on message %d: message content varies per turn", i)
			}
		}
	}
	for i, body := range bodies {
		checkBody(body)
		// Breakpoint placement is deterministic: system block 0 and tool
		// index 1 in both calls. Re-marshal to compare the stable spans
		// byte-for-byte across calls.
		sysA, _ := json.Marshal(body["system"])
		sysB, _ := json.Marshal(bodies[1-i]["system"])
		if !bytes.Equal(sysA, sysB) {
			t.Fatalf("system span differs across calls: %s vs %s", sysA, sysB)
		}
	}
}

// TestPromptCachingSkipsBelowMinimumCacheableLength pins that a prefix below
// the provider's minimum cacheable length requests no cache write at all —
// a session whose prefix is too short to cache is never made more expensive
// by the surcharge.
func TestPromptCachingSkipsBelowMinimumCacheableLength(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	if _, err := p.Complete(context.Background(), llm.Request{
		System:   "short system prompt",
		Messages: []llm.Message{llm.UserText("hi")},
		Tools: []llm.Tool{{
			Name:        "bash",
			Description: "run a command",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		}},
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sys := body["system"].([]any)[0].(map[string]any); sys["cache_control"] != nil {
		t.Fatalf("short system requested a cache write: %v", sys["cache_control"])
	}
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	if cc := tools[0].(map[string]any)["cache_control"]; cc != nil {
		t.Fatalf("short prefix requested a cache write: %v", cc)
	}
}

// TestFromMessagePopulatesCacheUsage pins the accounting half of the
// Anthropic translator: the cache counters flow from the streamed usage
// objects into llm.Usage, with InputTokens staying uncached.
func TestFromMessagePopulatesCacheUsage(t *testing.T) {
	srv := messagesServer(t, nil, []string{
		`{"type":"message_start","message":{"id":"msg_u","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"cache_read_input_tokens":30,"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30, Provider: "anthropic"}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
	// The three input counters sum to the provider-reported input total.
	if got := resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens; got != 60 {
		t.Fatalf("input counters sum = %d, want 60", got)
	}
}

// TestProviderWithoutUsageYieldsZeroedCounters pins the failure path: a
// response carrying no usage object must not panic and must yield zeroed
// counters (the broker then keeps the reservation charged).
func TestProviderWithoutUsageYieldsZeroedCounters(t *testing.T) {
	srv := messagesServer(t, nil, []string{
		`{"type":"message_start","message":{"id":"msg_z","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":null}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":null}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The counters must be zeroed; Provider is attribution metadata and is
	// set regardless (the translator still knows which provider spoke).
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 ||
		resp.Usage.CacheCreationInputTokens != 0 || resp.Usage.CacheReadInputTokens != 0 {
		t.Fatalf("usage = %+v, want zeroed counters", resp.Usage)
	}
}

// TestPromptCachingTwoCallsReportCacheReads pins the caching behaviour end
// to end: two consecutive Complete calls sharing an identical system prompt
// and tool set report CacheReadInputTokens > 0 on the second (the provider's
// usage object says the prefix came from cache).
func TestPromptCachingTwoCallsReportCacheReads(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		var usage string
		if n == 1 {
			usage = `{"input_tokens":2000,"cache_creation_input_tokens":2000,"output_tokens":1}`
		} else {
			usage = `{"input_tokens":50,"cache_read_input_tokens":2000,"output_tokens":1}`
		}
		fmt.Fprintf(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_p","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":%s}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`, usage)
	}))
	defer srv.Close()
	system := strings.Repeat("stable system prompt for prompt caching tests. ", 120)
	tools := []llm.Tool{{
		Name:        "bash",
		Description: strings.Repeat("run commands in a sandbox. ", 40),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}}
	p := New("k", srv.URL)
	first, err := p.Complete(context.Background(), llm.Request{System: system, Messages: []llm.Message{llm.UserText("turn one")}, Tools: tools}, nil)
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	second, err := p.Complete(context.Background(), llm.Request{System: system, Messages: []llm.Message{llm.UserText("turn two")}, Tools: tools}, nil)
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if first.Usage.CacheReadInputTokens != 0 || first.Usage.CacheCreationInputTokens != 2000 {
		t.Fatalf("first usage = %+v, want cache creation only", first.Usage)
	}
	if second.Usage.CacheReadInputTokens <= 0 {
		t.Fatalf("second usage = %+v, want CacheReadInputTokens > 0", second.Usage)
	}
	if second.Usage.InputTokens != 50 {
		t.Fatalf("second uncached input = %d, want 50", second.Usage.InputTokens)
	}
}

// TestPromptCachingBilledInputBelowNPrefix numerically asserts the point of
// the feature: for N turns of one session whose stable prefix is cached
// after the first call, total billed input is strictly less than N x the
// prefix size (the first turn pays creation, later turns pay 0.1x reads).
func TestPromptCachingBilledInputBelowNPrefix(t *testing.T) {
	const (
		prefixTokens = 2000 // stable system + tools prefix
		turns        = 5
		uncached     = 100 // per-turn variable input
	)
	billed := 0.0
	for turn := 1; turn <= turns; turn++ {
		u := llm.Usage{InputTokens: uncached}
		if turn == 1 {
			u.CacheCreationInputTokens = prefixTokens
		} else {
			u.CacheReadInputTokens = prefixTokens
		}
		billed += llm.AnthropicCost.BilledInput(u)
	}
	naive := float64(turns * (prefixTokens + uncached))
	if billed >= naive {
		t.Fatalf("billed %v >= naive %v: caching made N turns no cheaper", billed, naive)
	}
	if billed != float64(uncached*turns)+1.25*float64(prefixTokens)+0.1*float64(prefixTokens*(turns-1)) {
		t.Fatalf("billed = %v, want the numeric cache-write/read arithmetic", billed)
	}
}

// TestSystemExtraEmitsSecondUncachedSystemBlock pins finding 2 of the #247
// review: when a Request carries a changing SystemExtra (the agent's
// per-run context summary), the translator emits it as a SECOND system
// block without cache_control, so the first block — the byte-stable System
// prefix — keeps its ephemeral breakpoint and stays reusable across calls.
func TestSystemExtraEmitsSecondUncachedSystemBlock(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	// ~1100 tokens of stable system text: clears minCacheableTokens.
	system := strings.Repeat("waffle is a personal assistant with a stable system prompt. ", 90)
	const extra = "[CONTEXT SUMMARY turns=1-4 — generated for bounding only] prior work done"
	if _, err := p.Complete(context.Background(), llm.Request{
		System:      system,
		SystemExtra: extra,
		Messages:    []llm.Message{llm.UserText("hi")},
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	blocks := body["system"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("system blocks = %d, want 2 (stable prefix + uncached extra)", len(blocks))
	}
	first := blocks[0].(map[string]any)
	if first["text"] != system {
		t.Fatalf("first block text does not equal the stable System")
	}
	cc, ok := first["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Fatalf("first block cache_control = %v, want ephemeral breakpoint", first["cache_control"])
	}
	second := blocks[1].(map[string]any)
	if second["text"] != extra {
		t.Fatalf("second block text = %q, want %q", second["text"], extra)
	}
	if second["cache_control"] != nil {
		t.Fatalf("SystemExtra block carries a cache breakpoint: %v", second["cache_control"])
	}
}

// TestSystemExtraKeepsSystemPrefixByteStableAcrossCalls pins the point of
// the split end to end: two calls with the same System but different
// SystemExtra emit a byte-identical first system block (the cached span),
// while only the uncached second block varies.
func TestSystemExtraKeepsSystemPrefixByteStableAcrossCalls(t *testing.T) {
	system := strings.Repeat("stable system prompt for caching tests. ", 90)
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	defer srv.Close()
	p := New("k", srv.URL)
	for _, extra := range []string{"[CONTEXT SUMMARY turns=1-2] first summary", "[CONTEXT SUMMARY turns=1-3] second, longer summary"} {
		if _, err := p.Complete(context.Background(), llm.Request{
			System:      system,
			SystemExtra: extra,
			Messages:    []llm.Message{llm.UserText("hi")},
		}, nil); err != nil {
			t.Fatalf("Complete(%q): %v", extra, err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("captured %d request bodies, want 2", len(bodies))
	}
	firstA, _ := json.Marshal(bodies[0]["system"].([]any)[0])
	firstB, _ := json.Marshal(bodies[1]["system"].([]any)[0])
	if !bytes.Equal(firstA, firstB) {
		t.Fatalf("cached system block differs across calls: %s vs %s", firstA, firstB)
	}
	extraA, _ := json.Marshal(bodies[0]["system"].([]any)[1])
	extraB, _ := json.Marshal(bodies[1]["system"].([]any)[1])
	if bytes.Equal(extraA, extraB) {
		t.Fatalf("uncached extra block identical across calls: %s", extraA)
	}
}

// TestSystemExtraSuppressesToolsBreakpoint pins that a tools breakpoint is
// not emitted while a changing SystemExtra is present: the tools breakpoint
// caches everything before it, including the extra block, so its prefix
// would differ every call — the breakpoint would only pay the cache-write
// surcharge and never reuse an entry (#247 review).
func TestSystemExtraSuppressesToolsBreakpoint(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_t","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	system := strings.Repeat("stable system prompt for tools caching tests. ", 90)
	bigSchema := json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string","description":"` +
		strings.Repeat("a long description that costs tokens ", 12) + `"}},"required":["cmd"]}`)
	req := func(extra string) llm.Request {
		return llm.Request{
			System:      system,
			SystemExtra: extra,
			Messages:    []llm.Message{llm.UserText("hi")},
			Tools:       []llm.Tool{{Name: "bash", Description: strings.Repeat("run commands. ", 40), InputSchema: bigSchema}},
		}
	}
	if _, err := p.Complete(context.Background(), req("[CONTEXT SUMMARY turns=1-2] summary"), nil); err != nil {
		t.Fatalf("Complete with SystemExtra: %v", err)
	}
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	if cc := tools[0].(map[string]any)["cache_control"]; cc != nil {
		t.Fatalf("tools breakpoint emitted under a changing SystemExtra: %v", cc)
	}
	// Without SystemExtra the same prefix clears the minimum and the tools
	// breakpoint returns (covered in detail by
	// TestPromptCachingBreakpointsOnStablePrefix).
	if _, err := p.Complete(context.Background(), req(""), nil); err != nil {
		t.Fatalf("Complete without SystemExtra: %v", err)
	}
	if cc := body["tools"].([]any)[0].(map[string]any)["cache_control"]; cc == nil {
		t.Fatal("tools breakpoint missing once SystemExtra is empty")
	}
}

// TestSystemExtraAloneWhenSystemEmpty pins the degenerate split: with an
// empty stable System the extra text becomes the only system block and gets
// no cache breakpoint (its bytes change between calls).
func TestSystemExtraAloneWhenSystemEmpty(t *testing.T) {
	var body map[string]any
	srv := messagesServer(t, &body, []string{
		`{"type":"message_start","message":{"id":"msg_e","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	const extra = "[CONTEXT SUMMARY turns=1-2] summary only"
	if _, err := p.Complete(context.Background(), llm.Request{
		SystemExtra: extra,
		Messages:    []llm.Message{llm.UserText("hi")},
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	blocks := body["system"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("system blocks = %d, want 1", len(blocks))
	}
	block := blocks[0].(map[string]any)
	if block["text"] != extra {
		t.Fatalf("system text = %q, want %q", block["text"], extra)
	}
	if block["cache_control"] != nil {
		t.Fatalf("changing extra text requested a cache write: %v", block["cache_control"])
	}
}

// TestFromMessageTranslatesProviderCitations pins the provider-neutral source
// contract (#479): web-search citations become safe web sources and document
// citations become opaque workspace resources; snippets are bounded.
func TestFromMessageTranslatesProviderCitations(t *testing.T) {
	srv := messagesServer(t, nil, []string{
		`{"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"An answer."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","title":"Waffle docs","url":"https://example.com/docs","cited_text":"the cited line","document_index":0,"file_id":"file_web_1"}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","document_title":"Project plan","document_index":0,"file_id":"file_42","cited_text":"a plan line","start_char_index":0,"end_char_index":8}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Message.Blocks) != 1 || resp.Message.Blocks[0].Type != llm.BlockText {
		t.Fatalf("blocks = %+v", resp.Message.Blocks)
	}
	citations := resp.Message.Blocks[0].Citations
	if len(citations) != 2 {
		t.Fatalf("citations = %+v, want 2", citations)
	}
	web := citations[0]
	if web.ID != "c1" || web.Kind != llm.CitationWeb || web.Label != "Waffle docs" || web.URL != "https://example.com/docs" {
		t.Fatalf("web citation = %+v", web)
	}
	if web.Snippet != "the cited line" {
		t.Fatalf("web snippet = %q", web.Snippet)
	}
	doc := citations[1]
	if doc.ID != "c2" || doc.Kind != llm.CitationWorkspace || doc.Label != "Project plan" || doc.Resource != "file_42" {
		t.Fatalf("document citation = %+v", doc)
	}
	if doc.URL != "" {
		t.Fatalf("document citation must never carry a URL: %+v", doc)
	}
}

// TestFromMessageBoundedSnippets caps provider cited-text so hostile or
// oversized snippets cannot bloat persisted turns or the Desk drawer.
func TestFromMessageBoundedSnippets(t *testing.T) {
	long := strings.Repeat("x", 500)
	srv := messagesServer(t, nil, []string{
		`{"type":"message_start","message":{"id":"msg_l","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","title":"T","url":"https://example.com/x","cited_text":"` + long + `","document_index":0,"file_id":"f"}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	})
	defer srv.Close()
	p := New("k", srv.URL)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	snippet := resp.Message.Blocks[0].Citations[0].Snippet
	if len(snippet) > 283 {
		t.Fatalf("snippet length = %d, want bounded (280 chars + ellipsis)", len(snippet))
	}
}
