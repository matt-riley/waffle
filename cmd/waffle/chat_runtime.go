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
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	redactpkg "github.com/matt-riley/waffle/internal/redact"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/spill"
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
	mu            sync.Mutex
	commandMu     sync.Mutex
	agent         *agent.Agent
	agentCancel   context.CancelFunc
	commandCancel context.CancelFunc
	commandDone   chan struct{}
	sessions      *session.Store
	current       *session.Session
	history       []llm.Message
	persisted     int
	// temporary marks a conversation whose content must never enter durable
	// session storage, reflection, learning, or memory (#475).
	temporary bool
	// durableSpill is the agent's spill store, kept aside while a temporary
	// conversation disables durable recall so /new can restore it.
	durableSpill        *spill.Store
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
	// api carries the per-face API tool wiring (#254); zero disables them.
	api      apiBrokerWiring
	wsClient io.Closer

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
	return chatpkg.RedactState(state, redact), redactChatError(err, redact)
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
	built, cleanup, err := buildAgentWithProfileContext(resourceCtx, r.cfg, ws, skills, r.sessions, config.GroupMain, profileName, r.api)
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
	if options.Temporary {
		// A temporary conversation gets an in-memory scoped identity only:
		// no row is ever written, so listing, FTS, summaries, reflection, and
		// memory can never find it (#475).
		id, idErr := id.New("temp-session-")
		if idErr != nil {
			return chatpkg.State{}, fmt.Errorf("temporary session id: %w", idErr)
		}
		now := time.Now()
		current = &session.Session{ID: id, Title: "Temporary conversation", CreatedAt: now, UpdatedAt: now}
	} else {
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
	}

	var history []llm.Message
	if !options.Temporary && (options.Continue || strings.TrimSpace(options.SessionID) != "") {
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
	if !r.sessionOwners.acquireWait(r, current.ID, sessionOwnerDrainWait) {
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
	r.temporary = options.Temporary
	r.applyPersistPolicyLocked()
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
			Default:     alias == r.cfg.Agent.DefaultModel,
			Utility:     alias == r.cfg.Agent.UtilityModel,
			Description: strings.TrimSpace(r.cfg.Models[alias].Description),
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
		state.Lineage = chatpkg.BranchLineage{ForkedFrom: r.current.ForkedFrom, ForkedAtSeq: r.current.ForkedAtSeq}
	}
	state.Temporary = r.temporary
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
	return r.turnWithMedia(ctx, input, nil, emit)
}

// TurnMedia runs one turn with media blocks attached to the user message.
// Media is validated with llm.ValidateBlocks before the turn starts so
// unsupported or oversized payloads never enter history (#473).
func (r *chatRuntime) TurnMedia(ctx context.Context, input string, media []llm.Block, emit func(chatpkg.Event)) error {
	return r.TurnWithModes(ctx, input, chatpkg.TurnModeOptions{Media: media}, emit)
}

// taskModeGuidance is the trusted, server-owned guidance a validated task
// mode adds to the user message. It is fixed text — never operator-supplied —
// so it can be stripped safely from transcript rendering by exact match.
var taskModeGuidance = map[string]string{
	"quick": "Answer concisely and directly. Avoid unnecessary detail.",
	"deep":  "Work through the problem carefully before giving a final answer.",
	"draft": "Draft prose suitable for editing before use.",
}

// TurnWithModes runs one turn with validated per-turn task/reasoning modes.
// Modes only add trusted guidance or narrow limits; they never widen posture
// (#481). The mode metadata is persisted with the turn.
func (r *chatRuntime) TurnWithModes(ctx context.Context, input string, options chatpkg.TurnModeOptions, emit func(chatpkg.Event)) error {
	redact := r.runtimeRedactor()
	redactedEmit := func(event chatpkg.Event) {
		if emit != nil {
			emit(chatpkg.RedactEvent(event, redact))
		}
	}
	metadata := map[string]string{}
	guidance := []llm.Block(nil)
	if options.TaskMode != "" {
		metadata["task_mode"] = options.TaskMode
		if text := taskModeGuidance[options.TaskMode]; text != "" {
			guidance = append(guidance, llm.Block{Type: llm.BlockText, Text: text})
		}
	}
	if options.ReasoningEffort != "" {
		metadata["reasoning_effort"] = options.ReasoningEffort
	}
	media := append(guidance, options.Media...)
	return redactChatError(r.turn(ctx, input, media, metadata, redactedEmit), redact)
}

func (r *chatRuntime) turnWithMedia(ctx context.Context, input string, media []llm.Block, emit func(chatpkg.Event)) error {
	if err := llm.ValidateBlocks(media); err != nil {
		return fmt.Errorf("invalid media: %w", err)
	}
	redact := r.runtimeRedactor()
	redactedEmit := func(event chatpkg.Event) {
		if emit != nil {
			emit(chatpkg.RedactEvent(event, redact))
		}
	}
	return redactChatError(r.turn(ctx, input, media, nil, redactedEmit), redact)
}

func (r *chatRuntime) turn(ctx context.Context, input string, media []llm.Block, metadata map[string]string, emit func(chatpkg.Event)) error {
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
	if len(r.history) == 0 && current.Title == "" && r.persistable() {
		title := truncateChatTitle(input)
		if err := r.sessions.SetTitle(turnCtx, current.ID, title); err == nil {
			current.Title = title
		}
	}
	turnStart := len(r.history)
	message := llm.UserBlocks(input, media)
	message.Metadata = metadata
	r.history = append(r.history, message)
	history := append([]llm.Message(nil), r.history...)
	// persistedStart marks where this turn's new messages begin once the run
	// appends them, so artifact collection stays scoped to this exchange.
	persistedStart := r.persisted
	if !r.persistable() {
		// Temporary conversations never advance persisted; keep artifact
		// collection scoped to this exchange via the in-memory turn start.
		persistedStart = turnStart
	}
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
	if r.persistable() {
		for r.persisted < len(r.history) {
			if err := r.sessions.AppendTurn(persistCtx, current.ID, r.history[r.persisted]); err != nil {
				persistErr = err
				break
			}
			r.persisted++
		}
	}
	state := r.stateLocked(r.capabilities)
	// Collect artifacts declared only by this exchange's appended turns so a
	// later turn never re-emits artifacts from earlier ones (#480 review).
	artifacts := collectArtifacts(r.history[persistedStart:])
	citations := collectCitations(r.history)

	r.mu.Unlock()
	if persistErr != nil {
		emitEvent(chatpkg.Event{Kind: chatpkg.EventNotice, Text: fmt.Sprintf("persist turn: %v", persistErr), IsError: true})
	}
	if len(artifacts) > 0 {
		emitEvent(chatpkg.Event{Kind: chatpkg.EventArtifact, Artifacts: artifacts})
	}
	if len(citations) > 0 {
		emitEvent(chatpkg.Event{Kind: chatpkg.EventSources, Sources: citations})
	}

	emitMu.Lock()
	usage := observedUsage
	emitMu.Unlock()
	emitEvent(chatpkg.Event{Kind: chatpkg.EventTurnDone, IsError: runErr != nil, Usage: usage, State: &state})
	return runErr
}

// collectArtifacts projects artifacts declared by the completed exchange
// into the client-visible shape, in transcript order (#480). Artifacts are
// declared as BlockArtifact blocks inside tool results (write_artifact); the
// streaming client renders their cards from this event. An empty result
// means the exchange declared no artifacts.
func collectArtifacts(history []llm.Message) []chatpkg.Artifact {
	var out []chatpkg.Artifact
	for _, msg := range history {
		for _, block := range msg.Blocks {
			if block.Type != llm.BlockToolResult || block.ToolResult == nil {
				continue
			}
			for _, inner := range block.ToolResult.Blocks {
				if inner.Type != llm.BlockArtifact || inner.Artifact == nil {
					continue
				}
				ref := inner.Artifact
				out = append(out, chatpkg.Artifact{
					ID: ref.ID, Name: ref.Name, MediaType: ref.MediaType,
					Size: ref.Size, Digest: ref.Digest, State: ref.State,
				})
			}
		}
	}
	return out
}

// collectCitations projects the final assistant exchange's provider-neutral
// citations into the client-visible Source shape, in stable response order
// (#479). Citations attached to earlier (tool-loop) assistant turns are not
// re-emitted; the streaming client attaches the drawer to the completed
// exchange. An empty result means the provider attested no sources.
func collectCitations(history []llm.Message) []chatpkg.Source {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg.Role != llm.RoleAssistant {
			continue
		}
		var sources []chatpkg.Source
		next := 1
		for _, block := range msg.Blocks {
			if block.Type != llm.BlockText {
				continue
			}
			for _, citation := range block.Citations {
				id := strings.TrimSpace(citation.ID)
				if id == "" {
					id = fmt.Sprintf("s%d", next)
				}
				sources = append(sources, chatpkg.Source{
					ID:         id,
					Label:      citation.Label,
					Kind:       string(citation.Kind),
					URL:        citation.URL,
					Resource:   citation.Resource,
					Snippet:    citation.Snippet,
					Provenance: citation.Provenance,
				})
				next++
			}
		}
		return sources
	}
	return nil
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

// persistable reports whether this conversation may write durable session,
// reflection, learning, or memory state. The caller must hold r.mu.
func (r *chatRuntime) persistable() bool { return !r.temporary }

func (r *chatRuntime) blocksCurrentSessionWrite(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil && r.current.ID == id && !r.persistable()
}

func (r *chatRuntime) applyPersistPolicyLocked() {
	if r.agent == nil {
		return
	}
	if r.persistable() {
		if r.agent.Spill == nil && r.durableSpill != nil {
			r.agent.Spill = r.durableSpill
		}
		return
	}
	if r.agent.Spill != nil {
		r.durableSpill = r.agent.Spill
		r.agent.Spill = nil
	}
	if _, ok := r.agent.Tools.(persistableToolbox); !ok {
		r.agent.Tools = persistableToolbox{runtime: r, inner: r.agent.Tools}
	}
}

func (r *chatRuntime) reflectSession(ctx context.Context) error {
	r.mu.Lock()
	if !r.persistable() || r.persisted < 2 || r.agent == nil || r.current == nil {
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

func (r *chatRuntime) runtimeRedactor() func(string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent != nil && r.agent.Redact != nil {
		return r.agent.Redact
	}
	return func(value string) string { return value }
}

func redactChatError(err error, redact func(string) string) error {
	return redactpkg.RedactError(err, redact)
}

// persistableToolbox refuses durable memory, learning, and working-set writes
// while the conversation is temporary. The caller must apply it from
// chatRuntime so persistable() stays the single gate (#475).
type persistableToolbox struct {
	runtime *chatRuntime
	inner   tool.Toolbox
}

func durableWriteTool(name string) bool {
	switch name {
	case "remember", "memory_update", "distill_skill", "workspace_update":
		return true
	default:
		return false
	}
}

func (p persistableToolbox) denyDurable(name string) error {
	if !durableWriteTool(name) {
		return nil
	}
	p.runtime.mu.Lock()
	ok := p.runtime.persistable()
	p.runtime.mu.Unlock()
	if ok {
		return nil
	}
	return fmt.Errorf("%s is unavailable in a temporary conversation", name)
}

func (p persistableToolbox) Defs() []llm.Tool {
	if p.inner == nil {
		return nil
	}
	return p.inner.Defs()
}

func (p persistableToolbox) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	if err := p.denyDurable(name); err != nil {
		return "", err
	}
	if p.inner == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return p.inner.Run(ctx, name, input)
}

func (p persistableToolbox) RunWithID(ctx context.Context, id, name string, input json.RawMessage) (string, error) {
	if err := p.denyDurable(name); err != nil {
		return "", err
	}
	if p.inner == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if caller, ok := p.inner.(tool.CallerToolbox); ok {
		return caller.RunWithID(ctx, id, name, input)
	}
	return p.inner.Run(ctx, name, input)
}

func (p persistableToolbox) RunWithBlocks(ctx context.Context, name string, input json.RawMessage) (string, []llm.Block, error) {
	if err := p.denyDurable(name); err != nil {
		return "", nil, err
	}
	if p.inner == nil {
		return "", nil, fmt.Errorf("unknown tool %q", name)
	}
	if blocks, ok := p.inner.(tool.BlockToolbox); ok {
		return blocks.RunWithBlocks(ctx, name, input)
	}
	out, err := p.inner.Run(ctx, name, input)
	return out, nil, err
}
