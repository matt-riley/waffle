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
	if resp2.StopReason != llm.StopToolUse {
		t.Fatalf("stop=%q", resp2.StopReason)
	}
	uses := resp2.ToolUses()
	if len(uses) != 1 {
		t.Fatalf("tool uses=%d", len(uses))
	}
	arg := string(uses[0].Input)
	// With cap=5, first arg chunk is 6 bytes `{"a":"` so we take [:5] = `{"a":`
	if arg != `{"a":` || len(arg) != 5 {
		t.Errorf("tool arg should be capped to first 5 bytes, got %q (len=%d)", arg, len(arg))
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
