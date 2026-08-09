package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/broker"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/workspace"
)

const (
	chatNewConfirmArg            = "confirm"
	chatRuntimeCloseTimeout      = 10 * time.Second
	maxAttachedSkillContextBytes = 256 << 10
)

// chatRuntime owns the presentation-neutral state and behavior for one chat
// connection. Renderers are responsible only for displaying its state,
// events, and command results.
type chatRuntime struct {
	mu                  sync.Mutex
	commandMu           sync.Mutex
	agent               *agent.Agent
	agentCancel         context.CancelFunc
	commandCancel       context.CancelFunc
	commandDone         chan struct{}
	sessions            *session.Store
	current             *session.Session
	history             []llm.Message
	persisted           int
	cfg                 config.Config
	st                  *store.Store
	skills              []skill.Skill
	baseSystem          string
	skillWorkspace      memory.Workspace
	attachedSkills      []chatpkg.SkillRef
	profileName         string
	chatProfileName     string
	agentCleanupContext agentCleanupContext
	wsBroker            *broker.Broker
	wsURL               string
	wsClient            io.Closer

	modelError          string
	workspace           string
	capabilities        []string
	resourceCtx         context.Context
	resourceCancel      context.CancelFunc
	activeTurn          uint64
	nextTurn            uint64
	turnDone            chan struct{}
	blockTurns          bool
	pendingNewSessionID string
	closeTimeout        time.Duration
	cleanupStarted      bool
	cleanupDone         chan struct{}
	cleanupComplete     bool
	closeErr            error
	opening             bool
	closed              bool
	profileAgentBuilder func(context.Context, string) (*agent.Agent, func(), error)
	repoOpener          func(context.Context, string, string) (repoInstall, error)
	sessionOwners       *chatSessionOwners
	ownedSessionID      string
	retiredCleanup      []*chatRuntimeCleanup
}

type repoInstall struct {
	workspace *workspace.Workspace
	policy    *repopolicy.Policy
	tools     tool.Toolbox
	client    io.Closer
}

func newChatRuntime(_ context.Context, cfg config.Config, st *store.Store) (*chatRuntime, error) {
	if st == nil || st.DB == nil {
		return nil, errors.New("chat runtime requires an open store")
	}
	return &chatRuntime{cfg: cfg, st: st, sessions: session.New(st)}, nil
}

func (r *chatRuntime) Open(ctx context.Context, options chatpkg.OpenOptions) (chatpkg.State, error) {
	state, err := r.open(ctx, options)
	redact := r.runtimeRedactor()
	return redactChatState(state, redact), redactChatError(err, redact)
}

func (r *chatRuntime) open(ctx context.Context, options chatpkg.OpenOptions) (chatpkg.State, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return chatpkg.State{}, errors.New("chat runtime is closed")
	}
	if r.current != nil || r.opening {
		r.mu.Unlock()
		return chatpkg.State{}, errors.New("chat runtime is already open")
	}
	r.opening = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.opening = false
		r.mu.Unlock()
	}()

	profileName := strings.TrimSpace(options.Profile)
	if profileName != "" && !config.ValidProfileName(profileName) {
		return chatpkg.State{}, fmt.Errorf("invalid profile name %q", profileName)
	}
	if _, ok := r.cfg.Profile(profileName); !ok {
		return chatpkg.State{}, fmt.Errorf("unknown agent profile %q", profileName)
	}

	resourceCtx, resourceCancel := context.WithCancel(context.WithoutCancel(ctx))
	ws, skills, err := loadWorkspaceWithStore(r.st)
	if err != nil {
		resourceCancel()
		return chatpkg.State{}, err
	}
	built, cleanup, err := buildAgentWithProfileContext(resourceCtx, r.cfg, ws, skills, r.sessions, config.GroupMain, profileName)
	if err != nil {
		_ = cleanup(resourceCtx)
		resourceCancel()
		return chatpkg.State{}, err
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = cleanup(resourceCtx)
			resourceCancel()
		}
	}()

	var current *session.Session
	switch {
	case strings.TrimSpace(options.SessionID) != "":
		current, err = r.sessions.Get(ctx, strings.TrimSpace(options.SessionID))
	case options.Continue:
		current, err = r.sessions.Latest(ctx)
		if errors.Is(err, session.ErrNotFound) {
			err = nil
		}
	}
	if err != nil {
		return chatpkg.State{}, err
	}
	if current == nil {
		current, err = r.sessions.Create(ctx, "")
		if err != nil {
			return chatpkg.State{}, err
		}
	}

	var history []llm.Message
	if options.Continue || strings.TrimSpace(options.SessionID) != "" {
		history, err = r.sessions.Turns(ctx, current.ID)
		if err != nil {
			return chatpkg.State{}, err
		}
		history = session.Repair(history)
	}
	attachedNames, err := (&skill.Attachments{DB: r.st.DB, Lifecycle: r.st.SkillLifecycleGuard()}).List(ctx, current.ID)
	if err != nil {
		return chatpkg.State{}, err
	}
	attachedSkills, attachedSystem, err := buildAttachedSkillContext(built.System, skills, attachedNames)
	if err != nil {
		return chatpkg.State{}, err
	}
	if !r.sessionOwners.acquire(r, current.ID) {
		return chatpkg.State{}, sessionAlreadyActiveError{sessionID: current.ID}
	}
	ownershipAcquired := true
	defer func() {
		if ownershipAcquired {
			_ = r.sessionOwners.releaseContext(context.Background(), r, current.ID)
		}
	}()

	modelError := ""
	if current.ModelAlias != "" {
		if _, resolveErr := r.cfg.ResolveModel(current.ModelAlias); resolveErr != nil {
			modelError = resolveErr.Error()
		} else {
			built.Model = current.ModelAlias
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return chatpkg.State{}, errors.New("chat runtime is closed")
	}
	if r.current != nil {
		r.mu.Unlock()
		return chatpkg.State{}, errors.New("chat runtime is already open")
	}
	r.agent = built
	r.baseSystem = built.System
	r.agent.System = attachedSystem
	r.attachedSkills = attachedSkills
	r.agentCleanupContext = cleanup
	r.skills = skills
	r.skillWorkspace = ws
	r.profileName = profileName
	r.chatProfileName = profileName
	r.resourceCtx = resourceCtx
	r.resourceCancel = resourceCancel
	r.capabilities = append([]string(nil), options.Capabilities...)
	r.current = current
	r.ownedSessionID = current.ID
	r.history = history
	r.persisted = len(history)
	r.modelError = modelError
	state := r.stateLocked(r.capabilities)
	r.mu.Unlock()
	adopted = true
	ownershipAcquired = false
	return state, nil
}

func (r *chatRuntime) modelsLocked() []chatpkg.Model {
	models := make([]chatpkg.Model, 0, len(r.cfg.Models))
	current := ""
	if r.current != nil && r.current.ModelAlias != "" {
		current = r.current.ModelAlias
	} else if r.agent != nil {
		current = r.agent.Model
	}
	for alias := range r.cfg.Models {
		target, err := r.cfg.ResolveModel(alias)
		if err != nil {
			continue
		}
		models = append(models, chatpkg.Model{
			Alias: alias, Provider: target.ConnectionName,
			Upstream: target.UpstreamModel, Current: alias == current,
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Alias < models[j].Alias })
	return models
}

func (r *chatRuntime) stateLocked(capabilities []string) chatpkg.State {
	state := chatpkg.State{
		Profile:        "main",
		ConnectionMode: "direct",
		SandboxMode:    r.resolvedPolicy().Mode,
		Workspace:      r.workspace,
		ModelError:     r.modelError,
		Models:         r.modelsLocked(),
		Skills:         append([]chatpkg.SkillRef(nil), r.attachedSkills...),
		Capabilities:   append([]string(nil), capabilities...),
		History:        append([]llm.Message(nil), r.history...),
	}
	if r.current != nil {
		state.SessionID = r.current.ID
		state.Title = r.current.Title
		state.ModelAlias = r.current.ModelAlias
	}
	if r.agent != nil {
		if state.ModelAlias == "" {
			state.ModelAlias = r.agent.Model
		}
		state.Profile = r.agent.Profile
		state.ProviderLabel = r.providerLabelLocked()
	}
	if r.modelError != "" && r.current != nil {
		state.ModelAlias = r.current.ModelAlias
	}
	return state
}

func (r *chatRuntime) providerLabelLocked() string {
	if r.agent == nil {
		return ""
	}
	if runtime, ok := r.agent.Provider.(*modelRuntimeResolver); ok {
		if target, err := runtime.resolveTarget(r.agent.Model); err == nil {
			return fmt.Sprintf("%s (%s)", target.ConnectionName, target.Connection.Type)
		}
	}
	return r.cfg.Provider.Name
}

func (r *chatRuntime) Turn(ctx context.Context, input string, emit func(chatpkg.Event)) error {
	redact := r.runtimeRedactor()
	redactedEmit := func(event chatpkg.Event) {
		if emit != nil {
			emit(redactChatEvent(event, redact))
		}
	}
	return redactChatError(r.turn(ctx, input, redactedEmit), redact)
}

func (r *chatRuntime) turn(ctx context.Context, input string, emit func(chatpkg.Event)) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("chat runtime is closed")
	}
	if r.blockTurns {
		r.mu.Unlock()
		return errors.New("a chat command is changing runtime state")
	}
	r.pendingNewSessionID = ""
	if r.current == nil || r.agent == nil {
		r.mu.Unlock()
		return errors.New("chat runtime is not open")
	}
	if r.modelError != "" {
		err := fmt.Errorf("select an available model before sending a turn: %s", r.modelError)
		r.mu.Unlock()
		return err
	}
	if r.agentCancel != nil {
		r.mu.Unlock()
		return errors.New("a chat turn is already active")
	}

	turnCtx, cancel := context.WithCancel(ctx)
	r.nextTurn++
	turnID := r.nextTurn
	r.activeTurn = turnID
	r.agentCancel = cancel
	turnDone := make(chan struct{})
	r.turnDone = turnDone
	current := r.current
	runner := r.agent
	if len(r.history) == 0 && current.Title == "" {
		title := truncateChatTitle(input)
		if err := r.sessions.SetTitle(turnCtx, current.ID, title); err == nil {
			current.Title = title
		}
	}
	r.history = append(r.history, llm.UserText(input))
	history := append([]llm.Message(nil), r.history...)
	r.mu.Unlock()

	defer func() {
		cancel()
		r.mu.Lock()
		if r.activeTurn == turnID {
			r.agentCancel = nil
			r.activeTurn = 0
			r.turnDone = nil
			close(turnDone)
		}
		r.mu.Unlock()
	}()

	var emitMu sync.Mutex
	type activeToolCall struct {
		id      string
		started time.Time
	}
	activeToolCalls := make(map[string]activeToolCall)
	nextToolCall := 0
	emitEvent := func(event chatpkg.Event) {
		if emit == nil {
			return
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		emit(event)
	}
	var observedUsage llm.Usage
	newHistory, runErr := runner.Run(agent.WithSession(turnCtx, current.ID), history, agent.Hooks{
		OnText: func(delta string) {
			emitEvent(chatpkg.Event{Kind: chatpkg.EventTextDelta, Text: delta})
		},
		OnToolStart: func(use llm.ToolUse) {
			emitMu.Lock()
			nextToolCall++
			call := activeToolCall{
				id:      fmt.Sprintf("tool-%d", nextToolCall),
				started: time.Now(),
			}
			activeToolCalls[use.ID] = call
			emitMu.Unlock()
			emitEvent(chatpkg.Event{
				Kind:       chatpkg.EventToolStarted,
				ToolName:   use.Name,
				ToolCallID: call.id,
			})
		},
		OnToolDone: func(use llm.ToolUse, result llm.ToolResult) {
			emitMu.Lock()
			call, ok := activeToolCalls[use.ID]
			if ok {
				delete(activeToolCalls, use.ID)
			} else {
				nextToolCall++
				call = activeToolCall{id: fmt.Sprintf("tool-%d", nextToolCall), started: time.Now()}
			}
			duration := time.Since(call.started).Milliseconds()
			emitMu.Unlock()
			emitEvent(chatpkg.Event{
				Kind:       chatpkg.EventToolFinished,
				ToolName:   use.Name,
				ToolCallID: call.id,
				IsError:    result.IsError,
				ByteCount:  len(result.Content),
				DurationMS: duration,
			})
		},
		OnUsage: func(value llm.Usage) {
			emitMu.Lock()
			observedUsage = value
			emitMu.Unlock()
			// Emit mid-turn usage so the chat UI can update live token counts
			// before EventTurnDone. Empty text does not append to the transcript.
			emitEvent(chatpkg.Event{Kind: chatpkg.EventTextDelta, Usage: value})
		},
	})

	persistCtx := context.WithoutCancel(turnCtx)
	r.mu.Lock()
	r.history = newHistory
	var persistErr error
	for r.persisted < len(r.history) {
		if err := r.sessions.AppendTurn(persistCtx, current.ID, r.history[r.persisted]); err != nil {
			persistErr = err
			break
		}
		r.persisted++
	}
	state := r.stateLocked(r.capabilities)
	r.mu.Unlock()
	if persistErr != nil {
		emitEvent(chatpkg.Event{Kind: chatpkg.EventNotice, Text: fmt.Sprintf("persist turn: %v", persistErr), IsError: true})
	}

	emitMu.Lock()
	usage := observedUsage
	emitMu.Unlock()
	emitEvent(chatpkg.Event{Kind: chatpkg.EventTurnDone, IsError: runErr != nil, Usage: usage, State: &state})
	return runErr
}

func truncateChatTitle(input string) string {
	runes := []rune(input)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return input
}

func (r *chatRuntime) Cancel() {
	r.mu.Lock()
	cancel := r.agentCancel
	commandCancel := r.commandCancel
	r.pendingNewSessionID = ""
	r.mu.Unlock()
	if commandCancel != nil {
		commandCancel()
	}
	if cancel != nil {
		cancel()
	}
}

// The following lowercase methods keep existing focused command tests and
// callers source-compatible while delegating all behavior to chatRuntime.

func newChat(ctx context.Context, cfg config.Config, st *store.Store, continueLast bool, profileName string) (*chatRuntime, func(), error) {
	runtime, err := newChatRuntime(ctx, cfg, st)
	if err != nil {
		return nil, func() {}, err
	}
	if _, err := runtime.Open(ctx, chatpkg.OpenOptions{Continue: continueLast, Profile: profileName}); err != nil {
		_ = runtime.Close(ctx)
		return nil, func() {}, err
	}
	cleanup := func() { _ = runtime.Close(context.Background()) }
	return runtime, cleanup, nil
}

func (r *chatRuntime) reflectSession(ctx context.Context) error {
	r.mu.Lock()
	if r.persisted < 2 || r.agent == nil || r.current == nil {
		r.mu.Unlock()
		return nil
	}
	currentID := r.current.ID
	history := append([]llm.Message(nil), r.history...)
	provider := r.agent.Provider
	model := r.agent.Model
	if r.agent.UtilityModel != "" {
		model = r.agent.UtilityModel
	}
	r.mu.Unlock()

	summary, err := session.Reflect(ctx, provider, history, session.ReflectOptions{Model: model})
	if err != nil {
		return fmt.Errorf("session %s saved; summary skipped: %w", currentID, err)
	}
	if summary == "" {
		return nil
	}
	if err := r.sessions.SetSummary(ctx, currentID, summary); err != nil {
		return fmt.Errorf("save session %s summary: %w", currentID, err)
	}
	r.mu.Lock()
	if r.current != nil && r.current.ID == currentID {
		r.current.Summary = summary
	}
	r.mu.Unlock()
	return nil
}

func (r *chatRuntime) Close(ctx context.Context) error {
	err := r.close(ctx)
	return redactChatError(err, r.runtimeRedactor())
}

func (r *chatRuntime) close(ctx context.Context) error {
	cleanupCtx, cancel := detachedRuntimeCloseContext(ctx, r.runtimeCloseTimeout())
	defer cancel()

	r.mu.Lock()
	r.closed = true
	r.pendingNewSessionID = ""
	commandCancel := r.commandCancel
	commandDone := r.commandDone
	turnCancel := r.agentCancel
	turnDone := r.turnDone
	r.mu.Unlock()
	if commandCancel != nil {
		commandCancel()
	}
	if turnCancel != nil {
		turnCancel()
	}
	if commandDone != nil {
		select {
		case <-commandDone:
		case <-cleanupCtx.Done():
			return fmt.Errorf("wait for active chat command: %w", cleanupCtx.Err())
		}
	}
	if turnDone != nil {
		select {
		case <-turnDone:
		case <-cleanupCtx.Done():
			return fmt.Errorf("wait for active chat turn: %w", cleanupCtx.Err())
		}
	}
	return r.finishClose(cleanupCtx)
}

func (r *chatRuntime) runtimeCloseTimeout() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeTimeout > 0 {
		return r.closeTimeout
	}
	return chatRuntimeCloseTimeout
}

func (r *chatRuntime) finishClose(ctx context.Context) error {
	r.mu.Lock()
	if r.cleanupComplete {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	if r.cleanupStarted {
		done := r.cleanupDone
		r.mu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			err := r.closeErr
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return fmt.Errorf("wait for chat cleanup: %w", ctx.Err())
		}
	}
	r.cleanupStarted = true
	r.cleanupDone = make(chan struct{})
	done := r.cleanupDone
	r.mu.Unlock()

	err := r.cleanup(ctx)
	r.mu.Lock()
	r.closeErr = err
	r.cleanupComplete = cleanupCompleted(err)
	r.cleanupStarted = false
	close(done)
	r.mu.Unlock()
	return err
}

type redactedChatRuntimeError struct {
	cause   error
	message string
}

func (e *redactedChatRuntimeError) Error() string       { return e.message }
func (e *redactedChatRuntimeError) Unwrap() error       { return e.cause }
func (e *redactedChatRuntimeError) SafeMessage() string { return e.message }

func (r *chatRuntime) runtimeRedactor() func(string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent != nil && r.agent.Redact != nil {
		return r.agent.Redact
	}
	return func(value string) string { return value }
}

func redactChatError(err error, redact func(string) string) error {
	if err == nil {
		return nil
	}
	message := redact(err.Error())
	if message == err.Error() {
		return err
	}
	return &redactedChatRuntimeError{cause: err, message: message}
}

func redactChatEvent(event chatpkg.Event, redact func(string) string) chatpkg.Event {
	event.Text = redact(event.Text)
	event.ToolName = redact(event.ToolName)
	if event.State != nil {
		state := redactChatState(*event.State, redact)
		event.State = &state
	}
	return event
}

func redactChatResult(result chatpkg.Result, redact func(string) string) chatpkg.Result {
	result.Title = redact(result.Title)
	result.Text = redact(result.Text)
	for i := range result.Models {
		result.Models[i].Alias = redact(result.Models[i].Alias)
		result.Models[i].Provider = redact(result.Models[i].Provider)
		result.Models[i].Upstream = redact(result.Models[i].Upstream)
	}
	for i := range result.Sessions {
		result.Sessions[i].Title = redact(result.Sessions[i].Title)
		result.Sessions[i].Summary = redact(result.Sessions[i].Summary)
		result.Sessions[i].ModelAlias = redact(result.Sessions[i].ModelAlias)
	}
	for i := range result.Workset {
		result.Workset[i].Text = redact(result.Workset[i].Text)
	}
	if result.State != nil {
		state := redactChatState(*result.State, redact)
		result.State = &state
	}
	return result
}

func redactChatState(state chatpkg.State, redact func(string) string) chatpkg.State {
	state.Title = redact(state.Title)
	state.ModelAlias = redact(state.ModelAlias)
	state.ModelError = redact(state.ModelError)
	state.ProviderLabel = redact(state.ProviderLabel)
	state.Profile = redact(state.Profile)
	state.Workspace = redact(state.Workspace)
	for i := range state.History {
		state.History[i] = redactChatMessage(state.History[i], redact)
	}
	for i := range state.Models {
		state.Models[i].Alias = redact(state.Models[i].Alias)
		state.Models[i].Provider = redact(state.Models[i].Provider)
		state.Models[i].Upstream = redact(state.Models[i].Upstream)
	}
	state.Skills = append([]chatpkg.SkillRef(nil), state.Skills...)
	for i := range state.Skills {
		state.Skills[i].Name = redact(state.Skills[i].Name)
		state.Skills[i].Description = redact(state.Skills[i].Description)
	}
	return state
}

func redactChatMessage(message llm.Message, redact func(string) string) llm.Message {
	message.Blocks = append([]llm.Block(nil), message.Blocks...)
	for i := range message.Blocks {
		block := &message.Blocks[i]
		block.Text = redact(block.Text)
		block.Signature = redact(block.Signature)
		block.Data = redact(block.Data)
		if block.ToolUse != nil {
			toolUse := *block.ToolUse
			block.ToolUse = &toolUse
			block.ToolUse.ID = redact(block.ToolUse.ID)
			block.ToolUse.Name = redact(block.ToolUse.Name)
			block.ToolUse.Input = redactChatJSON(block.ToolUse.Input, redact)
		}
		if block.ToolResult != nil {
			toolResult := *block.ToolResult
			block.ToolResult = &toolResult
			block.ToolResult.ToolUseID = redact(block.ToolResult.ToolUseID)
			block.ToolResult.Content = redact(block.ToolResult.Content)
		}
	}
	return message
}

func redactChatJSON(raw json.RawMessage, redact func(string) string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	var walk func(any) any
	walk = func(current any) any {
		switch typed := current.(type) {
		case string:
			return redact(typed)
		case []any:
			for i := range typed {
				typed[i] = walk(typed[i])
			}
		case map[string]any:
			for key, item := range typed {
				typed[key] = walk(item)
			}
		}
		return current
	}
	encoded, err := json.Marshal(walk(value))
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return encoded
}
