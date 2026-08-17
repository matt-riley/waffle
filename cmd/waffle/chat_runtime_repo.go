package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/agentbuild"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/tool"
)

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
	if !r.persistable() {
		r.mu.Unlock()
		return chatpkg.Result{}, errors.New("temporary conversations cannot open a durable workspace")
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
	return buildAgentWithProfileContext(ctx, r.cfg, memWS, skills, r.sessions, config.GroupMain, profileName, r.api)
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
	profile, _ := r.cfg.Profile(profileName)
	toolPolicy, _ := agentbuild.ApplyProfile(hostPolicy, profile)
	toolPolicy.Profile = currentAgent.Profile
	if toolPolicy.Profile == "" {
		toolPolicy.Profile = "main"
	}
	systemExtra := fmt.Sprintf("\n\nYou are working in a container workspace on the repository %s, cloned at /work/repo. Your shell and file tools execute inside that container. Git pushes authenticate automatically.", install.workspace.Repo)
	if install.policy != nil {
		toolPolicy = agentbuild.ApplyRepo(toolPolicy, install.policy)
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
