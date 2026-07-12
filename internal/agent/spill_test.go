package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

type bigTool struct{}

func (bigTool) Def() llm.Tool {
	return llm.Tool{Name: "big", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (bigTool) Run(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("Z", tool.OutputLimit+100) + "TAIL", nil
}

func TestSpillOnLargeToolResult(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sp := &spill.Store{DB: st.DB}

	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "big", Input: json.RawMessage(`{}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{
		Provider: p,
		Tools:    tool.NewRegistry(bigTool{}),
		Model:    "m",
		Spill:    sp,
	}
	ctx = WithSession(ctx, "sess-spill")
	hist, err := a.Run(ctx, []llm.Message{llm.UserText("go")}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	var content string
	for _, m := range hist {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && b.ToolResult != nil {
				content = b.ToolResult.Content
			}
		}
	}
	if content == "" {
		t.Fatal("empty tool result")
	}
	if !strings.Contains(content, "expand_output") || !strings.Contains(content, "spill-") {
		snip := content
		if len(snip) > 120 {
			snip = snip[:120]
		}
		t.Fatalf("expected spill marker, got %q…", snip)
	}
	if utf8.RuneCountInString(content) > tool.OutputLimit+200 {
		// truncated body + marker should stay bounded
		t.Fatalf("result still huge: %d runes", utf8.RuneCountInString(content))
	}
}
