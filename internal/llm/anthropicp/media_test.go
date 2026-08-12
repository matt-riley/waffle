package anthropicp

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

// TestToParamsEmitsImageAndDocumentBlocks pins the wire shape of media
// blocks: images and documents become the SDK's image/document block params
// with base64 and URL sources, and tool results carrying blocks keep their
// structure inside the tool_result content union.
func TestToParamsEmitsImageAndDocumentBlocks(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	imgURL, err := llm.NewImageBlockFromURL("image/jpeg", "https://example.com/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := llm.NewDocumentBlock("application/pdf", []byte("%PDF-1.4 fake"))
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := llm.NewDocumentBlock("text/markdown", []byte("# Notes\nhello"))
	if err != nil {
		t.Fatal(err)
	}
	docURL, err := llm.NewDocumentBlockFromURL("application/pdf", "https://example.com/bill.pdf")
	if err != nil {
		t.Fatal(err)
	}
	chart, err := llm.NewImageBlock("image/png", []byte("chart-bytes"))
	if err != nil {
		t.Fatal(err)
	}

	params, err := toParams(llm.Request{
		Messages: []llm.Message{
			llm.UserText("what does this say?"),
			{Role: llm.RoleUser, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "first"},
				img,
				imgURL,
				pdf,
				markdown,
				docURL,
				{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{
					ToolUseID: "toolu_1",
					Blocks:    []llm.Block{{Type: llm.BlockText, Text: "chart:"}, chart},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("toParams: %v", err)
	}
	wire, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	messages := body["messages"].([]any)
	content := messages[1].(map[string]any)["content"].([]any)

	type srcShape struct{ srcType, mediaType, data, url string }

	// str renders an absent JSON field as "" so shape comparisons are exact.
	str := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	want := []struct {
		blockType string
		src       srcShape
	}{
		{"text", srcShape{}},
		{"image", srcShape{srcType: "base64", mediaType: "image/png", data: img.Source.Data}},
		{"image", srcShape{srcType: "url", url: "https://example.com/photo.jpg"}},
		{"document", srcShape{srcType: "base64", mediaType: "application/pdf", data: pdf.Source.Data}},
		{"document", srcShape{srcType: "text", mediaType: "text/markdown", data: "# Notes\nhello"}},
		{"document", srcShape{srcType: "url", url: "https://example.com/bill.pdf"}},
		{"tool_result", srcShape{}},
	}
	if len(content) != len(want) {
		t.Fatalf("content blocks = %d, want %d: %v", len(content), len(want), content)
	}
	for i, w := range want {
		block := content[i].(map[string]any)
		if block["type"] != w.blockType {
			t.Errorf("block %d type = %v, want %s", i, block["type"], w.blockType)
		}
		if w.blockType == "text" || w.blockType == "tool_result" {
			continue
		}
		src := block["source"].(map[string]any)
		if str(src["type"]) != w.src.srcType || str(src["media_type"]) != w.src.mediaType ||
			str(src["data"]) != w.src.data || str(src["url"]) != w.src.url {
			t.Errorf("block %d source = %v, want %+v", i, src, w.src)
		}
	}

	// Tool result content union: text and image blocks inside tool_result.
	toolResult := content[6].(map[string]any)
	toolContent := toolResult["content"].([]any)
	if len(toolContent) != 2 {
		t.Fatalf("tool result content = %v, want 2 blocks", toolContent)
	}
	if toolContent[0].(map[string]any)["type"] != "text" || toolContent[0].(map[string]any)["text"] != "chart:" {
		t.Errorf("tool result text block = %v", toolContent[0])
	}
	toolImage := toolContent[1].(map[string]any)
	if toolImage["type"] != "image" || toolImage["source"].(map[string]any)["data"] != chart.Source.Data {
		t.Errorf("tool result image block = %v", toolImage)
	}
}

// TestToParamsRejectsOversizedMediaAtCanonicalLayer pins the acceptance
// criterion that size limits live in internal/llm: an over-limit image is
// rejected by the translator via the canonical validator, with an error
// naming the limit and the provider.
func TestToParamsRejectsOversizedMediaAtCanonicalLayer(t *testing.T) {
	tooBig := llm.Block{Type: llm.BlockImage, Source: &llm.BlockSource{
		Type:      llm.SourceBase64,
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(make([]byte, llm.MaxMediaBytes+1)),
	}}
	_, err := toParams(llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{tooBig}}},
	})
	if err == nil {
		t.Fatal("oversized image accepted")
	}
	for _, want := range []string{"anthropic", "limit", "image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err, want)
		}
	}
}

// TestToBlocksUnsupportedTypeRemainsReachable asserts the existing
// unsupported-block-type default still fires for genuinely unknown types.
func TestToBlocksUnsupportedTypeRemainsReachable(t *testing.T) {
	_, err := toBlocks([]llm.Block{{Type: llm.BlockType("hologram")}})
	if err == nil || !strings.Contains(err.Error(), "unsupported block type") {
		t.Fatalf("err = %v, want unsupported block type", err)
	}
}
