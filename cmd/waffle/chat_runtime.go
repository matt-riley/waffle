package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
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

type chatRuntimeCleanup struct {
	mu     sync.Mutex
	client io.Closer
	agent  agentCleanupContext
}

func newChatRuntimeCleanup(client io.Closer, agentCleanup agentCleanupContext) *chatRuntimeCleanup {
	if client == nil && agentCleanup == nil {
		return nil
	}
	return &chatRuntimeCleanup{client: client, agent: agentCleanup}
}

func (c *chatRuntimeCleanup) close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var cleanupErr error
	if c.client != nil {
		if err := closeRuntimeResource(ctx, c.client); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			c.client = nil
		}
	}
	if c.agent != nil {
		if err := c.agent(ctx); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			c.agent = nil
		}
	}
	return cleanupErr
}

func (c *chatRuntimeCleanup) complete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client == nil && c.agent == nil
}

type contextCloser interface {
	CloseContext(context.Context) error
}

func closeRuntimeResource(ctx context.Context, closer io.Closer) error {
	if contextual, ok := closer.(contextCloser); ok {
		return contextual.CloseContext(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return closer.Close()
}

// newChatRuntime records dependencies without constructing provider, sandbox,
// or MCP resources. Open performs that work after validating client options.
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

func (r *chatRuntime) Command(ctx context.Context, command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	redact := r.runtimeRedactor()
	redactedEmit := func(event chatpkg.Event) {
		if emit != nil {
			emit(redactChatEvent(event, redact))
		}
	}
	result, err := r.command(ctx, command, redactedEmit)
	return redactChatResult(result, redact), redactChatError(err, redact)
}

func (r *chatRuntime) command(ctx context.Context, command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	if command.Name == chatpkg.CommandExit {
		return r.runCommand(ctx, command, emit)
	}
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	commandCtx, commandCancel := context.WithCancel(ctx)
	commandDone := make(chan struct{})
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		commandCancel()
		return chatpkg.Result{}, errors.New("chat runtime is closed")
	}
	r.commandCancel = commandCancel
	r.commandDone = commandDone
	r.mu.Unlock()
	defer r.finishCommand(commandCancel, commandDone)
	if err := commandCtx.Err(); err != nil {
		return chatpkg.Result{}, err
	}
	if invalidatesNewConfirmation(command) {
		r.mu.Lock()
		r.pendingNewSessionID = ""
		r.mu.Unlock()
	}
	switch command.Name {
	case chatpkg.CommandModel:
		return r.commandModel(commandCtx, command.Args)
	case chatpkg.CommandHelp, chatpkg.CommandModels, chatpkg.CommandNew,
		chatpkg.CommandSessions, chatpkg.CommandResume, chatpkg.CommandStatus,
		chatpkg.CommandUsage, chatpkg.CommandPermissions, chatpkg.CommandSkill, chatpkg.CommandSkills,
		chatpkg.CommandRepo, chatpkg.CommandWorkset, chatpkg.CommandExit:
		return r.runCommand(commandCtx, command, emit)
	default:
		return chatpkg.Result{}, fmt.Errorf("unknown chat command %q", command.Name)
	}
}

func invalidatesNewConfirmation(command chatpkg.ParsedCommand) bool {
	args := strings.TrimSpace(command.Args)
	switch command.Name {
	case chatpkg.CommandModel, chatpkg.CommandResume, chatpkg.CommandRepo:
		return args != ""
	case chatpkg.CommandSkill:
		return true
	case chatpkg.CommandSkills:
		return args != ""
	case chatpkg.CommandWorkset:
		verb, _, _ := strings.Cut(args, " ")
		return verb == "replace" || verb == "drop" || verb == "clear"
	default:
		return false
	}
}

func (r *chatRuntime) runCommand(ctx context.Context, command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	switch command.Name {
	case chatpkg.CommandHelp:
		return chatpkg.Result{Title: "Chat commands", Commands: chatpkg.Commands()}, nil
	case chatpkg.CommandModels:
		r.mu.Lock()
		defer r.mu.Unlock()
		return chatpkg.Result{Title: "Configured models", Models: r.modelsLocked()}, nil
	case chatpkg.CommandNew:
		return r.commandNew(ctx, command.Args, emit)
	case chatpkg.CommandSessions:
		return r.commandSessions(ctx, "Recent sessions")
	case chatpkg.CommandResume:
		return r.commandResume(ctx, command.Args, emit)
	case chatpkg.CommandStatus:
		r.mu.Lock()
		defer r.mu.Unlock()
		state := r.stateLocked(r.capabilities)
		return chatpkg.Result{Title: "Chat status", State: &state}, nil
	case chatpkg.CommandUsage:
		return r.commandUsage(ctx)
	case chatpkg.CommandPermissions:
		return r.commandPermissions(), nil
	case chatpkg.CommandSkill:
		return r.commandSkill(ctx, command.Args, emit)
	case chatpkg.CommandSkills:
		return r.commandSkills(ctx, command.Args, emit)
	case chatpkg.CommandRepo:
		return r.commandRepo(ctx, command.Args, emit)
	case chatpkg.CommandWorkset:
		return r.commandWorkset(ctx, command.Args)
	case chatpkg.CommandExit:
		err := r.Close(ctx)
		result := chatpkg.Result{ShouldClose: true}
		if err != nil {
			result.Text = "warning: " + err.Error()
			if emit != nil {
				emit(chatpkg.Event{Kind: chatpkg.EventNotice, Text: result.Text, IsError: true})
			}
		}
		return result, nil
	default:
		return chatpkg.Result{}, fmt.Errorf("unknown chat command %q", command.Name)
	}
}

func (r *chatRuntime) commandNew(ctx context.Context, args string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.current == nil {
			return chatpkg.Result{}, errors.New("chat runtime is not open")
		}
		if !r.blockTurns {
			r.pendingNewSessionID = r.current.ID
		}
		return chatpkg.Result{Confirm: true, Text: "Start a new session?"}, nil
	}
	if args != chatNewConfirmArg {
		return chatpkg.Result{}, errors.New("usage: /new")
	}
	r.mu.Lock()
	pending := r.current != nil && r.pendingNewSessionID != "" && r.pendingNewSessionID == r.current.ID && !r.blockTurns
	turnCancel := r.agentCancel
	turnDone := r.turnDone
	if pending {
		r.pendingNewSessionID = ""
		r.blockTurns = true
	}
	r.mu.Unlock()
	if !pending {
		return chatpkg.Result{}, errors.New("no pending /new confirmation")
	}
	defer r.endExclusiveChange()
	if turnCancel != nil {
		turnCancel()
		if turnDone != nil {
			select {
			case <-turnDone:
			case <-ctx.Done():
				return chatpkg.Result{}, fmt.Errorf("wait for active chat turn: %w", ctx.Err())
			}
		}
	}
	reflectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	reflectErr := r.reflectSession(reflectCtx)
	cancel()
	dropped, err := r.resetSession(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "turn is active") {
			return chatpkg.Result{Confirm: true, Text: "A turn is active; confirm before starting a new session."}, nil
		}
		return chatpkg.Result{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.stateLocked(r.capabilities)
	text := fmt.Sprintf("new session %s", r.current.ID)
	if dropped > 0 {
		text += fmt.Sprintf("; dropped %d unpinned model assumptions", dropped)
	}
	if reflectErr != nil {
		warning := "warning: " + reflectErr.Error()
		text = warning + "\n" + text
		if emit != nil {
			emit(chatpkg.Event{Kind: chatpkg.EventNotice, Text: warning, IsError: true})
		}
	}
	return chatpkg.Result{Text: text, State: &state}, nil
}

func (r *chatRuntime) endExclusiveChange() {
	r.mu.Lock()
	r.blockTurns = false
	r.mu.Unlock()
}

// resetSession implements the runtime's session/workset ownership transition.
// It is also retained as a narrow compatibility method for focused tests.
func (r *chatRuntime) resetSession(ctx context.Context) (int, error) {
	r.mu.Lock()
	if r.current == nil {
		r.mu.Unlock()
		return 0, errors.New("chat runtime is not open")
	}
	if r.agentCancel != nil {
		r.mu.Unlock()
		return 0, errors.New("a chat turn is active")
	}
	previous := r.current
	activeAgent := r.agent
	baseSystem := r.baseSystem
	activeSkills := append([]skill.Skill(nil), r.skills...)
	profileName := r.profileName
	r.mu.Unlock()

	profile, _ := r.cfg.Profile(profileName)
	model, err := resolveRuntimeProfileModel(r.cfg, profile)
	if err != nil {
		return 0, err
	}
	dropped := 0
	if r.st != nil {
		ws := &workset.Store{DB: r.st.DB}
		if n, err := ws.DropUnpinnedModelAssumptions(ctx, previous.ID); err == nil {
			dropped = n
		}
	}
	current, err := r.sessions.Create(ctx, "")
	if err != nil {
		return 0, err
	}
	nextSkills, nextSystem, err := buildAttachedSkillContext(baseSystem, activeSkills, nil)
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.current != previous || r.agent != activeAgent || r.agentCancel != nil || r.baseSystem != baseSystem {
		return 0, errors.New("chat runtime changed while starting a new session")
	}
	if !r.sessionOwners.transfer(r, previous.ID, current.ID) {
		return 0, sessionAlreadyActiveError{sessionID: current.ID}
	}
	r.current = current
	r.ownedSessionID = current.ID
	r.history = nil
	r.persisted = 0
	r.modelError = ""
	if r.agent != nil {
		r.agent.Model = model
		r.agent.System = nextSystem
	}
	r.attachedSkills = nextSkills
	return dropped, nil
}

func (r *chatRuntime) commandSessions(ctx context.Context, title string) (chatpkg.Result, error) {
	sessions, err := r.sessions.List(ctx, 50)
	if err != nil {
		return chatpkg.Result{}, err
	}
	return chatpkg.Result{Title: title, Sessions: chatSessions(sessions)}, nil
}

func chatSessions(sessions []session.Session) []chatpkg.Session {
	out := make([]chatpkg.Session, len(sessions))
	for i, value := range sessions {
		out[i] = chatpkg.Session{
			ID: value.ID, Title: value.Title, Summary: value.Summary,
			ModelAlias: value.ModelAlias, UpdatedAt: value.UpdatedAt,
		}
	}
	return out
}

func (r *chatRuntime) commandResume(ctx context.Context, id string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return r.commandSessions(ctx, "Resume a session")
	}
	r.mu.Lock()
	if r.current == nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("chat runtime is not open")
	}
	if r.agentCancel != nil {
		r.mu.Unlock()
		return chatpkg.Result{Confirm: true, Text: "A turn is active; confirm before resuming another session."}, nil
	}
	if r.blockTurns {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("a chat command is changing runtime state")
	}
	r.blockTurns = true
	activeAgent := r.agent
	baseSystem := r.baseSystem
	activeSkills := append([]skill.Skill(nil), r.skills...)
	r.mu.Unlock()
	defer r.endExclusiveChange()

	target, err := r.sessions.Get(ctx, id)
	if err != nil {
		return chatpkg.Result{}, err
	}
	history, err := r.sessions.Turns(ctx, target.ID)
	if err != nil {
		return chatpkg.Result{}, fmt.Errorf("load session %s: %w", target.ID, err)
	}
	history = session.Repair(history)

	profile, _ := r.cfg.Profile(r.profileName)
	model, err := resolveRuntimeProfileModel(r.cfg, profile)
	if err != nil {
		return chatpkg.Result{}, err
	}
	modelError := ""
	if target.ModelAlias != "" {
		if _, err := r.cfg.ResolveModel(target.ModelAlias); err != nil {
			modelError = err.Error()
		} else {
			model = target.ModelAlias
		}
	}
	attachedNames, err := (&skill.Attachments{DB: r.st.DB, Lifecycle: r.st.SkillLifecycleGuard()}).List(ctx, target.ID)
	if err != nil {
		return chatpkg.Result{}, err
	}
	attachedSkills, attachedSystem, err := buildAttachedSkillContext(baseSystem, activeSkills, attachedNames)
	if err != nil {
		return chatpkg.Result{}, err
	}
	reflectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	reflectErr := r.reflectSession(reflectCtx)
	cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent != activeAgent || r.baseSystem != baseSystem {
		return chatpkg.Result{}, errors.New("chat runtime changed while resuming session")
	}
	if !r.sessionOwners.transfer(r, r.current.ID, target.ID) {
		return chatpkg.Result{}, sessionAlreadyActiveError{sessionID: target.ID}
	}
	r.current = target
	r.ownedSessionID = target.ID
	r.history = history
	r.persisted = len(history)
	r.modelError = modelError
	r.agent.Model = model
	r.agent.System = attachedSystem
	r.attachedSkills = attachedSkills
	state := r.stateLocked(r.capabilities)
	text := "resumed session " + target.ID
	if reflectErr != nil {
		warning := "warning: " + reflectErr.Error()
		text = warning + "\n" + text
		if emit != nil {
			emit(chatpkg.Event{Kind: chatpkg.EventNotice, Text: warning, IsError: true})
		}
	}
	return chatpkg.Result{Text: text, State: &state}, nil
}

func (r *chatRuntime) commandUsage(ctx context.Context) (chatpkg.Result, error) {
	r.mu.Lock()
	if r.current == nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("chat runtime is not open")
	}
	sessionID := r.current.ID
	r.mu.Unlock()

	usageStore := usagepkg.New(r.st)
	current, err := usageStore.List(ctx, sessionID)
	if err != nil {
		return chatpkg.Result{}, err
	}
	aggregate, err := usageStore.List(ctx, "")
	if err != nil {
		return chatpkg.Result{}, err
	}
	rows := append(chatUsageRows(current), chatUsageRows(aggregate)...)
	currentTotals := totalUsageRows(current)
	aggregateTotals := totalUsageRows(aggregate)
	text := fmt.Sprintf(
		"Current session totals: requests=%d input=%d output=%d reserved=%d\nPersisted aggregate totals: requests=%d input=%d output=%d reserved=%d",
		currentTotals.Requests, currentTotals.InputTokens, currentTotals.OutputTokens, currentTotals.ReservedTokens,
		aggregateTotals.Requests, aggregateTotals.InputTokens, aggregateTotals.OutputTokens, aggregateTotals.ReservedTokens,
	)
	return chatpkg.Result{Title: "Usage", Text: text, Usage: rows}, nil
}

func chatUsageRows(rows []usagepkg.Row) []chatpkg.UsageRow {
	out := make([]chatpkg.UsageRow, len(rows))
	for i, row := range rows {
		out[i] = chatpkg.UsageRow{
			SessionID: row.SessionID, Period: row.Period, PeriodStart: row.PeriodStart,
			Requests: row.Requests, InputTokens: row.InputTokens,
			OutputTokens: row.OutputTokens, ReservedTokens: row.ReservedTokens,
		}
	}
	return out
}

func totalUsageRows(rows []usagepkg.Row) usagepkg.Row {
	var total usagepkg.Row
	for _, row := range rows {
		total.Requests += row.Requests
		total.InputTokens += row.InputTokens
		total.OutputTokens += row.OutputTokens
		total.ReservedTokens += row.ReservedTokens
	}
	return total
}

func (r *chatRuntime) commandPermissions() chatpkg.Result {
	policy := r.resolvedPolicy()
	return chatpkg.Result{
		Title: "Effective permissions",
		Permissions: &chatpkg.PermissionView{
			SandboxMode: policy.Mode,
			Allow:       append([]string(nil), policy.Allow...), Deny: append([]string(nil), policy.Deny...),
			DenyPrefixes: append([]string(nil), policy.DenyPrefixes...),
		},
	}
}

// resolvedPolicy is the runtime's effective permission view. It delegates to
// config.ApplyProfilePolicy so Desk's posture projection and the profile
// editor validate against the very policy the runtime enforces (#193).
func (r *chatRuntime) resolvedPolicy() config.ResolvedAgentPolicy {
	profile, _ := r.cfg.Profile(r.profileName)
	return config.ApplyProfilePolicy(r.cfg.AgentPolicy(config.GroupMain), profile)
}

func (r *chatRuntime) commandSkill(ctx context.Context, args string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	message, err := r.skillMessage(args)
	if err != nil {
		return chatpkg.Result{}, err
	}
	name, _, _ := strings.Cut(strings.TrimSpace(args), " ")
	if err := r.Turn(ctx, message, emit); err != nil {
		return chatpkg.Result{}, err
	}
	return chatpkg.Result{Text: "skill " + name + " completed"}, nil
}

func (r *chatRuntime) skillMessage(rest string) (string, error) {
	name, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if name == "" {
		return "", errors.New("usage: /skill <name> [args]")
	}
	s, ok := skill.Find(r.skills, name)
	if !ok {
		return "", fmt.Errorf("unknown skill %q (have: %s)", name, skillNames(r.skills))
	}
	body, err := s.Body()
	if err != nil {
		return "", err
	}
	message := fmt.Sprintf("The user invoked the skill %q. Follow its instructions:\n\n%s", s.Name, body)
	if strings.TrimSpace(args) != "" {
		message += "\n\nUser arguments: " + strings.TrimSpace(args)
	}
	return message, nil
}

func (r *chatRuntime) commandSkills(ctx context.Context, args string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.current == nil {
			return chatpkg.Result{}, errors.New("chat runtime is not open")
		}
		state := r.stateLocked(r.capabilities)
		return chatpkg.Result{Title: "Session skills", Text: formatSkillRefs(state.Skills), State: &state}, nil
	}
	if len(fields) != 2 || (fields[0] != "attach" && fields[0] != "detach") {
		return chatpkg.Result{}, errors.New("usage: /skills [attach <name>|detach <name>]")
	}
	return r.changeSessionSkill(ctx, fields[0], fields[1], emit)
}

func (r *chatRuntime) changeSessionSkill(ctx context.Context, action, name string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	name = strings.TrimSpace(name)
	r.mu.Lock()
	if r.current == nil || r.agent == nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("chat runtime is not open")
	}
	if r.agentCancel != nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("cannot change skills while a turn is active")
	}
	if r.blockTurns {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("a chat command is changing runtime state")
	}
	if action == "attach" {
		if _, ok := skill.Find(r.skills, name); !ok {
			r.mu.Unlock()
			return chatpkg.Result{}, fmt.Errorf("skill %q is not active or installed; activate or install it before attaching", name)
		}
	}
	r.blockTurns = true
	sessionID := r.current.ID
	activeAgent := r.agent
	baseSystem := r.baseSystem
	activeSkills := append([]skill.Skill(nil), r.skills...)
	skillWorkspace := r.skillWorkspace
	r.mu.Unlock()
	defer r.endExclusiveChange()

	attachments := &skill.Attachments{DB: r.st.DB, Workspace: skillWorkspace, Lifecycle: r.st.SkillLifecycleGuard()}
	currentNames, err := attachments.List(ctx, sessionID)
	if err != nil {
		return chatpkg.Result{}, err
	}
	wasAttached := containsString(currentNames, name)
	nextNames := append([]string(nil), currentNames...)
	switch action {
	case "attach":
		if !wasAttached {
			nextNames = append(nextNames, name)
			sort.Strings(nextNames)
		}
	case "detach":
		nextNames = removeString(nextNames, name)
	default:
		return chatpkg.Result{}, errors.New("usage: /skills [attach <name>|detach <name>]")
	}
	nextRefs, nextSystem, err := buildAttachedSkillContext(baseSystem, activeSkills, nextNames)
	if err != nil {
		return chatpkg.Result{}, err
	}
	if action == "attach" {
		err = attachments.Attach(ctx, sessionID, name)
	} else {
		err = attachments.Detach(ctx, sessionID, name)
	}
	if err != nil {
		return chatpkg.Result{}, err
	}

	r.mu.Lock()
	valid := r.current != nil && r.current.ID == sessionID && r.agent == activeAgent && r.baseSystem == baseSystem
	if valid {
		r.attachedSkills = nextRefs
		r.agent.System = nextSystem
		state := r.stateLocked(r.capabilities)
		r.mu.Unlock()
		if emit != nil {
			emit(chatpkg.Event{Kind: chatpkg.EventState, State: &state})
		}
		return chatpkg.Result{Text: action + "ed skill " + name, State: &state}, nil
	}
	r.mu.Unlock()

	var rollbackErr error
	if wasAttached {
		rollbackErr = attachments.Attach(ctx, sessionID, name)
	} else {
		rollbackErr = attachments.Detach(ctx, sessionID, name)
	}
	return chatpkg.Result{}, errors.Join(errors.New("chat session changed while updating skills"), rollbackErr)
}

func buildAttachedSkillContext(baseSystem string, active []skill.Skill, attachedNames []string) ([]chatpkg.SkillRef, string, error) {
	activeByName := make(map[string]skill.Skill, len(active))
	for _, candidate := range active {
		if _, exists := activeByName[candidate.Name]; !exists {
			activeByName[candidate.Name] = candidate
		}
	}
	attached := make(map[string]bool, len(attachedNames))
	for _, name := range attachedNames {
		attached[name] = true
	}

	refs := make([]chatpkg.SkillRef, 0, len(activeByName)+len(attached))
	for name, candidate := range activeByName {
		refs = append(refs, chatpkg.SkillRef{
			Name: name, Description: candidate.Description, Attached: attached[name],
		})
	}
	for name := range attached {
		if _, ok := activeByName[name]; !ok {
			refs = append(refs, chatpkg.SkillRef{Name: name, Attached: true, Missing: true})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	var block strings.Builder
	block.WriteString("\n<attached_skills>\n")
	injected := 0
	for _, ref := range refs {
		if !ref.Attached || ref.Missing {
			continue
		}
		body, err := activeByName[ref.Name].Body()
		if err != nil {
			return nil, "", fmt.Errorf("read attached skill %q: %w", ref.Name, err)
		}
		fmt.Fprintf(&block, "<attached_skill name=\"%s\">\n%s\n</attached_skill>\n", html.EscapeString(ref.Name), body)
		injected++
		if block.Len()+len("</attached_skills>") > maxAttachedSkillContextBytes {
			return nil, "", fmt.Errorf("attached skill context exceeds %d bytes; detach one or more skills", maxAttachedSkillContextBytes)
		}
	}
	if injected == 0 {
		return refs, baseSystem, nil
	}
	block.WriteString("</attached_skills>")
	if block.Len() > maxAttachedSkillContextBytes {
		return nil, "", fmt.Errorf("attached skill context exceeds %d bytes; detach one or more skills", maxAttachedSkillContextBytes)
	}
	return refs, baseSystem + block.String(), nil
}

func formatSkillRefs(refs []chatpkg.SkillRef) string {
	if len(refs) == 0 {
		return "no active or attached skills"
	}
	lines := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch {
		case ref.Missing:
			lines = append(lines, fmt.Sprintf("%s — attached but unavailable; restore or reactivate it, or run /skills detach %s", ref.Name, ref.Name))
		case ref.Attached:
			lines = append(lines, fmt.Sprintf("%s — %s (attached)", ref.Name, ref.Description))
		default:
			lines = append(lines, fmt.Sprintf("%s — %s", ref.Name, ref.Description))
		}
	}
	return strings.Join(lines, "\n")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func removeString(values []string, unwanted string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != unwanted {
			out = append(out, value)
		}
	}
	return out
}

func (r *chatRuntime) commandRepo(ctx context.Context, repoArg string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	repoArg = strings.TrimSpace(repoArg)
	if repoArg == "" {
		return chatpkg.Result{}, errors.New("usage: /repo <owner/repo>")
	}
	r.mu.Lock()
	if r.current == nil || r.agent == nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("chat runtime is not open")
	}
	if r.agentCancel != nil {
		r.mu.Unlock()
		return chatpkg.Result{Confirm: true, Text: "A turn is active; confirm before opening a repository workspace."}, nil
	}
	if r.blockTurns {
		r.mu.Unlock()
		return chatpkg.Result{Confirm: true, Text: "Another runtime change is active; confirm before opening a repository workspace."}, nil
	}
	r.blockTurns = true
	defer r.endExclusiveChange()
	resourceCtx := r.resourceCtx
	if resourceCtx == nil {
		resourceCtx = context.WithoutCancel(ctx)
	}
	repoOpener := r.repoOpener
	if repoOpener == nil && r.wsBroker == nil {
		b, url, err := startWorkspaceBroker(resourceCtx, r.cfg, r.st, io.Discard)
		if err != nil {
			r.mu.Unlock()
			return chatpkg.Result{}, err
		}
		r.wsBroker, r.wsURL = b, url
	}
	wsBroker, wsURL, chatProfile := r.wsBroker, r.wsURL, r.chatProfileName
	r.mu.Unlock()

	var install repoInstall
	if repoOpener != nil {
		var err error
		install, err = repoOpener(ctx, repoArg, chatProfile)
		if err != nil {
			return chatpkg.Result{}, err
		}
	} else {
		mgr := newWorkspaceManager(r.cfg, r.st, wsBroker)
		// Configure through the shared helper rather than setting BrokerURL
		// alone: under any egress but "full" the container is netlocked to
		// waffle-host and reaches everything else through the broker's egress
		// proxy. Without ProxyURL the clone has no route to the git host, so
		// setup fails and the workspace is torn down again immediately.
		configureServeWorkspaceManager(r.cfg, mgr, wsURL)
		ws, client, err := mgr.OpenWithProfile(ctx, repoArg, chatProfile)
		if err != nil {
			return chatpkg.Result{}, err
		}
		policy, err := mgr.LoadRepoPolicy(ctx, client)
		if err != nil {
			_ = client.Close()
			return chatpkg.Result{}, err
		}
		install = repoInstall{
			workspace: ws,
			policy:    policy,
			tools:     sandbox.NewQueueToolbox(client),
			client:    client,
		}
	}
	return r.installRepo(ctx, install, emit)
}

func (r *chatRuntime) buildCleanProfileAgent(ctx context.Context, profileName string) (*agent.Agent, agentCleanupContext, error) {
	if r.profileAgentBuilder != nil {
		built, cleanup, err := r.profileAgentBuilder(ctx, profileName)
		return built, func(cleanupCtx context.Context) error {
			if err := cleanupCtx.Err(); err != nil {
				return err
			}
			if cleanup != nil {
				cleanup()
			}
			return nil
		}, err
	}
	memWS, skills, err := loadWorkspaceWithStore(r.st)
	if err != nil {
		return nil, nil, err
	}
	return buildAgentWithProfileContext(ctx, r.cfg, memWS, skills, r.sessions, config.GroupMain, profileName)
}

func (r *chatRuntime) installRepo(ctx context.Context, install repoInstall, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	if install.workspace == nil || install.tools == nil || install.client == nil {
		return chatpkg.Result{}, errors.New("incomplete repository workspace install")
	}
	r.mu.Lock()
	profileName := r.chatProfileName
	resourceCtx := r.resourceCtx
	activeSkills := append([]skill.Skill(nil), r.skills...)
	r.mu.Unlock()
	if install.workspace.Profile != "" {
		profileName = install.workspace.Profile
	}
	if resourceCtx == nil {
		resourceCtx = context.WithoutCancel(ctx)
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = closeRuntimeResource(resourceCtx, install.client)
		}
	}()
	currentAgent, replacementCleanup, err := r.buildCleanProfileAgent(resourceCtx, profileName)
	if err != nil {
		if replacementCleanup != nil {
			_ = replacementCleanup(resourceCtx)
		}
		return chatpkg.Result{}, err
	}
	cleanupAdopted := false
	defer func() {
		if !cleanupAdopted && replacementCleanup != nil {
			_ = replacementCleanup(resourceCtx)
		}
	}()

	hostPolicy := r.cfg.AgentPolicy(config.GroupMain)
	if profileName != "" {
		if profile, ok := r.cfg.Profile(profileName); ok {
			if len(profile.Tools.Allow) > 0 {
				hostPolicy.Allow = profile.Tools.Allow
			}
			if len(profile.Tools.Deny) > 0 {
				hostPolicy.Deny = appendUniqueStrings(hostPolicy.Deny, profile.Tools.Deny...)
			}
		}
	}
	toolPolicy := tool.Policy{Allow: hostPolicy.Allow, Deny: hostPolicy.Deny, Profile: currentAgent.Profile}
	if toolPolicy.Profile == "" {
		toolPolicy.Profile = "main"
	}
	systemExtra := fmt.Sprintf("\n\nYou are working in a container workspace on the repository %s, cloned at /work/repo. Your shell and file tools execute inside that container. Git pushes authenticate automatically.", install.workspace.Repo)
	if install.policy != nil {
		toolPolicy = repopolicy.TightenTools(toolPolicy, install.policy.Tools)
		toolPolicy = applyCodeIntelCaps(toolPolicy, install.policy.CodeIntelCaps)
		if block := install.policy.PromptBlock(); block != "" {
			systemExtra += "\n\n" + block
		}
	}
	hostTools := tool.Restrict(currentAgent.Tools, toolPolicy)
	boxed := tool.Restrict(tool.Combine(install.tools, hostTools), toolPolicy)
	workspaceBaseSystem := currentAgent.System + systemExtra
	workspaceAgent := &agent.Agent{
		Provider: currentAgent.Provider, Tools: boxed, System: workspaceBaseSystem,
		Model: currentAgent.Model, UtilityModel: currentAgent.UtilityModel, Profile: currentAgent.Profile,
		MaxTokens: currentAgent.MaxTokens, MaxIterations: currentAgent.MaxIterations,
		Redact: currentAgent.Redact, Spill: currentAgent.Spill, Usage: currentAgent.Usage,
		Limits: currentAgent.Limits, Log: currentAgent.Log,
	}

	if reflectErr := r.reflectSession(ctx); reflectErr != nil && emit != nil {
		emit(chatpkg.Event{Kind: chatpkg.EventNotice, Text: "warning: " + reflectErr.Error(), IsError: true})
	}
	target, err := r.sessions.Get(ctx, install.workspace.SessionID)
	if err != nil {
		return chatpkg.Result{}, err
	}
	history, err := r.sessions.Turns(ctx, target.ID)
	if err != nil {
		return chatpkg.Result{}, fmt.Errorf("load workspace session %s: %w", target.ID, err)
	}
	history = session.Repair(history)
	attachedNames, err := (&skill.Attachments{DB: r.st.DB, Lifecycle: r.st.SkillLifecycleGuard()}).List(ctx, target.ID)
	if err != nil {
		return chatpkg.Result{}, err
	}
	attachedSkills, attachedSystem, err := buildAttachedSkillContext(workspaceBaseSystem, activeSkills, attachedNames)
	if err != nil {
		return chatpkg.Result{}, err
	}
	workspaceAgent.System = attachedSystem
	modelError := ""
	if target.ModelAlias != "" {
		if _, resolveErr := r.cfg.ResolveModel(target.ModelAlias); resolveErr != nil {
			modelError = resolveErr.Error()
		} else {
			workspaceAgent.Model = target.ModelAlias
		}
	}

	r.mu.Lock()
	if r.agentCancel != nil {
		r.mu.Unlock()
		return chatpkg.Result{Confirm: true, Text: "A turn started while the workspace was opening; confirm before switching sessions."}, nil
	}
	oldSessionID := ""
	if r.current != nil {
		oldSessionID = r.current.ID
	}
	if !r.sessionOwners.transfer(r, oldSessionID, target.ID) {
		r.mu.Unlock()
		return chatpkg.Result{}, sessionAlreadyActiveError{sessionID: target.ID}
	}
	retired := newChatRuntimeCleanup(r.wsClient, r.agentCleanupContext)
	if retired != nil {
		r.retiredCleanup = append(r.retiredCleanup, retired)
	}
	r.wsClient = install.client
	adopted = true
	r.agent = workspaceAgent
	r.baseSystem = workspaceBaseSystem
	r.attachedSkills = attachedSkills
	r.agentCleanupContext = replacementCleanup
	cleanupAdopted = true
	r.profileName = profileName
	r.current = target
	r.ownedSessionID = target.ID
	r.history = history
	r.persisted = len(history)
	r.modelError = modelError
	r.workspace = fmt.Sprintf("%s at /work/repo", install.workspace.Repo)
	state := r.stateLocked(r.capabilities)
	r.mu.Unlock()
	_ = r.cleanupRetiredResources(ctx)
	if emit != nil {
		emit(chatpkg.Event{Kind: chatpkg.EventState, State: &state})
	}
	return chatpkg.Result{Text: fmt.Sprintf("workspace %s: %s at /work/repo, image %s", install.workspace.ID, install.workspace.Repo, install.workspace.Image), State: &state}, nil
}

func (r *chatRuntime) commandWorkset(ctx context.Context, args string) (chatpkg.Result, error) {
	r.mu.Lock()
	if r.current == nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("no active session working set")
	}
	sessionID := r.current.ID
	r.mu.Unlock()
	ws := &workset.Store{DB: r.st.DB}
	fields := strings.Fields(args)
	if len(fields) == 0 || fields[0] == "list" {
		if len(fields) > 1 {
			return chatpkg.Result{}, errors.New("usage: /workset [list|replace <id> <text>|drop <id>|clear]")
		}
		entries, err := ws.List(ctx, sessionID)
		if err != nil {
			return chatpkg.Result{}, err
		}
		items := make([]chatpkg.WorkItem, len(entries))
		for i, entry := range entries {
			items[i] = chatpkg.WorkItem{ID: entry.ID, Text: entry.Body}
		}
		text := "working set is empty"
		if len(entries) > 0 {
			text = strings.TrimSpace(workset.Render(entries))
		}
		return chatpkg.Result{Title: "Working set", Text: text, Workset: items}, nil
	}
	switch fields[0] {
	case "replace":
		if len(fields) < 3 {
			return chatpkg.Result{}, errors.New("usage: /workset replace <id> <text>")
		}
		body := strings.TrimSpace(strings.TrimPrefix(args, "replace "+fields[1]))
		entry, err := ws.Replace(ctx, sessionID, fields[1], body, workset.SourceUser)
		if err != nil {
			return chatpkg.Result{}, err
		}
		return chatpkg.Result{Text: "replaced " + entry.ID}, nil
	case "drop":
		if len(fields) != 2 {
			return chatpkg.Result{}, errors.New("usage: /workset drop <id>")
		}
		if err := ws.Drop(ctx, sessionID, fields[1]); err != nil {
			return chatpkg.Result{}, err
		}
		return chatpkg.Result{Text: "dropped " + fields[1]}, nil
	case "clear":
		if len(fields) != 1 {
			return chatpkg.Result{}, errors.New("usage: /workset clear")
		}
		if err := ws.Clear(ctx, sessionID); err != nil {
			return chatpkg.Result{}, err
		}
		return chatpkg.Result{Text: "working set cleared"}, nil
	default:
		return chatpkg.Result{}, errors.New("usage: /workset [list|replace <id> <text>|drop <id>|clear]")
	}
}

func (r *chatRuntime) commandModel(ctx context.Context, alias string) (chatpkg.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.agent == nil {
		return chatpkg.Result{}, errors.New("chat runtime is not open")
	}
	if r.agentCancel != nil {
		return chatpkg.Result{}, errors.New("cannot change model while a turn is active")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return chatpkg.Result{Title: "Choose a model", Models: r.modelsLocked()}, nil
	}
	if _, err := r.cfg.ResolveModel(alias); err != nil {
		return chatpkg.Result{}, err
	}
	if err := r.sessions.SetModelAliasIfVersion(ctx, r.current.ID, alias, r.current.ModelAliasVersion); err != nil {
		return chatpkg.Result{}, err
	}
	r.current.ModelAlias = alias
	r.current.ModelAliasVersion++
	r.agent.Model = alias
	r.modelError = ""
	state := r.stateLocked(r.capabilities)
	return chatpkg.Result{Text: fmt.Sprintf("model set to %s", alias), State: &state}, nil
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
func (r *chatRuntime) worksetCommand(ctx context.Context, args string) (string, error) {
	result, err := r.commandWorkset(ctx, args)
	return result.Text, err
}

func (r *chatRuntime) repoCommand(ctx context.Context, repoArg string, stdout io.Writer) error {
	result, err := r.commandRepo(ctx, repoArg, func(event chatpkg.Event) { renderChatEvent(event, stdout, io.Discard) })
	if err == nil {
		renderChatResult(result, stdout)
	}
	return err
}

func (r *chatRuntime) switchToWorkspaceSession(ctx context.Context, sessionID string) error {
	target, err := r.sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	turns, err := r.sessions.Turns(ctx, sessionID)
	if err != nil {
		currentID := ""
		if r.current != nil {
			currentID = r.current.ID
		}
		return fmt.Errorf("load workspace session %s (staying on session %s): %w", sessionID, currentID, err)
	}
	turns = session.Repair(turns)
	r.mu.Lock()
	defer r.mu.Unlock()
	oldSessionID := ""
	if r.current != nil {
		oldSessionID = r.current.ID
	}
	if !r.sessionOwners.transfer(r, oldSessionID, target.ID) {
		return sessionAlreadyActiveError{sessionID: target.ID}
	}
	r.current = target
	r.ownedSessionID = target.ID
	r.history = turns
	r.persisted = len(turns)
	if r.agent != nil && target.ModelAlias != "" {
		if _, resolveErr := r.cfg.ResolveModel(target.ModelAlias); resolveErr != nil {
			r.modelError = resolveErr.Error()
		} else {
			r.modelError = ""
			r.agent.Model = target.ModelAlias
		}
	}
	return nil
}

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

func (r *chatRuntime) finishCommand(commandCancel context.CancelFunc, commandDone chan struct{}) {
	commandCancel()
	r.mu.Lock()
	if r.commandDone == commandDone {
		r.commandCancel = nil
		r.commandDone = nil
	}
	close(commandDone)
	r.mu.Unlock()
}

func detachedRuntimeCloseContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
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

func (r *chatRuntime) cleanup(ctx context.Context) error {
	var reflectionErr error
	if err := r.reflectSession(ctx); err != nil {
		reflectionErr = err
	}

	var teardownErr error
	r.mu.Lock()
	wsClient := r.wsClient
	agentCleanup := r.agentCleanupContext
	resourceCancel := r.resourceCancel
	ownedSessionID := r.ownedSessionID
	r.mu.Unlock()
	if wsClient != nil {
		if err := closeRuntimeResource(ctx, wsClient); err != nil {
			teardownErr = errors.Join(teardownErr, err)
		} else {
			r.mu.Lock()
			r.wsClient = nil
			r.mu.Unlock()
		}
	}
	if agentCleanup != nil {
		if err := agentCleanup(ctx); err != nil {
			teardownErr = errors.Join(teardownErr, err)
		} else {
			r.mu.Lock()
			if r.agentCleanupContext != nil {
				r.agentCleanupContext = nil
			}
			r.mu.Unlock()
		}
	}
	teardownErr = errors.Join(teardownErr, r.cleanupRetiredResources(ctx))
	if teardownErr != nil {
		return errors.Join(reflectionErr, teardownErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.sessionOwners.releaseContext(ctx, r, ownedSessionID); err != nil {
		return err
	}
	if resourceCancel != nil {
		resourceCancel()
	}
	r.mu.Lock()
	r.resourceCancel = nil
	if r.ownedSessionID == ownedSessionID {
		r.ownedSessionID = ""
	}
	r.mu.Unlock()
	if reflectionErr != nil {
		return completedChatCleanupError{err: reflectionErr}
	}
	return nil
}

func (r *chatRuntime) cleanupRetiredResources(ctx context.Context) error {
	r.mu.Lock()
	retired := append([]*chatRuntimeCleanup(nil), r.retiredCleanup...)
	r.mu.Unlock()

	var cleanupErr error
	for _, cleanup := range retired {
		cleanupErr = errors.Join(cleanupErr, cleanup.close(ctx))
	}

	r.mu.Lock()
	remaining := r.retiredCleanup[:0]
	for _, cleanup := range r.retiredCleanup {
		if !cleanup.complete() {
			remaining = append(remaining, cleanup)
		}
	}
	for i := len(remaining); i < len(r.retiredCleanup); i++ {
		r.retiredCleanup[i] = nil
	}
	r.retiredCleanup = remaining
	r.mu.Unlock()
	return cleanupErr
}

type completedChatCleanupError struct{ err error }

func (e completedChatCleanupError) Error() string        { return e.err.Error() }
func (e completedChatCleanupError) Unwrap() error        { return e.err }
func (completedChatCleanupError) CleanupCompleted() bool { return true }

func cleanupCompleted(err error) bool {
	if err == nil {
		return true
	}
	var completed interface{ CleanupCompleted() bool }
	return errors.As(err, &completed) && completed.CleanupCompleted()
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
