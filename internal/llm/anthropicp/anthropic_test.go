package anthropicp

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
