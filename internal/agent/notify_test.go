package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llmtest"
	"github.com/matt-riley/waffle/internal/notify"
	"github.com/matt-riley/waffle/internal/tool"
)

// TestAgentNotifyToolNoOriginDegradesToNoop covers sessions without a
// channel origin (terminal chat, eval): the notify tool is registered but no
// sender is attached, so a call degrades to a clear no-op — never an error
// and never a panic — and the run completes normally.
func TestAgentNotifyToolNoOriginDegradesToNoop(t *testing.T) {
	p := &fakeProvider{responses: []llm.Response{
		llmtest.ToolCall("notify", "notify-1", `{"message":"progress"}`),
		llmtest.Text("done"),
	}}
	a := &Agent{Provider: p, Tools: tool.NewRegistry(notify.Tool{}), Model: "m"}

	history, err := a.Run(context.Background(), []llm.Message{llm.UserText("work")}, Hooks{})
	if err != nil {
		t.Fatalf("Run without a channel origin failed: %v", err)
	}
	res := history[2].Blocks[0].ToolResult
	if res.IsError {
		t.Fatalf("no-origin notify returned an error result: %+v", res)
	}
	if !strings.Contains(res.Content, "no owner channel") {
		t.Fatalf("tool result = %q, want a clear no-op message", res.Content)
	}
	if !strings.Contains(history[len(history)-1].Text(), "done") {
		t.Fatalf("run did not finish normally: %+v", history[len(history)-1])
	}
}

// TestSpawnSubagentDoesNotInheritNotify covers #253's subagent rule: a
// spawn_subagent child must not silently inherit the ability to message the
// owner, even when the parent toolbox offers notify.
func TestSpawnSubagentDoesNotInheritNotify(t *testing.T) {
	var childToolNames []string
	notifyRan := false
	tl := SubagentTool{
		Provider: &recordingProvider{onComplete: func(req llm.Request) llm.Response {
			for _, td := range req.Tools {
				childToolNames = append(childToolNames, td.Name)
			}
			return llm.Response{
				StopReason: llm.StopEndTurn,
				Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
					{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"worked\"}\n```"},
				}},
			}
		}},
		Tools: tool.NewRegistry(
			namedTool{n: "read_file"},
			notify.Tool{},
		),
		Model: "m",
	}
	// The parent context carries a live sender: if the child inherited it,
	// a notify call would reach the owner. The child toolbox must not offer
	// notify at all.
	ctx := notify.WithSender(context.Background(), func(ctx context.Context, text string) error {
		notifyRan = true
		return nil
	})
	out, err := tl.Run(ctx, json.RawMessage(`{"task":"review the docs"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "worked") {
		t.Fatalf("handoff = %q", out)
	}
	for _, name := range childToolNames {
		if name == "notify" {
			t.Fatalf("child toolbox offers notify (%v); subagents must not inherit owner messaging", childToolNames)
		}
	}
	if notifyRan {
		t.Fatal("sender was invoked; child must not be able to notify")
	}
}
