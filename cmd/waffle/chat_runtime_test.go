package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestChatRuntimeModelSelectionPersistsAndResumeRestoresIt(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ModelAlias != "gpt" {
		t.Fatalf("saved = %+v", saved)
	}

	second := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	resumed, err := second.Open(ctx, chatpkg.OpenOptions{SessionID: state.SessionID})
	if err != nil || resumed.ModelAlias != "gpt" {
		t.Fatalf("resumed = %+v, %v", resumed, err)
	}
}

func TestChatRuntimeInvalidModelIsAtomic(t *testing.T) {
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(context.Background(), chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "missing"}, nil)
	if err == nil || runtime.agent.Model != state.ModelAlias {
		t.Fatalf("model=%q err=%v", runtime.agent.Model, err)
	}
	saved, getErr := sessions.Get(context.Background(), state.SessionID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if saved.ModelAlias != "" {
		t.Fatalf("invalid model persisted %q", saved.ModelAlias)
	}
}

func TestChatRuntimeModelSelectionPreservesCapabilities(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.capabilities = []string{"model-picker", "repo-workspaces"}

	result, err := runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || !reflect.DeepEqual(result.State.Capabilities, runtime.capabilities) {
		t.Fatalf("model result capabilities = %+v, want %v", result.State, runtime.capabilities)
	}
}

func TestChatRuntimeConsecutiveRepoInstallsUseCleanProfileBaselines(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.agent.Model = "gpt"

	builds := 0
	cleanups := make([]int, 3)
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		builds++
		index := builds
		return &agent.Agent{
			Provider: runtime.agent.Provider,
			Tools: tool.NewRegistry(
				runtimeNamedTool("host_keep"),
				runtimeNamedTool("repo_a_denied"),
			),
			System:  "clean profile baseline",
			Model:   "claude",
			Profile: "main",
		}, func() { cleanups[index]++ }, nil
	}

	targetA, err := sessions.Create(ctx, "repo a")
	if err != nil {
		t.Fatal(err)
	}
	targetB, err := sessions.Create(ctx, "repo b")
	if err != nil {
		t.Fatal(err)
	}
	clientA := &runtimeTestCloser{}
	if _, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "a", Repo: "owner/a", Image: "test", SessionID: targetA.ID},
		policy:    &repopolicy.Policy{Body: "REPO_A_POLICY", Tools: repopolicy.ToolFilter{Deny: []string{"repo_a_denied"}}},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_a")),
		client:    clientA,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if runtime.agent.Model != "claude" {
		t.Fatalf("repo A model = %q, want clean profile default claude", runtime.agent.Model)
	}
	if !strings.Contains(runtime.agent.System, "REPO_A_POLICY") || hasTool(runtime.agent.Tools, "repo_a_denied") {
		t.Fatalf("repo A system=%q tools=%v", runtime.agent.System, toolNames(runtime.agent.Tools))
	}

	clientB := &runtimeTestCloser{}
	if _, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "b", Repo: "owner/b", Image: "test", SessionID: targetB.ID},
		policy:    &repopolicy.Policy{Body: "REPO_B_POLICY"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_b")),
		client:    clientB,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("profile baseline builds = %d, want 2", builds)
	}
	if strings.Contains(runtime.agent.System, "REPO_A_POLICY") || !strings.Contains(runtime.agent.System, "REPO_B_POLICY") {
		t.Fatalf("repo B inherited repo A system prompt: %q", runtime.agent.System)
	}
	if !hasTool(runtime.agent.Tools, "repo_a_denied") || hasTool(runtime.agent.Tools, "workspace_a") || !hasTool(runtime.agent.Tools, "workspace_b") {
		t.Fatalf("repo B inherited repo A tools/policy: %v", toolNames(runtime.agent.Tools))
	}
	if cleanups[1] != 1 || cleanups[2] != 0 || clientA.closed != 1 || clientB.closed != 0 {
		t.Fatalf("cleanup after repo B: agents=%v clients=(%d,%d)", cleanups, clientA.closed, clientB.closed)
	}
}

func TestChatRuntimeUnboundRepoUsesOriginalChatProfileAfterBoundRepo(t *testing.T) {
	ctx := context.Background()
	cfg := configuredChatModels()
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"chat-default": {Model: "claude", Tools: config.ToolPolicy{Deny: []string{"chat_denied"}}},
		"repo-a":       {Model: "gpt", Tools: config.ToolPolicy{Deny: []string{"repo_a_denied"}}},
	}
	runtime, sessions := newRuntimeFixture(t, cfg)
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{Profile: "chat-default"}); err != nil {
		t.Fatal(err)
	}

	var builtProfiles []string
	runtime.profileAgentBuilder = func(_ context.Context, profileName string) (*agent.Agent, func(), error) {
		builtProfiles = append(builtProfiles, profileName)
		model := "claude"
		if profileName == "repo-a" {
			model = "gpt"
		}
		return &agent.Agent{
			Provider: runtime.agent.Provider,
			Tools: tool.NewRegistry(
				runtimeNamedTool("host_keep"),
				runtimeNamedTool("chat_denied"),
				runtimeNamedTool("repo_a_denied"),
			),
			System:  "profile baseline: " + profileName,
			Model:   model,
			Profile: profileName,
		}, func() {}, nil
	}

	targetA, err := sessions.Create(ctx, "bound repo a")
	if err != nil {
		t.Fatal(err)
	}
	targetB, err := sessions.Create(ctx, "unbound repo b")
	if err != nil {
		t.Fatal(err)
	}
	resultA, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "a", Repo: "owner/a", Image: "test", SessionID: targetA.ID, Profile: "repo-a"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_a")),
		client:    &runtimeTestCloser{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resultA.State == nil || resultA.State.Profile != "repo-a" || runtime.agent.Model != "gpt" || hasTool(runtime.agent.Tools, "repo_a_denied") {
		t.Fatalf("bound repo A state=%+v model=%q tools=%v", resultA.State, runtime.agent.Model, toolNames(runtime.agent.Tools))
	}

	resultB, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "b", Repo: "owner/b", Image: "test", SessionID: targetB.ID},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_b")),
		client:    &runtimeTestCloser{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builtProfiles, []string{"repo-a", "chat-default"}) {
		t.Fatalf("built profiles = %v, want bound repo then original chat default", builtProfiles)
	}
	if resultB.State == nil || resultB.State.Profile != "chat-default" || runtime.agent.Model != "claude" {
		t.Fatalf("unbound repo B state=%+v model=%q", resultB.State, runtime.agent.Model)
	}
	if !strings.Contains(runtime.agent.System, "profile baseline: chat-default") || strings.Contains(runtime.agent.System, "profile baseline: repo-a") {
		t.Fatalf("unbound repo B system = %q", runtime.agent.System)
	}
	if hasTool(runtime.agent.Tools, "chat_denied") || !hasTool(runtime.agent.Tools, "repo_a_denied") || hasTool(runtime.agent.Tools, "workspace_a") {
		t.Fatalf("unbound repo B inherited repo A policy/tools: %v", toolNames(runtime.agent.Tools))
	}
}

func TestChatRuntimeFailedRepoInstallCleansProvisionalResources(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	oldAgent, oldSession := runtime.agent, runtime.current
	provisionalCleanups := 0
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return &agent.Agent{
			Provider: oldAgent.Provider,
			Tools:    tool.NewRegistry(runtimeNamedTool("host_keep")),
			System:   "provisional",
			Model:    "claude",
			Profile:  "main",
		}, func() { provisionalCleanups++ }, nil
	}
	client := &runtimeTestCloser{}

	_, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "missing", Repo: "owner/missing", Image: "test", SessionID: "missing-session"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_missing")),
		client:    client,
	}, nil)
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("install err = %v, want session not found", err)
	}
	if provisionalCleanups != 1 || client.closed != 1 {
		t.Fatalf("provisional cleanup: agent=%d client=%d", provisionalCleanups, client.closed)
	}
	if runtime.agent != oldAgent || runtime.current != oldSession || runtime.wsClient != nil {
		t.Fatal("failed repo install mutated active runtime state")
	}
}

func TestChatRuntimeFailedRepoAgentBuildCleansReturnedResources(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("profile agent build failed")
	provisionalCleanups := 0
	runtime.profileAgentBuilder = func(context.Context, string) (*agent.Agent, func(), error) {
		return nil, func() { provisionalCleanups++ }, wantErr
	}
	client := &runtimeTestCloser{}

	_, err := runtime.installRepo(ctx, repoInstall{
		workspace: &workspace.Workspace{ID: "failed", Repo: "owner/failed", Image: "test", SessionID: "unused"},
		tools:     tool.NewRegistry(runtimeNamedTool("workspace_failed")),
		client:    client,
	}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("install err = %v, want %v", err, wantErr)
	}
	if provisionalCleanups != 1 || client.closed != 1 {
		t.Fatalf("failed build cleanup: agent=%d client=%d", provisionalCleanups, client.closed)
	}
}

func TestChatRuntimeModelDatabaseFailureIsAtomic(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_model_alias_update
		BEFORE UPDATE OF model_alias ON sessions
		BEGIN SELECT RAISE(ABORT, 'model write failed'); END`); err != nil {
		t.Fatal(err)
	}

	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil)
	if err == nil || runtime.agent.Model != state.ModelAlias {
		t.Fatalf("model=%q err=%v", runtime.agent.Model, err)
	}
	saved, getErr := sessions.Get(ctx, state.SessionID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if saved.ModelAlias != "" {
		t.Fatalf("failed model write persisted %q", saved.ModelAlias)
	}
}

func TestChatRuntimeRemovedPersistedModelRequiresReplacement(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}

	reduced := configuredChatModels()
	delete(reduced.Models, "gpt")
	second := newRuntimeAgainstSameStore(t, reduced, sessions)
	resumed, err := second.Open(ctx, chatpkg.OpenOptions{SessionID: state.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ModelAlias != "gpt" || resumed.ModelError == "" {
		t.Fatalf("resumed = %+v, want unavailable gpt with model error", resumed)
	}
	if second.agent.Model != "claude" {
		t.Fatalf("agent silently selected %q, want configured default unchanged", second.agent.Model)
	}
	if len(resumed.Models) != 1 || resumed.Models[0].Alias != "claude" {
		t.Fatalf("picker models = %+v", resumed.Models)
	}
	if err := second.Turn(ctx, "must not run", nil); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("Turn while model unavailable err = %v", err)
	}
	result, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.ModelError != "" || result.State.ModelAlias != "claude" {
		t.Fatalf("replacement result = %+v", result)
	}
}

func TestChatRuntimeTurnEmitsHooksAndPersistsHistory(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{
		{response: llm.Response{
			StopReason: llm.StopToolUse,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
				Type:    llm.BlockToolUse,
				ToolUse: &llm.ToolUse{ID: "call-1", Name: "runtime_test", Input: json.RawMessage(`{"ok":true}`)},
			}}},
			Usage: llm.Usage{InputTokens: 3, OutputTokens: 5},
		}},
		{response: llm.Response{
			StopReason: llm.StopEndTurn,
			Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}}},
			Usage:      llm.Usage{InputTokens: 2, OutputTokens: 7},
		}, stream: "done"},
	}}
	runtime.agent.Tools = tool.NewRegistry(runtimeTestTool{})

	var events []chatpkg.Event
	if err := runtime.Turn(ctx, "run the tool", func(event chatpkg.Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	wantKinds := []chatpkg.EventKind{
		chatpkg.EventToolStarted, chatpkg.EventToolFinished,
		chatpkg.EventTextDelta, chatpkg.EventTurnDone,
	}
	for _, want := range wantKinds {
		if !eventKinds(events)[want] {
			t.Fatalf("events = %+v, missing %s", events, want)
		}
	}
	done := events[len(events)-1]
	if done.Kind != chatpkg.EventTurnDone || done.Usage != (llm.Usage{InputTokens: 5, OutputTokens: 12}) {
		t.Fatalf("turn_done = %+v", done)
	}
	turns, err := sessions.Turns(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != len(runtime.history) || len(turns) != 4 || runtime.persisted != 4 {
		t.Fatalf("persisted turns=%d history=%d index=%d", len(turns), len(runtime.history), runtime.persisted)
	}
	saved, err := sessions.Get(ctx, state.SessionID)
	if err != nil || saved.Title != "run the tool" {
		t.Fatalf("title=%q err=%v", saved.Title, err)
	}
}

func TestChatRuntimeTurnPersistsUserMessageAndDoneEventOnRunError(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("provider failed")}}}
	var events []chatpkg.Event
	err = runtime.Turn(ctx, "keep this", func(event chatpkg.Event) { events = append(events, event) })
	if err == nil || err.Error() != "provider failed" {
		t.Fatalf("Turn err = %v", err)
	}
	turns, loadErr := sessions.Turns(ctx, state.SessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(turns) != 1 || turns[0].Text() != "keep this" {
		t.Fatalf("persisted turns = %+v", turns)
	}
	if len(events) == 0 || events[len(events)-1].Kind != chatpkg.EventTurnDone || !events[len(events)-1].IsError {
		t.Fatalf("events = %+v", events)
	}
}

func TestChatRuntimeCancelOnlyCancelsActiveTurn(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	provider := &runtimeBlockingProvider{started: make(chan struct{})}
	runtime.agent.Provider = provider
	errCh := make(chan error, 1)
	go func() { errCh <- runtime.Turn(ctx, "wait", nil) }()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	if err := runtime.Turn(ctx, "overlap", nil); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("overlapping Turn err = %v", err)
	}
	runtime.Cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Turn err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active turn was not canceled")
	}
	runtime.Cancel()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.agentCancel != nil {
		t.Fatal("active cancellation was not cleared")
	}
}

func TestChatRuntimeActiveNewOneConfirmationPreservesOldHistory(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &runtimeBlockingProvider{started: make(chan struct{})}
	runtime.agent.Provider = provider
	turnDone := make(chan error, 1)
	go func() { turnDone <- runtime.Turn(ctx, "preserve this", nil) }()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil)
	if err != nil || !result.Confirm {
		t.Fatalf("active /new = %+v, %v", result, err)
	}
	result, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, nil)
	if err != nil {
		t.Fatalf("single confirmation: %v", err)
	}
	if err := <-turnDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("active turn error = %v", err)
	}
	if result.State == nil || result.State.SessionID == state.SessionID {
		t.Fatalf("new state = %+v", result.State)
	}
	turns, err := sessions.Turns(ctx, state.SessionID)
	if err != nil || len(turns) != 1 || turns[0].Text() != "preserve this" {
		t.Fatalf("old turns = %+v, %v", turns, err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, nil); err == nil {
		t.Fatal("stale confirmation succeeded")
	}
}

func TestChatRuntimeCommandResults(t *testing.T) {
	tests := []struct {
		name    string
		command chatpkg.ParsedCommand
		prepare func(*testing.T, *chatRuntime, *session.Store)
		check   func(*testing.T, chatpkg.Result, *chatRuntime)
	}{
		{
			name: "help", command: chatpkg.ParsedCommand{Name: chatpkg.CommandHelp},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Chat commands" || !reflect.DeepEqual(got.Commands, chatpkg.Commands()) {
					t.Fatalf("help result = %+v", got)
				}
			},
		},
		{
			name: "model picker", command: chatpkg.ParsedCommand{Name: chatpkg.CommandModel},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Choose a model" || aliases(got.Models) != "claude,gpt" {
					t.Fatalf("model picker = %+v", got)
				}
			},
		},
		{
			name: "models", command: chatpkg.ParsedCommand{Name: chatpkg.CommandModels},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Configured models" || aliases(got.Models) != "claude,gpt" || got.Models[0].Provider != "local" || got.Models[0].Upstream != "upstream-claude" {
					t.Fatalf("models result = %+v", got)
				}
			},
		},
		{
			name: "new", command: chatpkg.ParsedCommand{Name: chatpkg.CommandNew},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				runtime.history = []llm.Message{llm.UserText("old history")}
			},
			check: func(t *testing.T, got chatpkg.Result, runtime *chatRuntime) {
				t.Helper()
				if !got.Confirm || got.State != nil || len(runtime.history) != 1 {
					t.Fatalf("new result = %+v history=%d", got, len(runtime.history))
				}
			},
		},
		{
			name: "new confirmed", command: chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				runtime.history = []llm.Message{llm.UserText("old history")}
				result, err := runtime.Command(context.Background(), chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil)
				if err != nil || !result.Confirm {
					t.Fatalf("request confirmation result=%+v err=%v", result, err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, runtime *chatRuntime) {
				t.Helper()
				if got.Confirm || got.State == nil || got.State.SessionID == "" || len(runtime.history) != 0 {
					t.Fatalf("confirmed new result = %+v history=%d", got, len(runtime.history))
				}
			},
		},
		{
			name: "sessions", command: chatpkg.ParsedCommand{Name: chatpkg.CommandSessions},
			prepare: func(t *testing.T, _ *chatRuntime, sessions *session.Store) {
				t.Helper()
				if _, err := sessions.Create(context.Background(), "second"); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Recent sessions" || len(got.Sessions) != 2 {
					t.Fatalf("sessions result = %+v", got)
				}
			},
		},
		{
			name: "resume picker", command: chatpkg.ParsedCommand{Name: chatpkg.CommandResume},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Resume a session" || len(got.Sessions) != 1 {
					t.Fatalf("resume picker = %+v", got)
				}
			},
		},
		{
			name: "status", command: chatpkg.ParsedCommand{Name: chatpkg.CommandStatus},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Chat status" || got.State == nil || got.State.ConnectionMode != "direct" || got.State.Profile != "main" || got.State.ProviderLabel != "local (openai)" {
					t.Fatalf("status result = %+v", got)
				}
			},
		},
		{
			name: "usage", command: chatpkg.ParsedCommand{Name: chatpkg.CommandUsage},
			prepare: func(t *testing.T, runtime *chatRuntime, sessions *session.Store) {
				t.Helper()
				ctx := context.Background()
				usageStore := usagepkg.New(runtime.st)
				if err := usageStore.AddRequestAt(ctx, runtime.current.ID, llm.Usage{InputTokens: 2, OutputTokens: 3}, time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
				other, err := sessions.Create(ctx, "other")
				if err != nil {
					t.Fatal(err)
				}
				if err := usageStore.AddRequestAt(ctx, other.ID, llm.Usage{InputTokens: 4, OutputTokens: 5}, time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				want := "Current session totals: requests=2 input=4 output=6 reserved=0\nPersisted aggregate totals: requests=4 input=12 output=16 reserved=0"
				if got.Title != "Usage" || len(got.Usage) != 6 || got.Text != want {
					t.Fatalf("usage result = %+v\ntext=%q", got, got.Text)
				}
			},
		},
		{
			name: "permissions", command: chatpkg.ParsedCommand{Name: chatpkg.CommandPermissions},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Effective permissions" || got.Permissions == nil || got.Permissions.SandboxMode != "host" {
					t.Fatalf("permissions result = %+v", got)
				}
			},
		},
		{
			name: "skill", command: chatpkg.ParsedCommand{Name: chatpkg.CommandSkill, Args: "audit fast"},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				path := filepath.Join(t.TempDir(), "SKILL.md")
				if err := os.WriteFile(path, []byte("---\nname: audit\n---\nInspect carefully."), 0o600); err != nil {
					t.Fatal(err)
				}
				runtime.skills = []skill.Skill{{Name: "audit", Path: path}}
				runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
					StopReason: llm.StopEndTurn,
					Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "audited"}}},
				}}}}
			},
			check: func(t *testing.T, got chatpkg.Result, runtime *chatRuntime) {
				t.Helper()
				if got.Text != "skill audit completed" || len(runtime.history) != 2 || !strings.Contains(runtime.history[0].Text(), "User arguments: fast") {
					t.Fatalf("skill result = %+v history=%+v", got, runtime.history)
				}
			},
		},
		{
			name: "workset", command: chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset},
			prepare: func(t *testing.T, runtime *chatRuntime, _ *session.Store) {
				t.Helper()
				ws := &workset.Store{DB: runtime.st.DB}
				if _, err := ws.Add(context.Background(), runtime.current.ID, workset.KindGoal, "finish task", workset.SourceUser, true); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got chatpkg.Result, _ *chatRuntime) {
				t.Helper()
				if got.Title != "Working set" || len(got.Workset) != 1 || got.Workset[0].Text != "finish task" {
					t.Fatalf("workset result = %+v", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, sessions := newRuntimeFixture(t, configuredChatModels())
			if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
				t.Fatal(err)
			}
			if tt.prepare != nil {
				tt.prepare(t, runtime, sessions)
			}
			got, err := runtime.Command(context.Background(), tt.command, nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, got, runtime)
		})
	}
}

func TestChatRuntimeCommandUsageErrors(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(context.Background(), chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		command chatpkg.ParsedCommand
		want    string
	}{
		{chatpkg.ParsedCommand{Name: chatpkg.CommandSkill}, "usage: /skill <name> [args]"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandRepo}, "usage: /repo <owner/repo>"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "replace only"}, "usage: /workset replace <id> <text>"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "drop"}, "usage: /workset drop <id>"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "clear extra"}, "usage: /workset clear"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandWorkset, Args: "wat"}, "usage: /workset [list|replace <id> <text>|drop <id>|clear]"},
		{chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "now"}, "usage: /new"},
	} {
		_, err := runtime.Command(context.Background(), tt.command, nil)
		if err == nil || err.Error() != tt.want {
			t.Errorf("Command(%+v) err = %v, want %q", tt.command, err, tt.want)
		}
	}
}

func TestChatRuntimeNewConfirmationRejectsDirectAndStaleConfirmation(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("direct confirmation err = %v", err)
	}
	if runtime.current.ID != state.SessionID {
		t.Fatal("direct confirmation changed the session")
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request confirmation result=%+v err=%v", result, err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("stale confirmation after model change err = %v", err)
	}
	if runtime.current.ID != state.SessionID {
		t.Fatal("stale confirmation changed the session")
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("second confirmation result=%+v err=%v", result, err)
	}
	runtime.Cancel()
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("stale confirmation after Cancel err = %v", err)
	}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("third confirmation result=%+v err=%v", result, err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
		StopReason: llm.StopEndTurn,
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "changed"}}},
	}}}}
	if err := runtime.Turn(ctx, "intervening turn", nil); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err == nil || err.Error() != "no pending /new confirmation" {
		t.Fatalf("stale confirmation after Turn err = %v", err)
	}
}

func TestChatRuntimeResumeLoadsBeforeMutatingStateAndRestoresModel(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	initial, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, target.ID, "gpt"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, target.ID, llm.UserText("earlier")); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.SessionID != target.ID || result.State.ModelAlias != "gpt" || runtime.agent.Model != "gpt" || len(runtime.history) != 1 {
		t.Fatalf("resume result = %+v agent=%q history=%d", result, runtime.agent.Model, len(runtime.history))
	}

	corrupt, err := sessions.Create(ctx, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.DB().ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		VALUES (?, 1, 'user', 'not json', '', ?)`, corrupt.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: corrupt.ID}, nil)
	if err == nil {
		t.Fatal("resume corrupt session succeeded")
	}
	if runtime.current.ID != target.ID || runtime.agent.Model != "gpt" || len(runtime.history) != 1 {
		t.Fatalf("failed resume mutated current=%s model=%s history=%d (initial=%s)", runtime.current.ID, runtime.agent.Model, len(runtime.history), initial.SessionID)
	}
}

func TestChatRuntimeExitWarnsOnReflectionFailureAndCleansUpOnce(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = 2
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed")}}}
	closed := 0
	runtime.wsClient = closeFunc(func() error { closed++; return nil })
	cleaned := 0
	runtime.agentCleanup = func() { cleaned++ }
	var events []chatpkg.Event
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandExit}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if !result.ShouldClose || !strings.Contains(result.Text, "warning") || len(events) != 1 || events[0].Kind != chatpkg.EventNotice || !events[0].IsError {
		t.Fatalf("exit result=%+v events=%+v", result, events)
	}
	if closed != 1 || cleaned != 1 {
		t.Fatalf("cleanup counts client=%d agent=%d", closed, cleaned)
	}
	if err := runtime.Close(ctx); err == nil || !strings.Contains(err.Error(), "reflection failed") {
		t.Fatalf("Close after exit err = %v", err)
	}
	if closed != 1 || cleaned != 1 {
		t.Fatalf("cleanup repeated client=%d agent=%d", closed, cleaned)
	}
}

func TestChatRuntimeRedactsTurnFailureAndPreservesCause(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-provider-canary-7391"
	providerErr := errors.New("provider failed with " + secret)
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: providerErr}}}
	var events []chatpkg.Event
	err := runtime.Turn(ctx, "hello", func(event chatpkg.Event) { events = append(events, event) })
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted:test]") {
		t.Fatalf("Turn error = %v", err)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("Turn error lost cause: %v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Text, secret) {
			t.Fatalf("event leaked secret: %+v", event)
		}
	}
}

func TestRedactChatStateDoesNotMutateHistoryAndCoversToolPayloads(t *testing.T) {
	const secret = "opaque-history-canary-2551"
	toolUse := &llm.ToolUse{ID: secret, Name: "read", Input: json.RawMessage(`{"path":"` + secret + `"}`)}
	toolResult := &llm.ToolResult{ToolUseID: secret, Content: secret}
	history := []llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, Text: secret, ToolUse: toolUse}, {Type: llm.BlockToolResult, ToolResult: toolResult}}}}
	state := chatpkg.State{History: append([]llm.Message(nil), history...)}
	redacted := redactChatState(state, func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") })
	if strings.Contains(fmt.Sprintf("%+v", redacted.History), secret) || strings.Contains(string(redacted.History[0].Blocks[0].ToolUse.Input), secret) {
		t.Fatalf("redacted history leaked secret: %+v", redacted.History)
	}
	if history[0].Blocks[0].Text != secret || history[0].Blocks[0].ToolUse.ID != secret || !strings.Contains(string(history[0].Blocks[0].ToolUse.Input), secret) || history[0].Blocks[1].ToolResult.Content != secret {
		t.Fatalf("redaction mutated runtime history: %+v", history)
	}
}

func TestChatRuntimeRedactsReflectionWarning(t *testing.T) {
	ctx := context.Background()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-reflection-canary-4826"
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed with " + secret)}}}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = len(runtime.history)
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request /new = %+v, %v", result, err)
	}
	var events []chatpkg.Event
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	visible := result.Text
	for _, event := range events {
		visible += "\n" + event.Text
	}
	if strings.Contains(visible, secret) || !strings.Contains(visible, "[redacted:test]") {
		t.Fatalf("visible warning = %q", visible)
	}
}

func TestChatRuntimeSocketRedactsConfiguredReflectionCanary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	socketDir, err := os.MkdirTemp("/tmp", "waffle-chat-redact-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	path := filepath.Join(socketDir, "chat.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- chatwire.Serve(ctx, listener, func(context.Context) (chatpkg.Backend, error) { return runtime, nil }, nil)
	}()
	client, err := chatwire.Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-socket-reflection-6017"
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed with " + secret)}}}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = len(runtime.history)
	if result, err := client.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request /new = %+v, %v", result, err)
	}
	var events []chatpkg.Event
	result, err := client.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: chatNewConfirmArg}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	visible := result.Text
	for _, event := range events {
		visible += "\n" + event.Text
	}
	if strings.Contains(visible, secret) || !strings.Contains(visible, "[redacted:test]") {
		t.Fatalf("socket-visible warning = %q", visible)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = listener.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chatwire serve did not stop")
	}
}

func TestChatRuntimeSessionOwnershipBusySwitchCloseAndReacquire(t *testing.T) {
	ctx := context.Background()
	first, sessions := newRuntimeFixture(t, configuredChatModels())
	target, err := sessions.Create(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	owners := newChatSessionOwners()
	first.sessionOwners = owners
	if _, err := first.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatal(err)
	}

	second := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	second.sessionOwners = owners
	secondState, err := second.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("busy resume error = %v", err)
	}
	if second.current.ID != secondState.SessionID {
		t.Fatalf("busy resume changed session to %s", second.current.ID)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil || result.State == nil || result.State.SessionID != target.ID {
		t.Fatalf("resume after release = %+v, %v", result, err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}

	third := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	third.sessionOwners = owners
	if _, err := third.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatalf("reacquire after switch close: %v", err)
	}
}

func TestChatRuntimeSessionOwnershipAllowsDifferentSessions(t *testing.T) {
	ctx := context.Background()
	first, sessions := newRuntimeFixture(t, configuredChatModels())
	owners := newChatSessionOwners()
	first.sessionOwners = owners
	second := newRuntimeAgainstSameStore(t, configuredChatModels(), sessions)
	second.sessionOwners = owners
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, runtime := range []*chatRuntime{first, second} {
		wg.Add(1)
		go func(runtime *chatRuntime) {
			defer wg.Done()
			_, err := runtime.Open(ctx, chatpkg.OpenOptions{})
			errs <- err
		}(runtime)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if first.current.ID == second.current.ID {
		t.Fatalf("different opens shared session %s", first.current.ID)
	}
}

func TestChatRuntimeSocketSessionOwnershipSwitchCloseAndReacquire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, sessions := newRuntimeFixture(t, configuredChatModels())
	target, err := sessions.Create(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	owners := newChatSessionOwners()
	factory := func(context.Context) (chatpkg.Backend, error) {
		runtime, runtimeErr := newChatRuntime(ctx, configuredChatModels(), &store.Store{DB: sessions.DB()})
		if runtimeErr == nil {
			runtime.sessionOwners = owners
		}
		return runtime, runtimeErr
	}
	socketDir, err := os.MkdirTemp("/tmp", "waffle-chat-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	listener, err := net.Listen("unix", filepath.Join(socketDir, "chat.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- chatwire.Serve(ctx, listener, factory, nil) }()
	dial := func() *chatwire.Client {
		client, dialErr := chatwire.Dial(ctx, listener.Addr().String())
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		return client
	}
	first := dial()
	if _, err := first.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatal(err)
	}
	second := dial()
	secondState, err := second.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	var remote *chatwire.RemoteError
	if !errors.As(err, &remote) || remote.Code != "session_active" || !strings.Contains(remote.Message, "already active") {
		t.Fatalf("busy socket resume = %#v", err)
	}
	status, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandStatus}, nil)
	if err != nil || status.State == nil || status.State.SessionID != secondState.SessionID {
		t.Fatalf("state after busy resume = %+v, %v", status, err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := second.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil || resumed.State == nil || resumed.State.SessionID != target.ID {
		t.Fatalf("resume after close = %+v, %v", resumed, err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	third := dial()
	if _, err := third.Open(ctx, chatpkg.OpenOptions{SessionID: target.ID}); err != nil {
		t.Fatalf("reacquire after switched owner close: %v", err)
	}
	if err := third.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = listener.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chatwire serve did not stop")
	}
}

func TestChatRuntimeNewReflectsOldSessionAndRestoresProfileModel(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{
		llm.UserText("question"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}},
	}
	for _, message := range runtime.history {
		if err := sessions.AppendTurn(ctx, state.SessionID, message); err != nil {
			t.Fatal(err)
		}
	}
	runtime.persisted = 2
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
		StopReason: llm.StopEndTurn,
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "old session summary"}}},
	}}}}
	if result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew}, nil); err != nil || !result.Confirm {
		t.Fatalf("request confirmation result=%+v err=%v", result, err)
	}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandNew, Args: "confirm"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Summary != "old session summary" {
		t.Fatalf("old summary = %q", old.Summary)
	}
	if result.State == nil || result.State.SessionID == state.SessionID || result.State.ModelAlias != "claude" || runtime.agent.Model != "claude" {
		t.Fatalf("new result=%+v agent=%q", result, runtime.agent.Model)
	}
}

func TestChatRuntimeResumeReflectsSessionBeingLeft(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	state, err := runtime.Open(ctx, chatpkg.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	for _, message := range runtime.history {
		if err := sessions.AppendTurn(ctx, state.SessionID, message); err != nil {
			t.Fatal(err)
		}
	}
	runtime.persisted = len(runtime.history)
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{response: llm.Response{
		StopReason: llm.StopEndTurn,
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "resume summary"}}},
	}}}}
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, nil)
	if err != nil || result.State == nil || result.State.SessionID != target.ID {
		t.Fatalf("resume = %+v, %v", result, err)
	}
	old, err := sessions.Get(ctx, state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Summary != "resume summary" {
		t.Fatalf("old summary = %q", old.Summary)
	}
}

func TestChatRuntimeResumeReflectionFailureWarnsRedactedAndStillSwitches(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredChatModels())
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.history = []llm.Message{llm.UserText("question"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "answer"}}}}
	runtime.persisted = len(runtime.history)
	target, err := sessions.Create(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-resume-reflection-1934"
	runtime.agent.Redact = func(value string) string { return strings.ReplaceAll(value, secret, "[redacted:test]") }
	runtime.agent.Provider = &runtimeScriptedProvider{responses: []runtimeProviderStep{{err: errors.New("reflection failed with " + secret)}}}
	var events []chatpkg.Event
	result, err := runtime.Command(ctx, chatpkg.ParsedCommand{Name: chatpkg.CommandResume, Args: target.ID}, func(event chatpkg.Event) { events = append(events, event) })
	if err != nil || result.State == nil || result.State.SessionID != target.ID {
		t.Fatalf("resume = %+v, %v", result, err)
	}
	visible := result.Text
	for _, event := range events {
		visible += "\n" + event.Text
	}
	if strings.Contains(visible, secret) || !strings.Contains(visible, "[redacted:test]") || !strings.Contains(visible, "warning") {
		t.Fatalf("visible warning = %q", visible)
	}
}

func TestChatRuntimeStatusUsesProfileSandboxOverride(t *testing.T) {
	cfg := configuredChatModels()
	cfg.Sandbox.Mode = "docker"
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"safe": {Sandbox: "host"},
	}
	runtime, _ := newRuntimeFixture(t, cfg)
	state, err := runtime.Open(context.Background(), chatpkg.OpenOptions{Profile: "safe"})
	if err != nil {
		t.Fatal(err)
	}
	if state.SandboxMode != "host" || state.Profile != "safe" {
		t.Fatalf("profile state = %+v", state)
	}
}

func TestChatRuntimeClosedStateRejectsFurtherUse(t *testing.T) {
	runtime, _ := newRuntimeFixture(t, configuredChatModels())
	ctx := context.Background()
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Turn(ctx, "after close", nil); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Turn after Close err = %v", err)
	}
	second, err := newChatRuntime(ctx, runtime.cfg, runtime.st)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Open(ctx, chatpkg.OpenOptions{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Open after Close err = %v", err)
	}
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func aliases(models []chatpkg.Model) string {
	values := make([]string, len(models))
	for i, model := range models {
		values[i] = model.Alias
	}
	return strings.Join(values, ",")
}

type runtimeProviderStep struct {
	response llm.Response
	stream   string
	err      error
}

type runtimeScriptedProvider struct {
	mu        sync.Mutex
	responses []runtimeProviderStep
}

func (p *runtimeScriptedProvider) Complete(_ context.Context, _ llm.Request, onEvent llm.StreamFunc) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.responses) == 0 {
		return nil, errors.New("no scripted response")
	}
	step := p.responses[0]
	p.responses = p.responses[1:]
	if step.stream != "" && onEvent != nil {
		onEvent(llm.Event{Type: llm.EventTextDelta, Text: step.stream})
	}
	if step.err != nil {
		return nil, step.err
	}
	response := step.response
	return &response, nil
}

type runtimeBlockingProvider struct{ started chan struct{} }

func (p *runtimeBlockingProvider) Complete(ctx context.Context, _ llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type runtimeTestTool struct{}

func (runtimeTestTool) Def() llm.Tool {
	return llm.Tool{Name: "runtime_test", Description: "runtime test", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (runtimeTestTool) Run(context.Context, json.RawMessage) (string, error) {
	return "tool output", nil
}

type runtimeNamedTool string

func (name runtimeNamedTool) Def() llm.Tool {
	return llm.Tool{Name: string(name), Description: "runtime named test tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (runtimeNamedTool) Run(context.Context, json.RawMessage) (string, error) {
	return "tool output", nil
}

type runtimeTestCloser struct{ closed int }

func (c *runtimeTestCloser) Close() error {
	c.closed++
	return nil
}

func hasTool(box tool.Toolbox, name string) bool {
	for _, def := range box.Defs() {
		if def.Name == name {
			return true
		}
	}
	return false
}

func toolNames(box tool.Toolbox) []string {
	defs := box.Defs()
	names := make([]string, len(defs))
	for i, def := range defs {
		names[i] = def.Name
	}
	return names
}

func eventKinds(events []chatpkg.Event) map[chatpkg.EventKind]bool {
	kinds := make(map[chatpkg.EventKind]bool, len(events))
	for _, event := range events {
		kinds[event.Kind] = true
	}
	return kinds
}

func configuredChatModels() config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConnection{
		"local": {Type: "openai", BaseURL: "https://models.invalid/v1"},
	}
	cfg.Models = map[string]config.ModelTarget{
		"claude": {Provider: "local", Model: "upstream-claude"},
		"gpt":    {Provider: "local", Model: "upstream-gpt"},
	}
	cfg.Agent.DefaultModel = "claude"
	cfg.Agent.UtilityModel = ""
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	return cfg
}

func newRuntimeFixture(t *testing.T, cfg config.Config) (*chatRuntime, *session.Store) {
	t.Helper()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runtime, err := newChatRuntime(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime, session.New(st)
}

func newRuntimeAgainstSameStore(t *testing.T, cfg config.Config, sessions *session.Store) *chatRuntime {
	t.Helper()
	runtime, err := newChatRuntime(context.Background(), cfg, &store.Store{DB: sessions.DB()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime
}
