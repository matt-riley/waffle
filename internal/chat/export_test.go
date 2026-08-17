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

func TestExportMessagesEmitsMediaPlaceholders(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := llm.NewDocumentBlock("application/pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{img}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "See this"}, doc}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, Text: ""}}},
	}
	exported := ExportMessages(messages)
	if len(exported) != 2 {
		t.Fatalf("exported = %d, want 2 (image-only kept, tool carrier omitted)", len(exported))
	}
	if exported[0].Text != "[image: image/png]" {
		t.Fatalf("image-only = %q, want [image: image/png]", exported[0].Text)
	}
	if exported[1].Text != "See this[document: application/pdf]" {
		t.Fatalf("mixed = %q", exported[1].Text)
	}
	markdown := ExportMarkdown("Shot review", "", messages)
	if !strings.Contains(markdown, "[image: image/png]") {
		t.Fatal("markdown omitted the image placeholder")
	}
}
