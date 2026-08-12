package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llmtest"
	"github.com/matt-riley/waffle/internal/notify"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

// TestGatewayNotifyToolDeliversOwnerMessage covers the notify tool end to
// end: the agent calls notify mid-run, the gateway resolves the destination
// from the session origin (message channel + chat id), and the run finishes
// normally after the notification.
func TestGatewayNotifyToolDeliversOwnerMessage(t *testing.T) {
	provider := &llmtest.Script{Responses: []llm.Response{
		llmtest.ToolCall("notify", "notify-1", `{"message":"60% through the migration"}`),
		llmtest.Text("done"),
	}}
	tools := tool.NewRegistry(notify.Tool{})
	gw, adapter, _ := runtimeGateway(t, provider, tools, usage.Limits{})

	reply, err := gw.converse(context.Background(), channel.Message{Channel: "fake", ChatID: "owner-chat", Text: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	adapter.mu.Lock()
	notices := append([]string(nil), adapter.sent["owner-chat"]...)
	adapter.mu.Unlock()
	if len(notices) != 1 || !strings.Contains(notices[0], "60% through the migration") {
		t.Fatalf("owner notices = %v, want the notify message", notices)
	}
}

// TestGatewayNotifyToolFailedSendDoesNotFailRun covers the fire-and-forget
// contract: a delivery failure surfaces as a tool error to the model, but
// the run itself completes and the owner still gets the final reply.
func TestGatewayNotifyToolFailedSendDoesNotFailRun(t *testing.T) {
	provider := &llmtest.Script{Responses: []llm.Response{
		llmtest.ToolCall("notify", "notify-1", `{"message":"heads up"}`),
		llmtest.Text("done"),
	}}
	gw, adapter, _ := runtimeGateway(t, provider, tool.NewRegistry(notify.Tool{}), usage.Limits{})
	adapter.sendErr = errors.New("adapter down")

	reply, err := gw.converse(context.Background(), channel.Message{Channel: "fake", ChatID: "owner-chat", Text: "work"})
	if err != nil {
		t.Fatalf("run failed on a failed notification send: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	adapter.mu.Lock()
	notices := append([]string(nil), adapter.sent["owner-chat"]...)
	adapter.mu.Unlock()
	if len(notices) != 0 {
		t.Fatalf("owner notices = %v, want none (send failed)", notices)
	}
}

// TestGatewayNotifyToolBoundsPerRun covers the per-run budget through the
// agent loop: a model that keeps calling notify hits the limit, the run
// still finishes, and only MaxPerRun messages reach the owner.
func TestGatewayNotifyToolBoundsPerRun(t *testing.T) {
	var responses []llm.Response
	for i := 0; i < notify.MaxPerRun+3; i++ {
		responses = append(responses, llmtest.ToolCall("notify", "notify-loop", `{"message":"update"}`))
	}
	responses = append(responses, llmtest.Text("done"))
	provider := &llmtest.Script{Responses: responses}
	gw, adapter, _ := runtimeGateway(t, provider, tool.NewRegistry(notify.Tool{}), usage.Limits{})

	reply, err := gw.converse(context.Background(), channel.Message{Channel: "fake", ChatID: "owner-chat", Text: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	adapter.mu.Lock()
	notices := append([]string(nil), adapter.sent["owner-chat"]...)
	adapter.mu.Unlock()
	if len(notices) != notify.MaxPerRun {
		t.Fatalf("owner notices = %d, want %d (per-run budget)", len(notices), notify.MaxPerRun)
	}
}
