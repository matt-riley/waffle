package chat

import (
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
)

// ExportMessage is one visible transcript turn in a presentation-neutral
// export. Only text blocks are exported; tool arguments, results, system
// prompts, and hidden payloads never cross this boundary (#476).
type ExportMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ExportMessages reduces a history to its visible text turns.
func ExportMessages(history []llm.Message) []ExportMessage {
	out := make([]ExportMessage, 0, len(history))
	for _, message := range history {
		role := "assistant"
		if message.Role == llm.RoleUser {
			role = "user"
		}
		text := ""
		for _, block := range message.Blocks {
			if block.Type == llm.BlockText {
				text += block.Text
			}
		}
		if text == "" {
			continue
		}
		out = append(out, ExportMessage{Role: role, Text: text})
	}
	return out
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
