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
	// Default and Utility record the Waffle-wide roles an alias holds.
	Default bool `json:"default,omitempty"`
	Utility bool `json:"utility,omitempty"`
	// Description is an optional operator-authored "use for" note (#484).
	Description string `json:"description,omitempty"`
}

// Session is the client-visible summary of a stored chat session.
type Session struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	ModelAlias string    `json:"model_alias"`
	UpdatedAt  time.Time `json:"updated_at"`
	Pinned     bool      `json:"pinned"`
}

// BranchLineage is the durable fork provenance of a conversation (#471): the
// source session it was branched from and the completed-exchange sequence the
// prefix was cut at. Zero/empty values mean the conversation was started
// fresh.
type BranchLineage struct {
	ForkedFrom  string `json:"forked_from,omitempty"`
	ForkedAtSeq int64  `json:"forked_at_seq,omitempty"`
}

// UsageRow is one persisted usage-accounting row.
type UsageRow struct {
	SessionID                string `json:"session_id"`
	Period                   string `json:"period"`
	PeriodStart              string `json:"period_start"`
	Requests                 int    `json:"requests"`
	InputTokens              int    `json:"input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	ReservedTokens           int    `json:"reserved_tokens"`
	// TunnelBytes is the tunnelled egress byte total for this row (#244).
	TunnelBytes int64 `json:"tunnel_bytes"`
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
	Lineage        BranchLineage `json:"lineage,omitempty"`
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
	// EventArtifact announces artifacts declared by the completed exchange
	// (#480).
	EventArtifact EventKind = "artifact"
	// EventSources carries the safe citation projection for the completed
	// exchange (#479).
	EventSources EventKind = "sources"
)

// Artifact is the client-visible, redacted projection of a declared session
// artifact (#480). ID is opaque (never a host path); Name and MediaType are
// safe display metadata; Size and Digest let the Desk verify before serving.
type Artifact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Digest    string `json:"digest,omitempty"`
	State     string `json:"state,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
}

// Source is the client-visible, redacted projection of a citation (#479).
// Labels and snippets are safe display text; URL is restricted to safe
// protocols; workspace resources are opaque IDs, never absolute paths.
type Source struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	URL        string `json:"url,omitempty"`
	Resource   string `json:"resource,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// Event is one presentation-neutral streamed backend update.
type Event struct {
	Kind       EventKind  `json:"kind"`
	Text       string     `json:"text"`
	ToolName   string     `json:"tool_name"`
	ToolCallID string     `json:"tool_call_id"`
	IsError    bool       `json:"is_error"`
	ByteCount  int        `json:"byte_count"`
	DurationMS int64      `json:"duration_ms"`
	Usage      llm.Usage  `json:"usage"`
	State      *State     `json:"state"`
	Artifacts  []Artifact `json:"artifacts,omitempty"`
	Sources    []Source   `json:"sources,omitempty"`
}

type eventJSON struct {
	Kind       EventKind  `json:"kind"`
	Text       string     `json:"text"`
	ToolName   string     `json:"tool_name"`
	ToolCallID string     `json:"tool_call_id"`
	IsError    bool       `json:"is_error"`
	ByteCount  int        `json:"byte_count"`
	DurationMS int64      `json:"duration_ms"`
	Usage      usageJSON  `json:"usage"`
	State      *State     `json:"state"`
	Artifacts  []Artifact `json:"artifacts,omitempty"`
	Sources    []Source   `json:"sources,omitempty"`
}

type usageJSON struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// MarshalJSON keeps the consumed llm.Usage value while giving its nested
// token counts stable snake-case wire names.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(eventJSON{
		Kind:       e.Kind,
		Text:       e.Text,
		ToolName:   e.ToolName,
		ToolCallID: e.ToolCallID,
		IsError:    e.IsError,
		ByteCount:  e.ByteCount,
		DurationMS: e.DurationMS,
		Usage: usageJSON{
			InputTokens:  e.Usage.InputTokens,
			OutputTokens: e.Usage.OutputTokens,
		},
		State:     e.State,
		Artifacts: e.Artifacts,
		Sources:   e.Sources,
	})
}

// UnmarshalJSON restores an Event from its presentation-neutral wire shape.
func (e *Event) UnmarshalJSON(data []byte) error {
	var wire eventJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = Event{
		Kind:       wire.Kind,
		Text:       wire.Text,
		ToolName:   wire.ToolName,
		ToolCallID: wire.ToolCallID,
		IsError:    wire.IsError,
		ByteCount:  wire.ByteCount,
		DurationMS: wire.DurationMS,
		Usage: llm.Usage{
			InputTokens:  wire.Usage.InputTokens,
			OutputTokens: wire.Usage.OutputTokens,
		},
		State:     wire.State,
		Artifacts: wire.Artifacts,
		Sources:   wire.Sources,
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
