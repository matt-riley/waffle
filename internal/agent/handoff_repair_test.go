package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

func TestHandoffRepairAttempt(t *testing.T) {
	calls := 0
	p := &recordingProvider{onComplete: func(req llm.Request) llm.Response {
		calls++
		if calls == 1 {
			// First reply: prose, no JSON.
			return llm.Response{
				StopReason: llm.StopEndTurn,
				Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "all done, no json"}}},
			}
		}
		// Repair reply.
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"repaired\"}\n```"},
			}},
		}
	}}
	tl := SubagentTool{Provider: p, Tools: tool.NewRegistry(), Model: "m"}
	out, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("expected repair Complete, calls=%d", calls)
	}
	if !strings.Contains(out, "repaired") {
		t.Fatalf("out=%q", out)
	}
}

func TestObservedVerificationDowngrades(t *testing.T) {
	// Child reports done without verification; observed bash fails.
	p := &recordingProvider{onComplete: func(req llm.Request) llm.Response {
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"claimed done\"}\n```"},
			}},
		}
	}}
	failBash := namedTool{n: "bash", run: nil}
	// Override Run to error via tool.Restrict won't work; use custom.
	tb := tool.NewRegistry(failingBash{})
	tl := SubagentTool{Provider: p, Tools: tb, Model: "m"}
	out, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","verify_commands":["go test"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "partial") && !strings.Contains(out, "fail") {
		t.Fatalf("expected verification downgrade: %q", out)
	}
	_ = failBash
}

type failingBash struct{}

func (failingBash) Def() llm.Tool {
	return llm.Tool{Name: "bash", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (failingBash) Run(context.Context, json.RawMessage) (string, error) {
	return "error: exit 1", nil
}
