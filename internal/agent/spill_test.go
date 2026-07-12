package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

type secretMidTool struct{}

func (secretMidTool) Def() llm.Tool {
	return llm.Tool{Name: "secret_mid", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (secretMidTool) Run(context.Context, json.RawMessage) (string, error) {
	// Secret buried past OutputLimit head region so truncation alone wouldn't hide it from spill.
	return strings.Repeat("A", tool.OutputLimit/2) + "sk-super-secret-value-NEVER-DISK" + strings.Repeat("B", tool.OutputLimit), nil
}

func TestRedactBeforeSpillSecretNeverOnDisk(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sp := &spill.Store{DB: st.DB}
	const secret = "sk-super-secret-value-NEVER-DISK"
	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "secret_mid", Input: json.RawMessage(`{}`)}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{
		Provider: p,
		Tools:    tool.NewRegistry(secretMidTool{}),
		Model:    "m",
		Spill:    sp,
		Redact:   func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") },
	}
	ctx = WithSession(ctx, "sess-redact")
	if _, err := a.Run(ctx, []llm.Message{llm.UserText("go")}, Hooks{}); err != nil {
		t.Fatal(err)
	}
	// Full spill body on disk must not contain the secret.
	var content string
	err = st.DB.QueryRowContext(ctx, `SELECT content FROM tool_spills WHERE session_id = ?`, "sess-redact").Scan(&content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, secret) {
		t.Fatal("secret present on disk after redact-before-spill")
	}
	if !strings.Contains(content, "[redacted]") {
		t.Fatal("expected redacted placeholder in spill")
	}
}

// TestSpillFromBuiltinReadFile verifies host builtins that used to Truncate
// to OutputLimit now return enough bytes for Agent.runOne to spill (#69).
func TestSpillFromBuiltinReadFile(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sp := &spill.Store{DB: st.DB}

	path := filepath.Join(t.TempDir(), "huge.txt")
	// Unique token past the head of OutputLimit so only a full return (then spill) preserves it.
	body := strings.Repeat("Q", tool.OutputLimit+50) + "BUILTIN_SPILL_TOKEN_ZZ" + strings.Repeat("q", 200)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{responses: []llm.Response{
		{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{
					ID: "t1", Name: "read_file",
					Input: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)),
				}},
			}},
			StopReason: llm.StopToolUse,
		},
		{Message: assistantText("ok"), StopReason: llm.StopEndTurn},
	}}
	a := &Agent{
		Provider: p,
		Tools:    tool.NewRegistry(tool.ReadFile{}),
		Model:    "m",
		Spill:    sp,
	}
	ctx = WithSession(ctx, "sess-builtin-spill")
	hist, err := a.Run(ctx, []llm.Message{llm.UserText("read it")}, Hooks{})
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
	if !strings.Contains(content, "expand_output") {
		t.Fatalf("expected spill marker in tool result, got %q…", truncateForLog(content, 160))
	}
	var stored string
	if err := st.DB.QueryRowContext(ctx, `SELECT content FROM tool_spills WHERE session_id = ?`, "sess-builtin-spill").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "BUILTIN_SPILL_TOKEN_ZZ") {
		t.Fatal("spill body missing token that only exists past OutputLimit")
	}
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
