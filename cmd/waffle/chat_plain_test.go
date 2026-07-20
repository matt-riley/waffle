package main

import (
	"context"
	"errors"
	"fmt"
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
	commandFunc   func(chatpkg.ParsedCommand, func(chatpkg.Event)) (chatpkg.Result, error)
	openErr       error
	turnErr       error
	closeErr      error
	closeCtxErr   error
	closeDeadline time.Time
	closeBounded  bool
	closeCalls    int
}

type plainErrorReader struct{ err error }

func (r plainErrorReader) Read([]byte) (int, error) { return 0, r.err }

func (b *plainBackend) Open(_ context.Context, options chatpkg.OpenOptions) (chatpkg.State, error) {
	b.openOptions = options
	return b.state, b.openErr
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
	if b.commandFunc != nil {
		return b.commandFunc(command, emit)
	}
	for _, event := range b.commandEvents {
		emit(event)
	}
	return b.results[command.Name], nil
}

func (*plainBackend) Cancel() {}

func (b *plainBackend) Close(ctx context.Context) error {
	b.closeCalls++
	b.closeCtxErr = ctx.Err()
	b.closeDeadline, b.closeBounded = ctx.Deadline()
	return b.closeErr
}

func TestPlainChatMakesNewConfirmationActionable(t *testing.T) {
	backend := &plainBackend{state: chatpkg.State{SessionID: "01CONFIRM"}}
	backend.commandFunc = func(command chatpkg.ParsedCommand, _ func(chatpkg.Event)) (chatpkg.Result, error) {
		switch {
		case command.Name == chatpkg.CommandNew && command.Args == "":
			return chatpkg.Result{Text: "Start a new session?", Confirm: true}, nil
		case command.Name == chatpkg.CommandNew && command.Args == "confirm":
			return chatpkg.Result{Text: "new session 02"}, nil
		case command.Name == chatpkg.CommandExit:
			return chatpkg.Result{ShouldClose: true}, nil
		default:
			return chatpkg.Result{}, fmt.Errorf("unexpected command: %+v", command)
		}
	}

	var stdout, stderr strings.Builder
	input := "/new\nyes\n/new confirm\n/exit\n"
	if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wantStdout := "waffle chat —  via  — session 01CONFIRM. /help for commands.\n" +
		"\nyou> Start a new session?\n" +
		"confirm with /new confirm\n" +
		"\nyou> \nyou> new session 02\n" +
		"\nyou> "
	if stdout.String() != wantStdout {
		t.Fatalf("stdout:\n%q\nwant:\n%q", stdout.String(), wantStdout)
	}
	if got, want := stderr.String(), "waffle: confirmation requires /new confirm\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if len(backend.turns) != 0 {
		t.Fatalf("bare yes became model turns: %q", backend.turns)
	}
	wantCommands := []chatpkg.ParsedCommand{
		{Name: chatpkg.CommandNew},
		{Name: chatpkg.CommandNew, Args: "confirm"},
		{Name: chatpkg.CommandExit},
	}
	if fmt.Sprint(backend.commands) != fmt.Sprint(wantCommands) {
		t.Fatalf("commands = %+v, want %+v", backend.commands, wantCommands)
	}
}

func TestPlainChatRendersCompleteResultInExactStableOrder(t *testing.T) {
	updated := time.Date(2026, time.July, 20, 12, 34, 56, 0, time.UTC)
	allFields := chatpkg.Result{
		Title: "All fields", Text: "body", Confirm: true,
		Commands:    []chatpkg.Command{{Name: chatpkg.CommandHelp, Usage: "/help", Description: "show commands"}},
		Models:      []chatpkg.Model{{Alias: "alpha", Provider: "local", Upstream: "a", Current: true}, {Alias: "beta", Provider: "cloud", Upstream: "b"}},
		Sessions:    []chatpkg.Session{{ID: "01A", Title: "First", Summary: "summary", ModelAlias: "alpha", UpdatedAt: updated}},
		Usage:       []chatpkg.UsageRow{{SessionID: "01A", Period: "day", PeriodStart: "2026-07-20", Requests: 2, InputTokens: 3, OutputTokens: 4, ReservedTokens: 5}},
		Permissions: &chatpkg.PermissionView{SandboxMode: "docker", Allow: []string{"read", "write"}, Deny: []string{"shell"}, DenyPrefixes: []string{"secret_"}},
		Workset:     []chatpkg.WorkItem{{ID: "W1", Text: "first item"}, {ID: "W2", Text: "second item"}},
		State: &chatpkg.State{SessionID: "01SESSION", ModelAlias: "alpha", ProviderLabel: "local", Profile: "main",
			ConnectionMode: "unix", SandboxMode: "docker", Workspace: "owner/repo"},
	}
	backend := &plainBackend{state: chatpkg.State{SessionID: "01RESULT"}}
	backend.commandFunc = func(command chatpkg.ParsedCommand, _ func(chatpkg.Event)) (chatpkg.Result, error) {
		if command.Name == chatpkg.CommandExit {
			return chatpkg.Result{ShouldClose: true}, nil
		}
		return allFields, nil
	}
	var stdout, stderr strings.Builder
	if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader("/resume 01A\n/exit\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	want := "waffle chat —  via  — session 01RESULT. /help for commands.\n\n" +
		"you> All fields\n" +
		"body\n" +
		fmt.Sprintf("%-58s %s\n", "/help", "show commands") +
		"* alpha via local (a)\n" +
		"  beta via cloud (b)\n" +
		"01A  First  model=alpha  summary=summary  updated=2026-07-20T12:34:56Z\n" +
		"session=01A period=day start=2026-07-20 requests=2 input=3 output=4 reserved=5\n" +
		"sandbox=docker allow=read,write deny=shell deny-prefixes=secret_\n" +
		"W1  first item\n" +
		"W2  second item\n" +
		"session=01SESSION model=alpha provider=local profile=main connection=unix sandbox=docker workspace=owner/repo\n" +
		"retry /resume 01A when idle\n" +
		"\nyou> "
	if stdout.String() != want {
		t.Fatalf("stdout:\n%q\nwant:\n%q", stdout.String(), want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPlainChatClosesOnceWhenOpenFailsAndPreservesOpenError(t *testing.T) {
	openErr := errors.New("open failed")
	backend := &plainBackend{openErr: openErr, closeErr: errors.New("close failed")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr strings.Builder
	err := runPlainChat(ctx, backend, chatpkg.OpenOptions{}, strings.NewReader(""), &strings.Builder{}, &stderr)
	if !errors.Is(err, openErr) {
		t.Fatalf("runPlainChat error = %v, want open error", err)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
	}
	assertBoundedCloseContext(t, backend)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no close warning after failed Open", stderr.String())
	}
}

func TestPlainChatReportsEOFCloseErrorOnceAndReturnsSuccess(t *testing.T) {
	backend := &plainBackend{
		state:    chatpkg.State{SessionID: "01EOF"},
		closeErr: errors.New("summary\x1b[31m\r\nfailed"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr strings.Builder
	if err := runPlainChat(ctx, backend, chatpkg.OpenOptions{}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("runPlainChat = %v, want graceful EOF success", err)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
	}
	assertBoundedCloseContext(t, backend)
	if got, want := stderr.String(), "waffle: warning: summary failed\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestPlainChatExitWarningIsSanitizedAndRenderedExactlyOnce(t *testing.T) {
	warning := "warning: summary\x1b[31m\r\nfailed"
	backend := &plainBackend{
		state:    chatpkg.State{SessionID: "01EXIT"},
		closeErr: errors.New("summary failed"),
	}
	backend.commandFunc = func(command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
		if command.Name != chatpkg.CommandExit {
			return chatpkg.Result{}, fmt.Errorf("unexpected command %s", command.Name)
		}
		emit(chatpkg.Event{Kind: chatpkg.EventNotice, Text: warning, IsError: true})
		return chatpkg.Result{Text: warning, ShouldClose: true}, nil
	}
	var stdout, stderr strings.Builder
	if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader("/exit\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got, want := stderr.String(), "waffle: warning: summary failed\n"; got != want {
		t.Fatalf("stderr = %q, want one warning %q", got, want)
	}
	if strings.Contains(stdout.String(), "warning") || strings.Contains(stdout.String(), "summary failed") {
		t.Fatalf("warning leaked to stdout: %q", stdout.String())
	}
	if backend.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
	}
}

func TestPlainChatDeduplicatesMixedResultWarningButKeepsSuccessText(t *testing.T) {
	backend := &plainBackend{
		state:    chatpkg.State{SessionID: "01MIXED"},
		closeErr: errors.New("summary failed"),
	}
	backend.commandFunc = func(command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
		switch command.Name {
		case chatpkg.CommandNew:
			emit(chatpkg.Event{Kind: chatpkg.EventNotice, Text: "warning: summary failed", IsError: true})
			return chatpkg.Result{Text: "warning: summary failed\nnew session 02"}, nil
		case chatpkg.CommandExit:
			return chatpkg.Result{ShouldClose: true}, nil
		default:
			return chatpkg.Result{}, fmt.Errorf("unexpected command %s", command.Name)
		}
	}
	var stdout, stderr strings.Builder
	if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader("/new confirm\n/exit\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got, want := stderr.String(), "waffle: warning: summary failed\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), "warning") || !strings.Contains(stdout.String(), "new session 02\n") {
		t.Fatalf("mixed result stdout = %q", stdout.String())
	}
}

func TestPlainChatPreservesPrimaryScanAndTurnErrorsOverCloseError(t *testing.T) {
	t.Run("scan error is returned", func(t *testing.T) {
		scanErr := errors.New("scan failed")
		backend := &plainBackend{
			state:    chatpkg.State{SessionID: "01SCAN"},
			closeErr: errors.New("close failed"),
		}
		var stderr strings.Builder
		err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, plainErrorReader{err: scanErr}, &strings.Builder{}, &stderr)
		if !errors.Is(err, scanErr) {
			t.Fatalf("runPlainChat error = %v, want scan error", err)
		}
		if got, want := stderr.String(), "waffle: warning: close failed\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
		if backend.closeCalls != 1 {
			t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
		}
	})

	t.Run("turn error remains before close warning", func(t *testing.T) {
		backend := &plainBackend{
			state:    chatpkg.State{SessionID: "01TURN"},
			turnErr:  errors.New("turn failed"),
			closeErr: errors.New("close failed"),
		}
		var stderr strings.Builder
		if err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader("hello\n"), &strings.Builder{}, &stderr); err != nil {
			t.Fatal(err)
		}
		if got, want := stderr.String(), "\nwaffle: turn failed\nwaffle: warning: close failed\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})
}

func assertBoundedCloseContext(t *testing.T, backend *plainBackend) {
	t.Helper()
	if backend.closeCtxErr != nil {
		t.Fatalf("Close context inherited cancellation: %v", backend.closeCtxErr)
	}
	if !backend.closeBounded {
		t.Fatal("Close context has no deadline")
	}
	remaining := time.Until(backend.closeDeadline)
	if remaining <= 0 || remaining > 11*time.Second {
		t.Fatalf("Close context deadline remaining = %v", remaining)
	}
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

func TestPlainChatScannerHasInclusiveOneMiBBound(t *testing.T) {
	content := strings.Repeat("x", 1<<20)
	tests := []struct {
		name      string
		input     string
		wantError bool
	}{
		{name: "exact limit at EOF", input: content},
		{name: "exact limit newline terminated", input: content + "\n"},
		{name: "one over limit at EOF", input: content + "x", wantError: true},
		{name: "one over limit newline terminated", input: content + "x\n", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &plainBackend{state: chatpkg.State{SessionID: "01BOUND"}, results: make(map[chatpkg.Name]chatpkg.Result)}
			err := runPlainChat(context.Background(), backend, chatpkg.OpenOptions{}, strings.NewReader(tt.input), &strings.Builder{}, &strings.Builder{})
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "token too long") {
					t.Fatalf("oversized input error = %v, want scanner bound failure", err)
				}
				if len(backend.turns) != 0 {
					t.Fatalf("oversized input reached Turn: %d calls", len(backend.turns))
				}
			} else {
				if err != nil {
					t.Fatalf("exact-limit input error = %v", err)
				}
				if len(backend.turns) != 1 || len(backend.turns[0]) != 1<<20 {
					t.Fatalf("exact-limit turns=%d length=%d", len(backend.turns), firstTurnLength(backend.turns))
				}
			}
			if backend.closeCalls != 1 {
				t.Fatalf("Close calls = %d, want 1", backend.closeCalls)
			}
		})
	}
}

func firstTurnLength(turns []string) int {
	if len(turns) == 0 {
		return 0
	}
	return len(turns[0])
}
