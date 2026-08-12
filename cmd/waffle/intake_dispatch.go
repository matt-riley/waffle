package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/agentbuild"
	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/hooks"
	"github.com/matt-riley/waffle/internal/intake"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/workspace"
)

// issueRunSession is one open workspace used for a single issue dispatch.
// Production uses a real container queue client; tests inject fakes (#51 e2e).
type issueRunSession interface {
	Workspace() *workspace.Workspace
	QueueTools() tool.Toolbox
	LoadRepoPolicy(ctx context.Context) (*repopolicy.Policy, error)
	RunHook(ctx context.Context, point hooks.Point) (hooks.Result, error)
	Close() error
}

// issueWorkspaceOpener opens (or resumes) a repo workspace for issue intake.
type issueWorkspaceOpener interface {
	Open(ctx context.Context, repo string) (issueRunSession, error)
	CloseWorkspace(ctx context.Context, workspaceID string, force bool) error
}

// turnAppender is the transcript persistence surface the issue dispatcher
// needs. An interface lets tests inject AppendTurn failures (#284).
type turnAppender interface {
	AppendTurn(ctx context.Context, sessionID string, msg llm.Message) error
}

// issueDispatcher opens a repo workspace and runs one issue on the restricted
// issue agent tier (#51). Workspace tools execute inside the container.
type issueDispatcher struct {
	cfg       config.Config
	st        *store.Store
	sessions  turnAppender
	skills    []skill.Skill
	memWS     memory.Workspace
	broker    *broker.Broker
	brokerURL string
	agent     *agent.Agent
	log       *slog.Logger

	// opener, when non-nil, replaces the production workspace.Manager path.
	// Tests inject a fake so Dispatch can be exercised without Docker.
	opener issueWorkspaceOpener
}

func (d *issueDispatcher) workspaceOpener() issueWorkspaceOpener {
	if d.opener != nil {
		return d.opener
	}
	return &prodIssueOpener{
		cfg:       d.cfg,
		st:        d.st,
		broker:    d.broker,
		brokerURL: d.brokerURL,
	}
}

func (d *issueDispatcher) Dispatch(ctx context.Context, watch intake.WatchConfig, iss intake.Issue, onClaim intake.ClaimUpdate) (string, error) {
	run, err := d.workspaceOpener().Open(ctx, watch.Repo)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = run.Close() }()

	ws := run.Workspace()
	// Report the opened workspace/session immediately so the claim carries
	// the identity reconciliation needs to force-close the workspace when
	// the issue closes mid-run (#296).
	if onClaim != nil {
		if err := onClaim(ws.ID, ws.SessionID); err != nil {
			return "", fmt.Errorf("record running claim: %w", err)
		}
	}

	// before_run: fatal on failure.
	if res, err := run.RunHook(ctx, hooks.BeforeRun); err != nil {
		return "", err
	} else if res.Output != "" {
		d.log.Info("before_run hook", "output", res.Output)
	}

	// Repo policy may only tighten the issue-tier tool policy; body is
	// injected as untrusted data. Manager also applies egress/idle/hooks (#53).
	hostPol := d.cfg.AgentPolicy(config.GroupIssue)
	toolPol := tool.Policy{Allow: hostPol.Allow, Deny: hostPol.Deny}
	var repoPrompt string
	if p, perr := run.LoadRepoPolicy(ctx); perr != nil {
		return "", perr
	} else if p != nil {
		toolPol = repopolicy.TightenTools(toolPol, p.Tools)
		toolPol = agentbuild.ApplyCodeIntelCaps(toolPol, p.CodeIntelCaps)
		repoPrompt = p.PromptBlock()
	}

	// Tools: workspace container queue + restricted issue agent tools.
	// Memory write tools are already denied by GroupIssue policy.
	baseTools := tool.Restrict(d.agent.Tools, toolPol)
	runTools := tool.Restrict(tool.Combine(run.QueueTools(), baseTools), toolPol)

	sys := d.agent.System
	sys += fmt.Sprintf("\n\nYou are working in a container workspace on %s at /work/repo.", ws.Repo)
	if repoPrompt != "" {
		sys += "\n\n" + repoPrompt
	}

	runAgent := &agent.Agent{
		Provider:  d.agent.Provider,
		Tools:     runTools,
		System:    sys,
		Model:     d.agent.Model,
		MaxTokens: d.agent.MaxTokens,
		Redact:    d.agent.Redact,
		Usage:     d.agent.Usage,
		Limits:    d.agent.Limits,
	}

	prompt := intake.PromptForIssue(iss)
	history := []llm.Message{llm.UserText(prompt)}
	if err := d.sessions.AppendTurn(ctx, ws.SessionID, history[0]); err != nil {
		return "", err
	}
	runCtx := agent.WithSession(ctx, ws.SessionID)
	out, runErr := runAgent.Run(runCtx, history, agent.Hooks{})
	for _, m := range out[1:] {
		if err := d.sessions.AppendTurn(ctx, ws.SessionID, m); err != nil {
			// A missing transcript must not be reported as a completed issue:
			// fail the run so the watcher retries it (#284).
			d.log.Warn("persist intake turn", "err", err)
			if runErr == nil {
				runErr = fmt.Errorf("persist turn: %w", err)
			}
			break
		}
	}

	// after_run is best-effort.
	if res, _ := run.RunHook(ctx, hooks.AfterRun); res.Err != nil {
		d.log.Warn("after_run hook failed", "err", res.Err, "output", res.Output)
	} else if res.Output != "" {
		d.log.Info("after_run hook", "output", res.Output)
	}

	var reply string
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == llm.RoleAssistant {
			reply = out[i].Text()
			break
		}
	}
	if runErr != nil {
		if reply == "" {
			return "", runErr
		}
		return reply + "\n\n(run error: " + runErr.Error() + ")", runErr
	}
	if reply == "" {
		reply = fmt.Sprintf("issue #%d completed with no assistant text", iss.Number)
	}
	return reply, nil
}

func (d *issueDispatcher) Cancel(ctx context.Context, claim intake.Claim) error {
	if claim.WorkspaceID == "" {
		// Best-effort: close any open workspace for the watched repo.
		return nil
	}
	return d.workspaceOpener().CloseWorkspace(ctx, claim.WorkspaceID, true)
}

// prodIssueOpener is the production workspace path (Docker container + queue).
type prodIssueOpener struct {
	cfg       config.Config
	st        *store.Store
	broker    *broker.Broker
	brokerURL string
}

func (o *prodIssueOpener) Open(ctx context.Context, repo string) (issueRunSession, error) {
	mgr := newWorkspaceManager(o.cfg, o.st, o.broker)
	if o.broker != nil {
		limits := brokerLimits(o.cfg, config.GroupIssue)
		grants := o.cfg.APIFaceGrants(config.GroupIssue)
		mgr.MintToken = func(mintCtx context.Context, sessionID string) (string, error) {
			return o.broker.MintScopedFaces(mintCtx, sessionID, sessionID, limits, grants)
		}
	}
	mgr.BrokerURL = o.brokerURL
	ws, client, err := mgr.Open(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &prodIssueRun{ws: ws, client: client, mgr: mgr}, nil
}

func (o *prodIssueOpener) CloseWorkspace(ctx context.Context, workspaceID string, force bool) error {
	mgr := newWorkspaceManager(o.cfg, o.st, o.broker)
	mgr.BrokerURL = o.brokerURL
	_, err := mgr.Close(ctx, workspaceID, force)
	return err
}

type prodIssueRun struct {
	ws     *workspace.Workspace
	client *sandbox.Client
	mgr    *workspace.Manager
}

func (r *prodIssueRun) Workspace() *workspace.Workspace { return r.ws }

func (r *prodIssueRun) QueueTools() tool.Toolbox {
	return sandbox.NewQueueToolbox(r.client)
}

func (r *prodIssueRun) LoadRepoPolicy(ctx context.Context) (*repopolicy.Policy, error) {
	return r.mgr.LoadRepoPolicy(ctx, r.client)
}

func (r *prodIssueRun) RunHook(ctx context.Context, point hooks.Point) (hooks.Result, error) {
	return r.mgr.RunHookFor(ctx, r.client, point, r.ws.ID, r.ws.SessionID)
}

func (r *prodIssueRun) Close() error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}

// ensureIssueAgent builds or returns the restricted issue-tier agent.
func ensureIssueAgent(ctx context.Context, cfg config.Config, memWS memory.Workspace, skills []skill.Skill, sessions *session.Store, agents map[string]*agent.Agent, api apiBrokerWiring) (*agent.Agent, func(), error) {
	if a := agents[config.GroupIssue]; a != nil {
		return a, func() {}, nil
	}
	return buildAgent(ctx, cfg, memWS, skills, sessions, config.GroupIssue, api)
}
