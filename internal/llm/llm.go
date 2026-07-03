// Package llm defines waffle's canonical LLM types (docs/plan.md, "Provider
// layer"). The agent loop speaks only these; providers are translators. The
// shape follows the Anthropic Messages API (the richest of the dialects) so
// translation loses nothing in the primary direction.
package llm

import (
	"context"
	"encoding/json"
)

// Role is a conversation role. System prompts are carried on Request, not as
// a message role.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType discriminates Block.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	// BlockThinking and BlockRedactedThinking carry model reasoning that
	// must be replayed to the provider unchanged on later turns. Providers
	// that don't understand them drop them.
	BlockThinking         BlockType = "thinking"
	BlockRedactedThinking BlockType = "redacted_thinking"
)

// Block is one content block within a message. The JSON encoding is
// waffle's storage format for persisted turns.
type Block struct {
	Type BlockType `json:"type"`

	Text       string      `json:"text,omitempty"`        // BlockText, and the visible text of BlockThinking
	Signature  string      `json:"signature,omitempty"`   // BlockThinking replay token
	Data       string      `json:"data,omitempty"`        // BlockRedactedThinking payload
	ToolUse    *ToolUse    `json:"tool_use,omitempty"`    // BlockToolUse
	ToolResult *ToolResult `json:"tool_result,omitempty"` // BlockToolResult
}

// ToolUse is the model asking for a tool invocation.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the outcome of a tool invocation, sent back as user content.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one turn of conversation.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// UserText builds a plain-text user message.
func UserText(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// Text concatenates the message's text blocks.
func (m Message) Text() string {
	var s string
	for _, b := range m.Blocks {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// Tool describes a callable tool. InputSchema is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Request is one model call.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// StopReason says why the model stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage is token accounting for one call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is the model's complete answer to a Request.
type Response struct {
	Message    Message // Role is always RoleAssistant
	StopReason StopReason
	Usage      Usage
}

// ToolUses returns the response's tool_use blocks.
func (r *Response) ToolUses() []ToolUse {
	var uses []ToolUse
	for _, b := range r.Message.Blocks {
		if b.Type == BlockToolUse && b.ToolUse != nil {
			uses = append(uses, *b.ToolUse)
		}
	}
	return uses
}

// EventType discriminates streaming events.
type EventType string

const (
	// EventTextDelta carries an incremental chunk of assistant text.
	EventTextDelta EventType = "text_delta"
)

// Event is a streaming callback payload.
type Event struct {
	Type EventType
	Text string
}

// StreamFunc receives streaming events; it may be nil.
type StreamFunc func(Event)

// Provider is a model backend. Complete sends req, streams incremental
// events to onEvent (if non-nil), and returns the final response.
type Provider interface {
	Complete(ctx context.Context, req Request, onEvent StreamFunc) (*Response, error)
}
