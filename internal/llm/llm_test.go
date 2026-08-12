package llm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture loads a checked-in persisted-turn fixture.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// TestTurnJSONEncodingIsAdditive asserts the storage contract for persisted
// turns: a turn written before image/document blocks existed unmarshals
// unchanged (no new keys, no data loss) and re-marshals byte-identically to
// the canonical old shape; a turn written after the change round-trips
// byte-identically too.
func TestTurnJSONEncodingIsAdditive(t *testing.T) {
	t.Run("pre-change turn unmarshals without new keys and re-marshals byte-identically", func(t *testing.T) {
		fixture := readFixture(t, "turn_pre_image.json")
		var msg Message
		if err := json.Unmarshal(fixture, &msg); err != nil {
			t.Fatalf("unmarshal pre-change fixture: %v", err)
		}
		if len(msg.Blocks) != 2 {
			t.Fatalf("blocks = %d, want 2", len(msg.Blocks))
		}
		tr := msg.Blocks[0].ToolResult
		if tr == nil || tr.ToolUseID != "toolu_1" || tr.Content != "deploy.sh:3: rsync --delete typo-here" || !tr.IsError {
			t.Fatalf("tool result mangled: %+v", tr)
		}
		if len(tr.Blocks) != 0 {
			t.Fatalf("pre-change tool result gained blocks: %+v", tr.Blocks)
		}
		if msg.Blocks[1].Text != "that broke it" {
			t.Fatalf("text block mangled: %+v", msg.Blocks[1])
		}
		out, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		// The additive fields must stay absent: the re-marshal is the exact
		// byte stream old waffle persisted. The top-level "blocks" key is the
		// message's own; the per-block "source" and the tool result's
		// "blocks" must not appear.
		var generic map[string]any
		if err := json.Unmarshal(out, &generic); err != nil {
			t.Fatalf("unmarshal re-marshal: %v", err)
		}
		blocks := generic["blocks"].([]any)
		toolResult := blocks[0].(map[string]any)["tool_result"].(map[string]any)
		if _, ok := toolResult["blocks"]; ok {
			t.Fatalf("tool result gained blocks key: %s", out)
		}
		if _, ok := blocks[1].(map[string]any)["source"]; ok {
			t.Fatalf("text block gained source key: %s", out)
		}
		canonical := `{"role":"user","blocks":[{"type":"tool_result","tool_result":{"tool_use_id":"toolu_1","content":"deploy.sh:3: rsync --delete typo-here","is_error":true}},{"type":"text","text":"that broke it"}]}`
		if string(out) != canonical {
			t.Fatalf("re-marshal != canonical old bytes\n got: %s\nwant: %s", out, canonical)
		}
	})

	t.Run("post-change turn round-trips byte-identically", func(t *testing.T) {
		fixture := readFixture(t, "turn_with_image.json")
		var msg Message
		if err := json.Unmarshal(fixture, &msg); err != nil {
			t.Fatalf("unmarshal post-change fixture: %v", err)
		}
		if len(msg.Blocks) != 3 {
			t.Fatalf("blocks = %d, want 3", len(msg.Blocks))
		}
		img := msg.Blocks[1]
		if img.Type != BlockImage || img.Source == nil || img.Source.Type != SourceBase64 ||
			img.Source.MediaType != "image/png" || img.Source.Data == "" {
			t.Fatalf("image block mangled: %+v", img)
		}
		tr := msg.Blocks[2].ToolResult
		if tr == nil || len(tr.Blocks) != 2 || tr.Blocks[0].Text != "chart rendered" ||
			tr.Blocks[1].Source == nil || tr.Blocks[1].Source.URL != "https://example.com/chart.png" {
			t.Fatalf("tool result blocks mangled: %+v", tr)
		}
		first, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var again Message
		if err := json.Unmarshal(first, &again); err != nil {
			t.Fatalf("unmarshal re-marshal: %v", err)
		}
		second, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("marshal again: %v", err)
		}
		if string(first) != string(second) {
			t.Fatalf("round-trip not byte-identical\n1: %s\n2: %s", first, second)
		}
	})
}

// TestNewImageBlockEnforcesCanonicalLimits pins the size and type limits in
// the canonical layer: over-limit payloads and unsupported media types are
// rejected with errors naming the limit/type.
func TestNewImageBlockEnforcesCanonicalLimits(t *testing.T) {
	t.Run("over-limit payload rejected with limit named", func(t *testing.T) {
		_, err := NewImageBlock("image/png", make([]byte, MaxMediaBytes+1))
		if err == nil {
			t.Fatal("oversized image accepted")
		}
		if !strings.Contains(err.Error(), "exceeds the") || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("error does not name the limit: %v", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", MaxMediaBytes)) {
			t.Fatalf("error does not state the byte limit: %v", err)
		}
	})

	t.Run("unsupported media type rejected with named error", func(t *testing.T) {
		_, err := NewImageBlock("image/tiff", []byte("x"))
		if !strings.Contains(err.Error(), ErrUnsupportedMediaType.Error()) {
			t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
		}
		_, err = NewDocumentBlock("application/x-unknown", []byte("x"))
		if !strings.Contains(err.Error(), ErrUnsupportedMediaType.Error()) {
			t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
		}
	})

	t.Run("case-insensitive media types accepted", func(t *testing.T) {
		if _, err := NewImageBlock("image/PNG", []byte("x")); err != nil {
			t.Fatalf("uppercase media type rejected: %v", err)
		}
	})

	t.Run("invalid base64 in source rejected without truncation", func(t *testing.T) {
		bad := Block{Type: BlockImage, Source: &BlockSource{Type: SourceBase64, MediaType: "image/png", Data: "not base64!!"}}
		err := ValidateBlock(bad)
		if err == nil || !strings.Contains(err.Error(), "invalid base64") {
			t.Fatalf("err = %v, want invalid base64 error", err)
		}
	})

	t.Run("url source length capped", func(t *testing.T) {
		long := "https://example.com/" + strings.Repeat("a", MaxMediaURLLen)
		_, err := NewImageBlockFromURL("image/png", long)
		if err == nil || !strings.Contains(err.Error(), "url") || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v, want url length error", err)
		}
	})

	t.Run("document at the limit is accepted and round-trips", func(t *testing.T) {
		payload := make([]byte, MaxMediaBytes)
		block, err := NewDocumentBlock("application/pdf", payload)
		if err != nil {
			t.Fatalf("limit-size document rejected: %v", err)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		var back Block
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		decoded, err := base64.StdEncoding.DecodeString(back.Source.Data)
		if err != nil || len(decoded) != MaxMediaBytes {
			t.Fatalf("decoded = %d bytes, %v", len(decoded), err)
		}
	})
}

// TestValidateBlocksRejectsOversizedMediaInBulk asserts the canonical
// validation used by translators rejects an over-limit block inside a longer
// message with an error naming the offending block index.
func TestValidateBlocksRejectsOversizedMediaInBulk(t *testing.T) {
	big, err := NewImageBlock("image/png", make([]byte, MaxMediaBytes/2))
	if err != nil {
		t.Fatal(err)
	}
	tooBig := Block{Type: BlockImage, Source: &BlockSource{Type: SourceBase64, MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(make([]byte, MaxMediaBytes+1))}}
	err = ValidateBlocks([]Block{{Type: BlockText, Text: "ok"}, big, tooBig})
	if err == nil {
		t.Fatal("oversized block accepted")
	}
	if !strings.Contains(err.Error(), "block 2") {
		t.Fatalf("error does not name the block index: %v", err)
	}
}

// TestLabelUntrustedMedia pins the untrusted framing for media content: a
// label block is inserted before media, the message is unchanged without
// media, and the operation is idempotent.
func TestLabelUntrustedMedia(t *testing.T) {
	img, err := NewImageBlock("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := NewDocumentBlock("application/pdf", []byte("%PDF-1.4"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("labels media with untrusted framing", func(t *testing.T) {
		blocks := []Block{{Type: BlockText, Text: "what is this?"}, img, doc}
		labelled := LabelUntrustedMedia(blocks)
		if len(labelled) != 4 {
			t.Fatalf("labelled blocks = %d, want 4", len(labelled))
		}
		if labelled[0].Type != BlockText || labelled[0].Text != "what is this?" {
			t.Fatalf("blocks[0] = %+v, want original text first", labelled[0])
		}
		if labelled[1].Type != BlockText || labelled[1].Text != UntrustedMediaLabel {
			t.Fatalf("blocks[1] = %+v, want untrusted label before first media block", labelled[1])
		}
		if labelled[2].Type != BlockImage || labelled[3].Type != BlockDocument {
			t.Fatalf("media order mangled: %+v", labelled)
		}
	})

	t.Run("leaves text-only messages unchanged", func(t *testing.T) {
		blocks := []Block{{Type: BlockText, Text: "hi"}}
		if got := LabelUntrustedMedia(blocks); len(got) != 1 || got[0].Text != "hi" {
			t.Fatalf("text-only message changed: %+v", got)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		once := LabelUntrustedMedia([]Block{{Type: BlockText, Text: "q"}, img})
		twice := LabelUntrustedMedia(once)
		if len(twice) != len(once) {
			t.Fatalf("double label added blocks: %+v", twice)
		}
	})

	t.Run("does not duplicate a label already present", func(t *testing.T) {
		blocks := []Block{{Type: BlockText, Text: UntrustedMediaLabel}, img}
		got := LabelUntrustedMedia(blocks)
		if len(got) != 2 {
			t.Fatalf("label duplicated: %+v", got)
		}
	})
}

// TestHasMedia asserts message-level media detection.
func TestHasMedia(t *testing.T) {
	img, err := NewImageBlock("image/png", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !(Message{Blocks: []Block{img}}).HasMedia() {
		t.Fatal("image message not detected")
	}
	if (Message{Blocks: []Block{{Type: BlockText, Text: "x"}}}).HasMedia() {
		t.Fatal("text message detected as media")
	}
}

// TestToolResultBlocksBackwardCompatible asserts a tool result that carries
// blocks still unmarshals for consumers that only read Content (the
// historical field) and vice versa.
func TestToolResultBlocksBackwardCompatible(t *testing.T) {
	img, err := NewImageBlock("image/png", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	tr := ToolResult{ToolUseID: "t1", Blocks: []Block{img}}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"blocks"`) {
		t.Fatalf("blocks not marshaled: %s", raw)
	}
	var back ToolResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Blocks) != 1 || back.Blocks[0].Source == nil {
		t.Fatalf("blocks lost: %+v", back)
	}
	// Old consumers see the empty Content string, not a panic.
	if back.Content != "" {
		t.Fatalf("content = %q", back.Content)
	}
}
