package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
)

func (r *chatRuntime) Command(ctx context.Context, command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	redact := r.runtimeRedactor()
	redactedEmit := func(event chatpkg.Event) {
		if emit != nil {
			emit(chatpkg.RedactEvent(event, redact))
		}
	}
	result, err := r.command(ctx, command, redactedEmit)
	return chatpkg.RedactResult(result, redact), redactChatError(err, redact)
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
		chatpkg.CommandRepo, chatpkg.CommandWorkset, chatpkg.CommandRename,
		chatpkg.CommandPin, chatpkg.CommandUnpin, chatpkg.CommandDelete, chatpkg.CommandExit:
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
	case chatpkg.CommandRename, chatpkg.CommandPin, chatpkg.CommandUnpin, chatpkg.CommandDelete:
		return args != ""
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
	case chatpkg.CommandRename:
		return r.commandRename(ctx, command.Args)
	case chatpkg.CommandPin:
		return r.commandPin(ctx, command.Args, true)
	case chatpkg.CommandUnpin:
		return r.commandPin(ctx, command.Args, false)
	case chatpkg.CommandDelete:
		return r.commandDelete(ctx, command.Args, emit)
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

// commandRename renames a conversation. The title is the remainder of the
// arguments after the session id and is bounded to keep labels readable.
func (r *chatRuntime) commandRename(ctx context.Context, args string) (chatpkg.Result, error) {
	id, title, ok := strings.Cut(args, " ")
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	if !ok || id == "" || title == "" {
		return chatpkg.Result{}, errors.New("usage: /rename <session> <title>")
	}
	if len([]rune(title)) > 200 {
		return chatpkg.Result{}, errors.New("title is too long (maximum 200 characters)")
	}
	if err := r.sessions.SetTitle(ctx, id, title); err != nil {
		return chatpkg.Result{}, err
	}
	return chatpkg.Result{Text: fmt.Sprintf("renamed conversation %s", id)}, nil
}

// commandPin pins or unpins a conversation without changing its recency.
func (r *chatRuntime) commandPin(ctx context.Context, args string, pinned bool) (chatpkg.Result, error) {
	id := strings.TrimSpace(args)
	if id == "" {
		verb := "pin"
		if !pinned {
			verb = "unpin"
		}
		return chatpkg.Result{}, fmt.Errorf("usage: /%s <session>", verb)
	}
	if err := r.sessions.SetPinned(ctx, id, pinned); err != nil {
		return chatpkg.Result{}, err
	}
	verb := "pinned"
	if !pinned {
		verb = "unpinned"
	}
	return chatpkg.Result{Text: fmt.Sprintf("%s conversation %s", verb, id)}, nil
}

// commandDelete removes a conversation. Deleting the current conversation
// fails closed while a turn is active and otherwise starts a fresh session so
// the desk always has a valid authoritative session to render.
func (r *chatRuntime) commandDelete(ctx context.Context, id string, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return chatpkg.Result{}, errors.New("usage: /delete <session>")
	}
	r.mu.Lock()
	if r.current == nil {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("chat runtime is not open")
	}
	currentID := r.current.ID
	active := r.agentCancel != nil
	r.mu.Unlock()
	if id == currentID && active {
		return chatpkg.Result{}, errors.New("a turn is active in this conversation; wait for it to finish before deleting")
	}
	if _, err := r.sessions.Get(ctx, id); err != nil {
		return chatpkg.Result{}, err
	}
	if err := r.sessions.Delete(ctx, id); err != nil {
		if errors.Is(err, session.ErrSessionWorkspaceActive) {
			return chatpkg.Result{}, errors.New("this conversation owns a live workspace; close it before deleting")
		}
		return chatpkg.Result{}, err
	}
	if id != currentID {
		return chatpkg.Result{Text: fmt.Sprintf("deleted conversation %s", id)}, nil
	}
	// The current conversation was deleted: open a fresh one in its place.
	dropped, err := r.resetSession(ctx)
	if err != nil {
		return chatpkg.Result{}, fmt.Errorf("deleted %s but could not start a fresh session: %w", id, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.stateLocked(r.capabilities)
	text := fmt.Sprintf("deleted conversation %s; started %s", id, r.current.ID)
	if dropped > 0 {
		text += fmt.Sprintf("; dropped %d unpinned model assumptions", dropped)
	}
	return chatpkg.Result{Text: text, State: &state}, nil
}

func chatSessions(sessions []session.Session) []chatpkg.Session {
	out := make([]chatpkg.Session, len(sessions))
	for i, value := range sessions {
		out[i] = chatpkg.Session{
			ID: value.ID, Title: value.Title, Summary: value.Summary,
			ModelAlias: value.ModelAlias, UpdatedAt: value.UpdatedAt,
			Pinned: value.Pinned,
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
		"Current session totals: requests=%d input=%d cache_write=%d cache_read=%d output=%d reserved=%d tunnel_bytes=%d\nPersisted aggregate totals: requests=%d input=%d cache_write=%d cache_read=%d output=%d reserved=%d tunnel_bytes=%d",
		currentTotals.Requests, currentTotals.InputTokens, currentTotals.CacheCreationInputTokens, currentTotals.CacheReadInputTokens, currentTotals.OutputTokens, currentTotals.ReservedTokens, currentTotals.TunnelBytes,
		aggregateTotals.Requests, aggregateTotals.InputTokens, aggregateTotals.CacheCreationInputTokens, aggregateTotals.CacheReadInputTokens, aggregateTotals.OutputTokens, aggregateTotals.ReservedTokens, aggregateTotals.TunnelBytes,
	)
	return chatpkg.Result{Title: "Usage", Text: text, Usage: rows}, nil
}

func chatUsageRows(rows []usagepkg.Row) []chatpkg.UsageRow {
	out := make([]chatpkg.UsageRow, len(rows))
	for i, row := range rows {
		out[i] = chatpkg.UsageRow{
			SessionID: row.SessionID, Period: row.Period, PeriodStart: row.PeriodStart,
			Requests: row.Requests, InputTokens: row.InputTokens,
			CacheCreationInputTokens: row.CacheCreationInputTokens,
			CacheReadInputTokens:     row.CacheReadInputTokens,
			OutputTokens:             row.OutputTokens,
			ReservedTokens:           row.ReservedTokens,
			TunnelBytes:              row.TunnelBytes,
		}
	}
	return out
}

func totalUsageRows(rows []usagepkg.Row) usagepkg.Row {
	var total usagepkg.Row
	for _, row := range rows {
		total.Requests += row.Requests
		total.InputTokens += row.InputTokens
		total.CacheCreationInputTokens += row.CacheCreationInputTokens
		total.CacheReadInputTokens += row.CacheReadInputTokens
		total.OutputTokens += row.OutputTokens
		total.ReservedTokens += row.ReservedTokens
		total.TunnelBytes += row.TunnelBytes
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

func (r *chatRuntime) worksetCommand(ctx context.Context, args string) (string, error) {
	result, err := r.commandWorkset(ctx, args)
	return result.Text, err
}
