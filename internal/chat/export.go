package chat

import (
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
)

// ExportMessage is one visible transcript turn in a presentation-neutral
// export. Text blocks are exported as written; image and document blocks
// become placeholders such as [image: image/png] so media-only turns are
// not omitted. Tool arguments, results, system prompts, and hidden
// payloads never cross this boundary (#476).
type ExportMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ExportMessages reduces a history to its visible text and media turns.
func ExportMessages(history []llm.Message) []ExportMessage {
	out := make([]ExportMessage, 0, len(history))
	for _, message := range history {
		role := "assistant"
		if message.Role == llm.RoleUser {
			role = "user"
		}
		text := exportVisibleText(message.Blocks)
		if text == "" {
			continue
		}
		out = append(out, ExportMessage{Role: role, Text: text})
	}
	return out
}

func exportVisibleText(blocks []llm.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case llm.BlockText:
			b.WriteString(block.Text)
		case llm.BlockImage, llm.BlockDocument:
			kind := "image"
			if block.Type == llm.BlockDocument {
				kind = "document"
			}
			mediaType := "unknown"
			if block.Source != nil && block.Source.MediaType != "" {
				mediaType = block.Source.MediaType
			}
			fmt.Fprintf(&b, "[%s: %s]", kind, mediaType)
		}
	}
	return b.String()
}

// ExportMarkdown renders the visible transcript as an owner-local Markdown
// file with only explicitly safe metadata.
func ExportMarkdown(title, profile string, history []llm.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", strings.TrimSpace(title))
	if profile != "" {
		fmt.Fprintf(&b, "Profile: %s\n\n", profile)
	}
	for _, message := range ExportMessages(history) {
		label := "Waffle"
		if message.Role == "user" {
			label = "You"
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", label, message.Text)
	}
	return b.String()
}
