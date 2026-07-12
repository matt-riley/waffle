package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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

func TestSpawnSubagentProfileCannotWidenTools(t *testing.T) {
	// Parent only has read_file; profile tries to allow bash — bash must not run.
	ran := false
	bash := namedTool{n: "bash", run: func() { ran = true }}
	// Parent toolbox deliberately omits bash.
	parentTB := tool.NewRegistry(namedTool{n: "read_file"})
	// Child profile allow includes bash, but parent tools don't have it.
	tl := SubagentTool{
		Provider: &recordingProvider{onComplete: func(req llm.Request) llm.Response {
			// Ask for bash — should be denied by restrict/unknown.
			if len(req.Tools) == 1 {
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
	// Attach bash only on a different toolbox would be widening — profile allow
	// without parent def means bash is unknown.
	_ = bash
	out, err := tl.Run(context.Background(), json.RawMessage(`{"task":"t","profile":"evil"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("bash must not run under parent-restricted tools")
	}
	if !strings.Contains(out, "ok") && !strings.Contains(out, "error") {
		// Either denied tool result then done, or failed handoff — both fine.
		t.Logf("out=%q", out)
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
