package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llmtest"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

func runtimeGateway(t *testing.T, provider llm.Provider, tools tool.Toolbox, limits usage.Limits) (*Gateway, *fakeAdapter, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	entities := entity.New(st, sessions)
	group, err := entities.GroupFor(ctx, "fake", "owner-chat", "main")
	if err != nil {
		t.Fatal(err)
	}
	adapter := newFakeAdapter()
	usageStore := usage.New(st)
	a := &agent.Agent{Provider: provider, Tools: tools, Model: "m", Usage: usageStore, Limits: limits}
	return &Gateway{Agent: a, Entities: entities, Sessions: sessions, Adapters: []channel.Adapter{adapter}, Usage: usageStore}, adapter, group.SessionID
}

func TestGatewayUsageAlertConfiguredThresholdDeliversOnce(t *testing.T) {
	first := llmtest.Text("one")
	first.Usage = llm.Usage{InputTokens: 50}
	second := llmtest.Text("two")
	second.Usage = llm.Usage{InputTokens: 10}
	provider := &llmtest.Script{Responses: []llm.Response{first, second}}
	limits := usage.Limits{TokensPerDay: 100, AlertThresholdPercent: 50}
	gw, adapter, _ := runtimeGateway(t, provider, tool.NewRegistry(), limits)

	for _, text := range []string{"first", "second"} {
		if _, err := gw.converse(context.Background(), channel.Message{Channel: "fake", ChatID: "owner-chat", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	adapter.mu.Lock()
	notices := append([]string(nil), adapter.sent["owner-chat"]...)
	adapter.mu.Unlock()
	if len(notices) != 1 || !strings.Contains(notices[0], "50%") {
		t.Fatalf("owner notices = %v, want one 50%% threshold notice", notices)
	}
}

func TestGatewayFetchTaintsRememberProvenance(t *testing.T) {
	provider := &llmtest.Script{Responses: []llm.Response{
		llmtest.ToolCall("fetch", "fetch-1", `{}`),
		llmtest.ToolCall("remember", "remember-1", `{"note":"derived fact"}`),
		llmtest.ToolCall("distill_skill", "distill-1", `{"name":"derived-skill","description":"derived procedure","body":"step one is sufficiently detailed then step two"}`),
		llmtest.Text("done"),
	}}
	ws := memory.Workspace{Dir: t.TempDir()}
	gate := &memory.Gate{Mode: "auto", WS: ws}
	tools := tool.NewRegistry(gwNamedTool("fetch"), memory.RememberTool{WS: ws, Gate: gate}, memory.DistillTool{WS: ws, Gate: gate})
	gw, _, sessionID := runtimeGateway(t, provider, tools, usage.Limits{})
	if _, err := gw.converse(context.Background(), channel.Message{Channel: "fake", ChatID: "owner-chat", Text: "research"}); err != nil {
		t.Fatal(err)
	}
	pending, err := gate.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	for _, candidate := range pending {
		p := candidate.Provenance
		if p.SessionID != sessionID || p.Channel != "fake" || !p.UntrustedContext {
			t.Fatalf("%s provenance = %+v, want session=%s channel=fake fetch-tainted", candidate.Kind, p, sessionID)
		}
	}
}

func TestGatewayNotifyGateDeliversOwnerDiff(t *testing.T) {
	provider := &llmtest.Script{Responses: []llm.Response{
		llmtest.ToolCall("remember", "remember-1", `{"note":"visible change"}`),
		llmtest.ToolCall("distill_skill", "distill-1", `{"name":"visible-skill","description":"visible procedure","body":"step one is sufficiently detailed then step two"}`),
		llmtest.Text("done"),
	}}
	ws := memory.Workspace{Dir: t.TempDir()}
	gate := &memory.Gate{Mode: "notify", WS: ws}
	tools := tool.NewRegistry(memory.RememberTool{WS: ws, Gate: gate}, memory.DistillTool{WS: ws, Gate: gate})
	gw, adapter, _ := runtimeGateway(t, provider, tools, usage.Limits{})
	if _, err := gw.converse(context.Background(), channel.Message{Channel: "fake", ChatID: "owner-chat", Text: "remember"}); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	notices := append([]string(nil), adapter.sent["owner-chat"]...)
	adapter.mu.Unlock()
	if len(notices) != 2 || !strings.Contains(strings.Join(notices, "\n"), "+ visible change") || !strings.Contains(strings.Join(notices, "\n"), "visible-skill") {
		t.Fatalf("owner notices = %v", notices)
	}
	if body, err := os.ReadFile(ws.MemoryPath()); err != nil || !strings.Contains(string(body), "visible change") {
		t.Fatalf("live memory = %q, %v", body, err)
	}
}
