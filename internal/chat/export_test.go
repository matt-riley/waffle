package chat

import (
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

func TestExportMessagesOnlyVisibleText(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Question"}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Answer"}, {Type: llm.BlockToolUse, Text: ""}}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolResult, Text: "secret tool payload"}}},
	}
	exported := ExportMessages(messages)
	if len(exported) != 2 {
		t.Fatalf("exported = %d, want 2 (tool carriers excluded)", len(exported))
	}
	if exported[0].Role != "user" || exported[1].Role != "assistant" {
		t.Fatalf("roles = %v, %v", exported[0].Role, exported[1].Role)
	}
	markdown := ExportMarkdown("Release review", "reviewer", messages)
	if !strings.Contains(markdown, "# Release review") {
		t.Fatal("missing title heading")
	}
	if !strings.Contains(markdown, "Profile: reviewer") {
		t.Fatal("missing profile line")
	}
	if !strings.Contains(markdown, "## You") || !strings.Contains(markdown, "## Waffle") {
		t.Fatal("missing role headings")
	}
	if strings.Contains(markdown, "secret tool payload") {
		t.Fatal("tool payload leaked into the export")
	}
}
