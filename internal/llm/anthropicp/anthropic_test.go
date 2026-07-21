package anthropicp

import (
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
