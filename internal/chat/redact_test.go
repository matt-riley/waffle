package chat

import (
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

// TestRedactMessageRunsOnTextPartsOfMixedContent pins the acceptance
// criterion that secret redaction still runs on the text parts of a
// mixed-content message: block-carrying tool results and media URL
// references are scrubbed, while binary base64 payloads are left untouched.
func TestRedactMessageRunsOnTextPartsOfMixedContent(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	urlImg, err := llm.NewImageBlockFromURL("image/png", "https://example.com/sk-secret-value.png")
	if err != nil {
		t.Fatal(err)
	}
	msg := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: "see sk-secret-value"},
		img,
		urlImg,
		{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{
			ToolUseID: "t1",
			Blocks:    []llm.Block{{Type: llm.BlockText, Text: "token sk-secret-value inside"}, img},
		}},
	}}
	redact := func(s string) string { return strings.ReplaceAll(s, "sk-secret-value", "[redacted]") }
	out := RedactMessage(msg, redact)

	if out.Blocks[0].Text != "see [redacted]" {
		t.Errorf("text block = %q", out.Blocks[0].Text)
	}
	if out.Blocks[1].Source.Data != img.Source.Data {
		t.Errorf("base64 payload touched: %q", out.Blocks[1].Source.Data)
	}
	if out.Blocks[2].Source.URL != "https://example.com/[redacted].png" {
		t.Errorf("url source not redacted: %q", out.Blocks[2].Source.URL)
	}
	tr := out.Blocks[3].ToolResult
	if tr.Blocks[0].Text != "token [redacted] inside" {
		t.Errorf("tool result text part = %q", tr.Blocks[0].Text)
	}
	if tr.Blocks[1].Source.Data != img.Source.Data {
		t.Errorf("tool result media payload touched: %q", tr.Blocks[1].Source.Data)
	}
	// The original message is unchanged (projection returns a copy).
	if msg.Blocks[0].Text != "see sk-secret-value" {
		t.Errorf("original mutated: %q", msg.Blocks[0].Text)
	}
}
