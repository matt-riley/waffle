package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

type plainBackend struct {
	state         chatpkg.State
	openOptions   chatpkg.OpenOptions
	turns         []string
	commands      []chatpkg.ParsedCommand
	events        []chatpkg.Event
	commandEvents []chatpkg.Event
	results       map[chatpkg.Name]chatpkg.Result
	turnErr       error
	closeCalls    int
}

func (b *plainBackend) Open(_ context.Context, options chatpkg.OpenOptions) (chatpkg.State, error) {
	b.openOptions = options
	return b.state, nil
}

func (b *plainBackend) Turn(_ context.Context, input string, emit func(chatpkg.Event)) error {
	b.turns = append(b.turns, input)
	for _, event := range b.events {
		emit(event)
	}
	return b.turnErr
}

func (b *plainBackend) Command(_ context.Context, command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	b.commands = append(b.commands, command)
	for _, event := range b.commandEvents {
		emit(event)
	}
	return b.results[command.Name], nil
}

func (*plainBackend) Cancel() {}

func (b *plainBackend) Close(context.Context) error {
	b.closeCalls++
	return nil
}

func TestPlainChatOpensScansExactCommandsAndClosesOnShouldClose(t *testing.T) {
	updated := time.Date(2026, time.July, 20, 12, 34, 56, 0, time.UTC)
	backend := &plainBackend{
		state: chatpkg.State{
			SessionID: "01SESSION", ModelAlias: "writer", ProviderLabel: "local (openai)",
			ModelError: "select another model", History: []llm.Message{llm.UserText("earlier")},
		},
		events: []chatpkg.Event{
			{Kind: chatpkg.EventTextDelta, Text: "answer"},
			{Kind: chatpkg.EventToolStarted, ToolName: "read\x1b[31m\nsecrets"},
			{Kind: chatpkg.EventToolFinished, ToolName: "read\x1b[31m\nsecrets", ByteCount: 42},
			{Kind: chatpkg.EventNotice, Text: "notice\r\nwith\x1b[2m controls"},
			{Kind: chatpkg.EventTurnDone},
		},
		results: map[chatpkg.Name]chatpkg.Result{
			chatpkg.CommandHelp: {
				Title: "Chat commands",
				Commands: []chatpkg.Command{
					{Name: chatpkg.CommandHelp, Usage: "/help", Description: "show commands"},
					{Name: chatpkg.CommandExit, Usage: "/exit", Description: "close chat"},
				},
			},
			chatpkg.CommandModels: {Title: "Models", Models: []chatpkg.Model{
				{Alias: "alpha", Provider: "local", Upstream: "a", Current: true},
				{Alias: "beta", Provider: "cloud", Upstream: "b"},
			}},
			chatpkg.CommandSessions: {Title: "Sessions", Sessions: []chatpkg.Session{
				{ID: "01A", Title: "First", Summary: "summary", ModelAlias: "alpha", UpdatedAt: updated},
				{ID: "01B", Title: "Second", ModelAlias: "beta", UpdatedAt: updated.Add(time.Minute)},
			}},
			chatpkg.CommandUsage: {Title: "Usage", Usage: []chatpkg.UsageRow{
				{SessionID: "01A", Period: "day", PeriodStart: "2026-07-20", Requests: 2, InputTokens: 3, OutputTokens: 4, ReservedTokens: 5},
			}},
			chatpkg.CommandPermissions: {Title: "Permissions", Permissions: &chatpkg.PermissionView{
				SandboxMode: "docker", Allow: []string{"read", "write"}, Deny: []string{"shell"}, DenyPrefixes: []string{"secret_"},
			}},
			chatpkg.CommandWorkset: {Title: "Workset", Workset: []chatpkg.WorkItem{
				{ID: "W1", Text: "first item"}, {ID: "W2", Text: "second item"},
			}},
			chatpkg.CommandStatus: {Title: "Status", State: &chatpkg.State{
				SessionID: "01SESSION", ModelAlias: "alpha", ProviderLabel: "local", Profile: "main",
				ConnectionMode: "unix", SandboxMode: "docker", Workspace: "owner/repo",
			}},
			chatpkg.CommandExit: {ShouldClose: true},
		},
	}
	input := strings.Join([]string{
		"/help", "/models", "/sessions", "/usage", "/permissions", "/workset list", "/status",
		"/modelsx remains a model message", "/exit", "must not run",
	}, "\n") + "\n"
	var stdout, stderr strings.Builder
	open := chatpkg.OpenOptions{Continue: true, Profile: "research", Capabilities: []string{"plain"}}
	if err := runPlainChat(context.Background(), backend, open, strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("runPlainChat: %v", err)
	}
	if backend.openOptions.Continue != open.Continue || backend.openOptions.Profile != open.Profile || strings.Join(backend.openOptions.Capabilities, ",") != "plain" {
		t.Fatalf("Open options = %+v, want %+v", backend.openOptions, open)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
	}
	if got := strings.Join(backend.turns, "|"); got != "/modelsx remains a model message" {
		t.Fatalf("Turn inputs = %q", got)
	}
	wantCommands := []chatpkg.Name{chatpkg.CommandHelp, chatpkg.CommandModels, chatpkg.CommandSessions, chatpkg.CommandUsage, chatpkg.CommandPermissions, chatpkg.CommandWorkset, chatpkg.CommandStatus, chatpkg.CommandExit}
	if len(backend.commands) != len(wantCommands) {
		t.Fatalf("commands = %+v, want %v", backend.commands, wantCommands)
	}
	for i, want := range wantCommands {
		if backend.commands[i].Name != want {
			t.Fatalf("command %d = %+v, want %s", i, backend.commands[i], want)
		}
	}

	out := stdout.String()
	for _, want := range []string{
		"waffle chat — writer via local (openai) — session 01SESSION. /help for commands.",
		"(continuing with 1 earlier turns)", "Chat commands", "/help", "/exit", "Models",
		"* alpha via local (a)", "  beta via cloud (b)", "Sessions", "01A  First  model=alpha",
		"Usage", "session=01A period=day start=2026-07-20 requests=2 input=3 output=4 reserved=5",
		"sandbox=docker allow=read,write deny=shell deny-prefixes=secret_", "W1  first item", "W2  second item",
		"session=01SESSION model=alpha provider=local profile=main connection=unix sandbox=docker workspace=owner/repo",
		"answer", "[read secrets]", "[read secrets -> ok, 42 bytes]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "must not run") || strings.Contains(out, "\x1b") {
		t.Fatalf("stdout contains post-close input or ANSI control: %q", out)
	}
	errOut := stderr.String()
	for _, want := range []string{"waffle: select another model", "notice with controls"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q: %q", want, errOut)
		}
	}
	if strings.Contains(errOut, "\x1b") || strings.Contains(errOut, "\r") {
		t.Fatalf("stderr contains control characters: %q", errOut)
	}
}

func TestPlainChatReportsParseAndTurnErrorsAndClosesOnEOF(t *testing.T) {
	backend := &plainBackend{
		state:   chatpkg.State{SessionID: "01EOF", ModelAlias: "gpt", ProviderLabel: "test"},
		turnErr: errors.New("provider\x1b[31m\r\nfailed"),
		results: make(map[chatpkg.Name]chatpkg.Result),
	}
	var stdout, stderr strings.Builder
	if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader("/skill\nhello\n"), &stdout, &stderr); err != nil {
		t.Fatalf("runPlainChat: %v", err)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
	}
	for _, want := range []string{"usage: /skill <name> [args]", "provider failed"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q: %q", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "\x1b") || strings.Contains(stderr.String(), "\r") {
		t.Fatalf("stderr contains control characters: %q", stderr.String())
	}
}

func TestPlainChatWritesTextDeltasVerbatim(t *testing.T) {
	delta := "first\r\n\tsecond"
	backend := &plainBackend{
		state:   chatpkg.State{SessionID: "01TEXT"},
		events:  []chatpkg.Event{{Kind: chatpkg.EventTextDelta, Text: delta}},
		results: make(map[chatpkg.Name]chatpkg.Result),
	}
	var stdout strings.Builder
	if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader("hello\n"), &stdout, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), delta) {
		t.Fatalf("text delta was changed: got %q, want substring %q", stdout.String(), delta)
	}
}

func TestPlainChatScannerIsBoundedToOneMiB(t *testing.T) {
	backend := &plainBackend{state: chatpkg.State{SessionID: "01BOUND"}, results: make(map[chatpkg.Name]chatpkg.Result)}
	err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader(strings.Repeat("x", (1<<20)+1)), &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("oversized input error = %v, want scanner bound failure", err)
	}
	if len(backend.turns) != 0 || backend.closeCalls != 1 {
		t.Fatalf("oversized input turns=%d close=%d", len(backend.turns), backend.closeCalls)
	}
}
