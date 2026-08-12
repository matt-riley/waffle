package openaip

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

// TestTranslateMessageMapsImagesToImageURLParts pins the OpenAI-compatible
// wire shape for image content: user-message images become image_url content
// parts (data URI for inline base64, the URL for URL sources), interleaved
// with text parts in order.
func TestTranslateMessageMapsImagesToImageURLParts(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	imgURL, err := llm.NewImageBlockFromURL("image/jpeg", "https://example.com/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	srv := sseServer(t, &body, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	defer srv.Close()

	p := New("k", srv.URL+"/v1")
	if _, err := p.Complete(context.Background(), llm.Request{
		Model:    "m",
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "before"}, img, imgURL, {Type: llm.BlockText, Text: "after"}}}},
	}, nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	msgs := body["messages"].([]any)
	user := msgs[0].(map[string]any)
	parts := user["content"].([]any)
	if len(parts) != 4 {
		t.Fatalf("content parts = %d, want 4: %v", len(parts), parts)
	}
	want := []map[string]any{
		{"type": "text", "text": "before"},
		{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + img.Source.Data}},
		{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/photo.jpg"}},
		{"type": "text", "text": "after"},
	}
	for i, w := range want {
		if !reflect.DeepEqual(parts[i], w) {
			t.Errorf("part %d = %v, want %v", i, parts[i], w)
		}
	}
}

// TestTranslateMessageRejectsUnrepresentableBlocks pins the acceptance
// criterion that a provider which cannot represent a block type fails with
// an explicit error naming the provider and the block type — never silent
// loss. Documents are unrepresentable anywhere in this dialect; images are
// unrepresentable inside tool results (role tool content is a plain string).
func TestTranslateMessageRejectsUnrepresentableBlocks(t *testing.T) {
	img, err := llm.NewImageBlock("image/png", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := llm.NewDocumentBlock("application/pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("document in user message names provider and block type", func(t *testing.T) {
		_, err := translateMessage(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{pdf}})
		if err == nil {
			t.Fatal("document accepted")
		}
		for _, want := range []string{"openai", "document"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err %q missing %q", err, want)
			}
		}
	})

	t.Run("image in tool result names provider and block type", func(t *testing.T) {
		_, err := translateMessage(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "t1", Blocks: []llm.Block{img}}},
		}})
		if err == nil {
			t.Fatal("image tool result accepted")
		}
		for _, want := range []string{"openai", "image"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err %q missing %q", err, want)
			}
		}
	})

	t.Run("text blocks in tool result flatten to the tool message string", func(t *testing.T) {
		msgs, err := translateMessage(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "t1", Blocks: []llm.Block{{Type: llm.BlockText, Text: "a"}, {Type: llm.BlockText, Text: "b"}}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 || msgs[0].Content.Text != "ab" || msgs[0].ToolCallID != "t1" {
			t.Fatalf("tool message = %+v", msgs)
		}
	})
}

// TestToWireRejectsOversizedMediaAtCanonicalLayer pins the acceptance
// criterion that size limits live in internal/llm for this translator too:
// an over-limit image is rejected via the canonical validator with an error
// naming the provider and the limit.
func TestToWireRejectsOversizedMediaAtCanonicalLayer(t *testing.T) {
	tooBig := llm.Block{Type: llm.BlockImage, Source: &llm.BlockSource{
		Type:      llm.SourceBase64,
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(make([]byte, llm.MaxMediaBytes+1)),
	}}
	p := New("k", "http://localhost:1/v1")
	_, err := p.toWire(llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{tooBig}}},
	})
	if err == nil {
		t.Fatal("oversized image accepted")
	}
	for _, want := range []string{"openai", "limit", "image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err, want)
		}
	}
}

// TestTextOnlyWireShapeUnchanged pins that text-only requests keep the plain
// string content form (no content-part arrays) so existing endpoints see the
// exact historical shape.
func TestTextOnlyWireShapeUnchanged(t *testing.T) {
	p := New("k", "http://localhost:1/v1")
	w, err := p.toWire(llm.Request{
		Model:    "m",
		System:   "sys",
		Messages: []llm.Message{llm.UserText("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Contains(got, `"image_url"`) || strings.Contains(got, `"type":"text"`) {
		t.Fatalf("text-only request gained content parts: %s", got)
	}
	if !strings.Contains(got, `"content":"hi"`) {
		t.Fatalf("text-only content not a plain string: %s", got)
	}
}
