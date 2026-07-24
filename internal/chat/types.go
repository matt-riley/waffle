// Package chat defines the presentation-neutral contract shared by Waffle's
// direct, socket, plain, and interactive chat implementations.
package chat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
)

// OpenOptions selects the state a backend opens for a chat client.
type OpenOptions struct {
	Continue     bool     `json:"continue"`
	SessionID    string   `json:"session_id"`
	Profile      string   `json:"profile"`
	Capabilities []string `json:"capabilities"`
}

// Model describes a configured model without exposing provider credentials.
type Model struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Upstream string `json:"upstream"`
	Current  bool   `json:"current"`
}

// Session is the client-visible summary of a stored chat session.
type Session struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	ModelAlias string    `json:"model_alias"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// UsageRow is one persisted usage-accounting row.
type UsageRow struct {
	SessionID      string `json:"session_id"`
	Period         string `json:"period"`
	PeriodStart    string `json:"period_start"`
	Requests       int    `json:"requests"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	ReservedTokens int    `json:"reserved_tokens"`
}

// PermissionView describes effective sandbox and tool policy without any
// configuration body or secret-bearing state.
type PermissionView struct {
	SandboxMode  string   `json:"sandbox_mode"`
	Allow        []string `json:"allow"`
	Deny         []string `json:"deny"`
	DenyPrefixes []string `json:"deny_prefixes"`
}

// WorkItem is one client-visible item in a session's working set.
type WorkItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// SkillRef describes a skill's availability and attachment state without
// exposing its filesystem path or instruction body.
type SkillRef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Attached    bool   `json:"attached"`
	Missing     bool   `json:"missing"`
}

// State is the complete presentation-neutral chat state.
type State struct {
	SessionID      string        `json:"session_id"`
	Title          string        `json:"title"`
	ModelAlias     string        `json:"model_alias"`
	ModelError     string        `json:"model_error"`
	ProviderLabel  string        `json:"provider_label"`
	Profile        string        `json:"profile"`
	ConnectionMode string        `json:"connection_mode"`
	SandboxMode    string        `json:"sandbox_mode"`
	Workspace      string        `json:"workspace"`
	History        []llm.Message `json:"history"`
	Models         []Model       `json:"models"`
	Skills         []SkillRef    `json:"skills"`
	Capabilities   []string      `json:"capabilities"`
}

// EventKind identifies a streamed chat lifecycle event.
type EventKind string

const (
	// EventTextDelta carries incremental assistant text.
	EventTextDelta EventKind = "text_delta"
	// EventToolStarted announces a tool invocation.
	EventToolStarted EventKind = "tool_started"
	// EventToolFinished announces a completed tool invocation.
	EventToolFinished EventKind = "tool_finished"
	// EventNotice carries a client-visible informational or error notice.
	EventNotice EventKind = "notice"
	// EventState carries a replacement state snapshot.
	EventState EventKind = "state"
	// EventTurnDone marks completion of the current model turn.
	EventTurnDone EventKind = "turn_done"
)

// Event is one presentation-neutral streamed backend update.
type Event struct {
	Kind      EventKind `json:"kind"`
	Text      string    `json:"text"`
	ToolName  string    `json:"tool_name"`
	IsError   bool      `json:"is_error"`
	ByteCount int       `json:"byte_count"`
	Usage     llm.Usage `json:"usage"`
	State     *State    `json:"state"`
}

type eventJSON struct {
	Kind      EventKind `json:"kind"`
	Text      string    `json:"text"`
	ToolName  string    `json:"tool_name"`
	IsError   bool      `json:"is_error"`
	ByteCount int       `json:"byte_count"`
	Usage     usageJSON `json:"usage"`
	State     *State    `json:"state"`
}

type usageJSON struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// MarshalJSON keeps the consumed llm.Usage value while giving its nested
// token counts stable snake-case wire names.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(eventJSON{
		Kind:      e.Kind,
		Text:      e.Text,
		ToolName:  e.ToolName,
		IsError:   e.IsError,
		ByteCount: e.ByteCount,
		Usage: usageJSON{
			InputTokens:  e.Usage.InputTokens,
			OutputTokens: e.Usage.OutputTokens,
		},
		State: e.State,
	})
}

// UnmarshalJSON restores an Event from its presentation-neutral wire shape.
func (e *Event) UnmarshalJSON(data []byte) error {
	var wire eventJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = Event{
		Kind:      wire.Kind,
		Text:      wire.Text,
		ToolName:  wire.ToolName,
		IsError:   wire.IsError,
		ByteCount: wire.ByteCount,
		Usage: llm.Usage{
			InputTokens:  wire.Usage.InputTokens,
			OutputTokens: wire.Usage.OutputTokens,
		},
		State: wire.State,
	}
	return nil
}

// Result is the typed response to a local chat command.
type Result struct {
	Title       string          `json:"title"`
	Text        string          `json:"text"`
	Commands    []Command       `json:"commands"`
	Models      []Model         `json:"models"`
	Sessions    []Session       `json:"sessions"`
	Usage       []UsageRow      `json:"usage"`
	Permissions *PermissionView `json:"permissions"`
	Workset     []WorkItem      `json:"workset"`
	State       *State          `json:"state"`
	Confirm     bool            `json:"confirm"`
	ShouldClose bool            `json:"should_close"`
}

// Backend is the shared lifecycle implemented by direct and managed chat
// connections. Cancel is a synchronous best-effort interruption. Close owns
// final cancellation and active-work drain, must return when its context ends,
// and must not leave an untracked finalizer running after it returns.
type Backend interface {
	Open(context.Context, OpenOptions) (State, error)
	Turn(context.Context, string, func(Event)) error
	Command(context.Context, ParsedCommand, func(Event)) (Result, error)
	Cancel()
	Close(context.Context) error
}
