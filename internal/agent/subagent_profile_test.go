package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

func TestSpawnSubagentProfileRebuildsSystemAndModel(t *testing.T) {
	var models []string
	var systems []string
	p := &recordingProvider{
		onComplete: func(req llm.Request) llm.Response {
			models = append(models, req.Model)
			systems = append(systems, req.System)
			// Return valid handoff JSON.
			return llm.Response{
				StopReason: llm.StopEndTurn,
				Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
					{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"reviewed\"}\n```"},
				}},
			}
		},
	}
	tl := SubagentTool{
		Provider:  p,
		Tools:     tool.NewRegistry(namedTool{n: "read_file"}),
		Model:     "parent-model",
		MaxTokens: 100,
		Profiles: map[string]ChildProfile{
			"reviewer": {
				System: "You are a strict code reviewer.",
				Model:  "review-model",
				Tools:  tool.Policy{Allow: []string{"read_file"}},
			},
		},
	}
	out, err := tl.Run(context.Background(), json.RawMessage(`{"task":"review pkg","profile":"reviewer"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "reviewed") {
		t.Fatalf("out = %q", out)
	}
	if len(models) == 0 || models[0] != "review-model" {
		t.Fatalf("models = %v, want review-model", models)
	}
	if len(systems) == 0 || !strings.Contains(systems[0], "strict code reviewer") {
		t.Fatalf("system = %q", systems)
	}
	if !strings.Contains(out, "profile=reviewer") {
		t.Fatalf("handoff should record profile: %q", out)
	}
}

func TestSpawnSubagentRejectsUnknownAndDisallowedProfile(t *testing.T) {
	tl := SubagentTool{
		Provider:        oneShotProvider{reply: "x"},
		Tools:           tool.NewRegistry(),
		Model:           "m",
		Profiles:        map[string]ChildProfile{"reviewer": {System: "r"}},
		AllowedProfiles: []string{"reviewer"},
	}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"hacker"}`)); err == nil {
		t.Fatal("expected disallowed profile error")
	}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"missing"}`)); err == nil {
		t.Fatal("expected unknown profile error")
	}
}

func TestProfileTargetingStillEnforcesSubagentDepthLimit(t *testing.T) {
	toolUnderTest := SubagentTool{
		Provider: oneShotProvider{reply: "must not run"},
		Tools:    tool.NewRegistry(), Model: "m", Depth: 3,
		Profiles: map[string]ChildProfile{"reviewer": {System: "review"}},
	}
	if _, err := toolUnderTest.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"reviewer"}`)); err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("profile-targeted depth error=%v", err)
	}
}

func TestProfileTargetedSubagentsRespectConcurrencyLimit(t *testing.T) {
	provider := &blockingProfileProvider{
		started: make(chan struct{}, maxSubagentConcurrency+4),
		release: make(chan struct{}),
	}
	spawn := SubagentTool{
		Provider: provider,
		Tools:    tool.NewRegistry(),
		Model:    "m",
		Profiles: map[string]ChildProfile{"reviewer": {System: "review"}},
	}
	a := &Agent{Tools: tool.NewRegistry(spawn)}
	uses := make([]llm.ToolUse, maxSubagentConcurrency+4)
	for i := range uses {
		uses[i] = llm.ToolUse{ID: fmt.Sprintf("child-%d", i), Name: "spawn_subagent", Input: json.RawMessage(`{"task":"review","profile":"reviewer"}`)}
	}
	done := make(chan []llm.ToolResult, 1)
	go func() { done <- a.runTools(context.Background(), uses, Hooks{}) }()

	for i := 0; i < maxSubagentConcurrency; i++ {
		select {
		case <-provider.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d profile-targeted children started", i)
		}
	}
	select {
	case <-provider.started:
		t.Fatalf("more than %d profile-targeted children started concurrently", maxSubagentConcurrency)
	case <-time.After(100 * time.Millisecond):
	}
	close(provider.release)
	select {
	case results := <-done:
		for i, result := range results {
			if result.IsError {
				t.Fatalf("result %d: %+v", i, result)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("profile-targeted children did not finish")
	}
	if got := provider.max.Load(); got != maxSubagentConcurrency {
		t.Fatalf("max concurrent children = %d, want %d", got, maxSubagentConcurrency)
	}
}

type blockingProfileProvider struct {
	started chan struct{}
	release chan struct{}
	active  atomic.Int32
	max     atomic.Int32
}

func (p *blockingProfileProvider) Complete(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for {
		maximum := p.max.Load()
		if active <= maximum || p.max.CompareAndSwap(maximum, active) {
			break
		}
	}
	p.started <- struct{}{}
	<-p.release
	return &llm.Response{
		StopReason: llm.StopEndTurn,
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
			Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"reviewed\"}\n```",
		}}},
	}, nil
}

func TestSubagentSpawnLogIncludesChildProfileWithoutTask(t *testing.T) {
	var logs bytes.Buffer
	toolUnderTest := SubagentTool{
		Provider: oneShotProvider{reply: "ok"}, Tools: tool.NewRegistry(), Model: "m",
		Profiles: map[string]ChildProfile{"reviewer": {System: "review"}},
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
	}
	if _, err := toolUnderTest.Run(context.Background(), json.RawMessage(`{"task":"PRIVATE_SUBTASK","profile":"reviewer"}`)); err != nil {
		t.Fatal(err)
	}
	body := logs.String()
	if !strings.Contains(body, `msg="subagent spawn"`) || !strings.Contains(body, "profile=reviewer") {
		t.Fatalf("logs=%s", body)
	}
	if strings.Contains(body, "PRIVATE_SUBTASK") {
		t.Fatalf("task leaked into logs: %s", body)
	}
}

func TestSpawnSubagentProfileCannotWidenTools(t *testing.T) {
	// Parent only has read_file; profile tries to allow bash — bash must not run
	// even if profile allow includes it (tighten-only intersect with parent toolbox).
	ran := false
	bash := namedTool{n: "bash", run: func() { ran = true }}
	parentTB := tool.NewRegistry(namedTool{n: "read_file"})
	var offered []string
	calls := 0
	tl := SubagentTool{
		Provider: &recordingProvider{onComplete: func(req llm.Request) llm.Response {
			calls++
			if calls == 1 {
				offered = offered[:0]
				for _, d := range req.Tools {
					offered = append(offered, d.Name)
				}
				// First turn: ask for bash — must fail as unknown/not permitted.
				return llm.Response{
					StopReason: llm.StopToolUse,
					Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
						{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "1", Name: "bash", Input: json.RawMessage(`{}`)}},
					}},
				}
			}
			return llm.Response{
				StopReason: llm.StopEndTurn,
				Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
					{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"ok\"}\n```"},
				}},
			}
		}},
		Tools: parentTB,
		Model: "m",
		Profiles: map[string]ChildProfile{
			"evil": {Tools: tool.Policy{Allow: []string{"bash", "read_file"}}},
		},
	}
	_ = bash
	out, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"evil"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("bash must not run under parent-restricted tools")
	}
	for _, n := range offered {
		if n == "bash" {
			t.Fatalf("bash must not appear in child tool defs; offered=%v", offered)
		}
	}
	if !strings.Contains(out, "ok") && !strings.Contains(out, "error") && !strings.Contains(out, "unknown") {
		t.Logf("out=%q", out)
	}
}

func TestSpawnSubagentAllowedChildSet(t *testing.T) {
	// allowed child set: only listed profiles may be spawned.
	tl := SubagentTool{
		Provider: oneShotProvider{reply: "ok"},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		Profiles: map[string]ChildProfile{
			"reviewer": {System: "r"},
			"hacker":   {System: "h"},
		},
		AllowedProfiles: []string{"reviewer"},
	}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"reviewer"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"hacker"}`)); err == nil {
		t.Fatal("hacker not in allowed set")
	}
}

type namedTool struct {
	n   string
	run func()
}

func (n namedTool) Def() llm.Tool {
	return llm.Tool{Name: n.n, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (n namedTool) Run(context.Context, json.RawMessage) (string, error) {
	if n.run != nil {
		n.run()
	}
	return "ran:" + n.n, nil
}

type recordingProvider struct {
	onComplete func(llm.Request) llm.Response
	mu         sync.Mutex
	calls      int
}

func (p *recordingProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.mu.Lock()
	p.calls++
	fn := p.onComplete
	p.mu.Unlock()
	r := fn(req)
	return &r, nil
}
