package chat

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
)

func TestCommandRegistryParsesAliasesAndNearMisses(t *testing.T) {
	tests := []struct {
		input string
		name  Name
		args  string
		ok    bool
	}{
		{"/help", CommandHelp, "", true},
		{"/exit", CommandExit, "", true},
		{"/quit", CommandExit, "", true},
		{"/model claude", CommandModel, "claude", true},
		{"/models", CommandModels, "", true},
		{"/new", CommandNew, "", true},
		{"/reset", CommandNew, "", true},
		{"/clear", CommandNew, "", true},
		{"/sessions", CommandSessions, "", true},
		{"/resume 01ABC", CommandResume, "01ABC", true},
		{"/status", CommandStatus, "", true},
		{"/usage", CommandUsage, "", true},
		{"/permissions", CommandPermissions, "", true},
		{"/skill audit fast", CommandSkill, "audit fast", true},
		{"/skills", CommandSkills, "", true},
		{"/skills attach reviewer", CommandSkills, "attach reviewer", true},
		{"/skills detach reviewer", CommandSkills, "detach reviewer", true},
		{"/repo owner/repo", CommandRepo, "owner/repo", true},
		{"/workset list", CommandWorkset, "list", true},
		{"/modelsx", "", "", false},
		{"/skillful audit", "", "", false},
		{"/skillsx", "", "", false},
		{"/repos owner/repo", "", "", false},
		{"plain /model text", "", "", false},
	}
	for _, tt := range tests {
		got, ok, err := ParseInput(tt.input)
		if err != nil {
			t.Fatalf("ParseInput(%q): %v", tt.input, err)
		}
		if ok != tt.ok || got.Name != tt.name || got.Args != tt.args {
			t.Errorf("ParseInput(%q) = %+v,%v", tt.input, got, ok)
		}
	}
}

func TestParseInputAcceptsFirstTokenWhitespace(t *testing.T) {
	got, ok, err := ParseInput("/model\t claude 3 ")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != (ParsedCommand{Name: CommandModel, Args: "claude 3"}) {
		t.Fatalf("ParseInput = %+v,%v", got, ok)
	}
}

func TestParseInputReportsExactUsage(t *testing.T) {
	for _, input := range []string{"/skill", "/repo   "} {
		got, ok, err := ParseInput(input)
		if !ok {
			t.Fatalf("ParseInput(%q) ok = false", input)
		}
		if got.Name == "" {
			t.Fatalf("ParseInput(%q) command = %+v", input, got)
		}
		command := commandByName(t, got.Name)
		if err == nil || err.Error() != "usage: "+command.Usage {
			t.Fatalf("ParseInput(%q) error = %v, want usage: %s", input, err, command.Usage)
		}
	}
}

func TestSkillsCommandReportsExactUsageForMalformedForms(t *testing.T) {
	for _, input := range []string{
		"/skills attach",
		"/skills detach",
		"/skills reviewer",
		"/skills attach reviewer extra",
		"/skills detach reviewer extra",
	} {
		got, ok, err := ParseInput(input)
		if !ok || got.Name != CommandSkills {
			t.Fatalf("ParseInput(%q) = %+v,%v", input, got, ok)
		}
		if err == nil || err.Error() != "usage: /skills [attach <name>|detach <name>]" {
			t.Fatalf("ParseInput(%q) error = %v", input, err)
		}
	}
}

func TestCompletionIsStableAndDocumented(t *testing.T) {
	got := Complete("/mo")
	if len(got) != 2 || got[0].Name != CommandModel || got[1].Name != CommandModels {
		t.Fatalf("Complete = %+v", got)
	}
	for _, command := range Commands() {
		if command.Usage == "" || command.Description == "" {
			t.Fatalf("undocumented: %+v", command)
		}
	}
	skills := Complete("/ski")
	if got := commandNames(skills); !reflect.DeepEqual(got, []Name{CommandSkill, CommandSkills}) {
		t.Fatalf("Complete(/ski) = %v", got)
	}
}

func TestCommandsAreCanonicalAndImmutable(t *testing.T) {
	want := []Name{
		CommandHelp, CommandExit, CommandModel, CommandModels, CommandNew,
		CommandSessions, CommandResume, CommandStatus, CommandUsage,
		CommandPermissions, CommandSkill, CommandSkills, CommandRepo, CommandWorkset,
		CommandRename, CommandPin, CommandUnpin, CommandDelete, CommandBranch,
	}
	first := Commands()
	if got := commandNames(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("Commands names = %v, want %v", got, want)
	}
	first[0].Description = "changed"
	first[1].Aliases[0] = "changed"
	second := Commands()
	if second[0].Description == "changed" || second[1].Aliases[0] != "quit" {
		t.Fatalf("Commands returned mutable registry: %+v", second[:2])
	}

	completed := Complete("/e")
	completed[0].Description = "changed"
	if Complete("/e")[0].Description == "changed" {
		t.Fatal("Complete returned mutable registry entry")
	}
	if got := Complete("/qu"); len(got) != 0 {
		t.Fatalf("Complete returned aliases, want canonical commands only: %+v", got)
	}
}

func TestSharedDTOsUseStableJSONFieldNames(t *testing.T) {
	stamp := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	values := []struct {
		value any
		keys  []string
	}{
		{OpenOptions{Continue: true, SessionID: "s", Profile: "p", Capabilities: []string{"c"}, Temporary: true}, []string{"continue", "session_id", "profile", "capabilities", "temporary"}},
		{Model{Alias: "a", Provider: "p", Upstream: "u", Current: true}, []string{"alias", "provider", "upstream", "current"}},
		{Session{ID: "s", Title: "t", Summary: "x", ModelAlias: "a", UpdatedAt: stamp, Pinned: true}, []string{"id", "title", "summary", "model_alias", "updated_at", "pinned"}},
		{UsageRow{SessionID: "s", Period: "day", PeriodStart: "today", Requests: 1, InputTokens: 2, OutputTokens: 3, ReservedTokens: 4, CacheCreationInputTokens: 5, CacheReadInputTokens: 6, TunnelBytes: 7}, []string{"session_id", "period", "period_start", "requests", "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "reserved_tokens", "tunnel_bytes"}},
		{PermissionView{SandboxMode: "read-only", Allow: []string{"read"}, Deny: []string{"bash"}, DenyPrefixes: []string{"secret"}}, []string{"sandbox_mode", "allow", "deny", "deny_prefixes"}},
		{WorkItem{ID: "w", Text: "work"}, []string{"id", "text"}},
		{SkillRef{Name: "reviewer", Description: "review changes", Attached: true, Missing: false}, []string{"name", "description", "attached", "missing"}},
		{State{SessionID: "s", Title: "t", ModelAlias: "a", ModelError: "missing", ProviderLabel: "p", Profile: "default", ConnectionMode: "direct", SandboxMode: "read-only", Workspace: "w", History: []llm.Message{{}}, Models: []Model{{Alias: "a"}}, Skills: []SkillRef{{Name: "reviewer"}}, Capabilities: []string{"c"}}, []string{"session_id", "title", "model_alias", "model_error", "provider_label", "profile", "connection_mode", "sandbox_mode", "workspace", "history", "models", "skills", "capabilities", "lineage"}},
		{Event{Kind: EventNotice, Text: "text", ToolName: "tool", ToolCallID: "call-1", IsError: true, ByteCount: 2, DurationMS: 3, Usage: llm.Usage{InputTokens: 1}, State: &State{SessionID: "s"}}, []string{"kind", "text", "tool_name", "tool_call_id", "is_error", "byte_count", "duration_ms", "usage", "state"}},
		{Event{Kind: EventNotice, Text: "text", ToolName: "tool", ToolCallID: "call-1", IsError: true, ByteCount: 2, DurationMS: 3, Usage: llm.Usage{InputTokens: 1}, State: &State{SessionID: "s"}, Artifacts: []Artifact{{ID: "art-1", Name: "a.md"}}, Sources: []Source{{ID: "s1", Label: "l", Kind: "web"}}}, []string{"kind", "text", "tool_name", "tool_call_id", "is_error", "byte_count", "duration_ms", "usage", "state", "artifacts", "sources"}},
		{Event{Kind: EventNotice, Text: "text", ToolName: "tool", ToolCallID: "call-1", IsError: true, ByteCount: 2, DurationMS: 3, Usage: llm.Usage{InputTokens: 1}, State: &State{SessionID: "s"}}, []string{"kind", "text", "tool_name", "tool_call_id", "is_error", "byte_count", "duration_ms", "usage", "state"}},
		{Event{Kind: EventNotice, Text: "text", ToolName: "tool", ToolCallID: "call-1", IsError: true, ByteCount: 2, DurationMS: 3, Usage: llm.Usage{InputTokens: 1}, State: &State{SessionID: "s"}, Sources: []Source{{ID: "s1", Label: "l", Kind: "web"}}}, []string{"kind", "text", "tool_name", "tool_call_id", "is_error", "byte_count", "duration_ms", "usage", "state", "sources"}},

		{Result{Title: "t", Text: "x", Commands: []Command{{Name: CommandHelp}}, Models: []Model{{Alias: "a"}}, Sessions: []Session{{ID: "s"}}, Usage: []UsageRow{{Requests: 1}}, Permissions: &PermissionView{SandboxMode: "read-only"}, Workset: []WorkItem{{ID: "w"}}, State: &State{SessionID: "s"}, Confirm: true, ShouldClose: true}, []string{"title", "text", "commands", "models", "sessions", "usage", "permissions", "workset", "state", "confirm", "should_close"}},
		{Command{Name: CommandExit, Usage: "/exit", Aliases: []string{"quit"}, Description: "close"}, []string{"name", "usage", "aliases", "description"}},
		{ParsedCommand{Name: CommandModel, Args: "a"}, []string{"name", "args"}},
	}

	for _, tt := range values {
		data, err := json.Marshal(tt.value)
		if err != nil {
			t.Fatalf("Marshal(%T): %v", tt.value, err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatalf("Unmarshal(%T): %v", tt.value, err)
		}
		for _, key := range tt.keys {
			if _, ok := object[key]; !ok {
				t.Errorf("Marshal(%T) = %s, missing key %q", tt.value, data, key)
			}
		}
		if len(object) != len(tt.keys) {
			t.Errorf("Marshal(%T) keys = %v, want exactly %v", tt.value, reflect.ValueOf(object).MapKeys(), tt.keys)
		}
	}
}

func TestEventUsageUsesSnakeCaseAndRoundTrips(t *testing.T) {
	want := Event{
		Kind:       EventToolFinished,
		ToolCallID: "tool-7",
		DurationMS: 14,
		Usage:      llm.Usage{InputTokens: 12, OutputTokens: 34},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	var usage map[string]int
	if err := json.Unmarshal(object["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(usage, map[string]int{"input_tokens": 12, "output_tokens": 34}) {
		t.Fatalf("event usage JSON = %s, want snake-case token fields", object["usage"])
	}

	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestEventKindsAreStable(t *testing.T) {
	want := []EventKind{"text_delta", "tool_started", "tool_finished", "notice", "state", "turn_done"}
	got := []EventKind{EventTextDelta, EventToolStarted, EventToolFinished, EventNotice, EventState, EventTurnDone}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
}

type backendContract struct{}

func (backendContract) Open(context.Context, OpenOptions) (State, error) { return State{}, nil }
func (backendContract) Turn(context.Context, string, func(Event)) error  { return nil }
func (backendContract) Command(context.Context, ParsedCommand, func(Event)) (Result, error) {
	return Result{}, nil
}
func (backendContract) Cancel()                     {}
func (backendContract) Close(context.Context) error { return nil }

var _ Backend = backendContract{}

func commandByName(t *testing.T, name Name) Command {
	t.Helper()
	for _, command := range Commands() {
		if command.Name == name {
			return command
		}
	}
	t.Fatalf("command %q not found", name)
	return Command{}
}

func commandNames(commands []Command) []Name {
	names := make([]Name, len(commands))
	for i, command := range commands {
		names[i] = command.Name
	}
	return names
}

func TestUsageErrorsDoNotMatchNearMisses(t *testing.T) {
	for _, input := range []string{"/skillful", "/repos"} {
		_, ok, err := ParseInput(input)
		if ok || err != nil {
			t.Fatalf("ParseInput(%q) = ok %v, error %v", input, ok, err)
		}
	}
}

func TestAllCanonicalCommandValues(t *testing.T) {
	for _, command := range Commands() {
		if command.Name == "" || command.Usage != "/"+string(command.Name) && !strings.HasPrefix(command.Usage, "/"+string(command.Name)+" ") {
			t.Errorf("command name and usage disagree: %+v", command)
		}
	}
}
