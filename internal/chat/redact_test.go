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

// TestRedactArtifactsScrubsDisplayMetadata pins the artifact boundary (#480):
// artifact names, media types, digests, and tool names pass through the
// exact-value redactor; the opaque ID is left for the client to address.
func TestRedactArtifactsScrubsDisplayMetadata(t *testing.T) {
	redact := func(s string) string { return strings.ReplaceAll(s, "sk-artifact-secret", "[redacted]") }
	out := RedactArtifacts([]Artifact{
		{ID: "art-1", Name: "sk-artifact-secret.md", MediaType: "text/markdown", Digest: "sk-artifact-secret", ToolName: "write_artifact"},
	}, redact)
	if out[0].ID != "art-1" {
		t.Fatalf("opaque id changed: %q", out[0].ID)
	}
	if out[0].Name != "[redacted].md" || out[0].Digest != "[redacted]" {
		t.Fatalf("artifact metadata = %+v", out[0])
	}
}

// TestRedactMessageScrubsCitationMetadata pins the source boundary (#479):
// citation labels, URLs, resources, snippets, and provenance all pass
// through the exact-value redactor before projection, and the original
// message is unchanged.
func TestRedactMessageScrubsCitationMetadata(t *testing.T) {
	msg := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
		Type: llm.BlockText,
		Text: "answer with sk-cite-secret",
		Citations: []llm.Citation{
			{
				ID: "s1", Kind: llm.CitationWeb, Label: "sk-cite-secret docs",
				URL: "https://example.com/sk-cite-secret", Snippet: "sk-cite-secret line",
				Provenance: "provider citation",
			},
			{ID: "s2", Kind: llm.CitationWorkspace, Label: "Plan", Resource: "/var/lib/waffle/private/plan.md"},
		},
	}}}
	redact := func(s string) string { return strings.ReplaceAll(s, "sk-cite-secret", "[redacted]") }
	out := RedactMessage(msg, redact)
	citations := out.Blocks[0].Citations
	if citations[0].Label != "[redacted] docs" {
		t.Errorf("label = %q", citations[0].Label)
	}
	if citations[0].URL != "https://example.com/[redacted]" {
		t.Errorf("url = %q", citations[0].URL)
	}
	if citations[0].Snippet != "[redacted] line" {
		t.Errorf("snippet = %q", citations[0].Snippet)
	}
	if msg.Blocks[0].Citations[0].Label != "sk-cite-secret docs" {
		t.Errorf("original citation mutated: %q", msg.Blocks[0].Citations[0].Label)
	}
}
