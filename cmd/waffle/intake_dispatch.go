package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/matt-riley/waffle/internal/agent"
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
)

// issueDispatcher opens a repo workspace and runs one issue on the restricted
// issue agent tier (#51). Workspace tools execute inside the container.
type issueDispatcher struct {
	cfg       config.Config
	st        *store.Store
	sessions  *session.Store
	skills    []skill.Skill
	memWS     memory.Workspace
	broker    *broker.Broker
	brokerURL string
	agent     *agent.Agent
	log       *slog.Logger
}

func (d *issueDispatcher) Dispatch(ctx context.Context, watch intake.WatchConfig, iss intake.Issue) (string, error) {
	mgr := newWorkspaceManager(d.cfg, d.st, d.broker)
	mgr.BrokerURL = d.brokerURL
	ws, client, err := mgr.Open(ctx, watch.Repo)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	defer func() { _ = client.Close() }()

	// before_run: fatal on failure.
	if res, err := mgr.RunHookFor(ctx, client, hooks.BeforeRun, ws.ID, ws.SessionID); err != nil {
		return "", err
	} else if res.Output != "" {
		d.log.Info("before_run hook", "output", res.Output)
	}

	// Repo policy may only tighten the issue-tier tool policy.
	hostPol := d.cfg.AgentPolicy(config.GroupIssue)
	toolPol := tool.Policy{Allow: hostPol.Allow, Deny: hostPol.Deny}
	var repoPrompt string
	if raw, err := mgrBashCat(ctx, client, "/work/repo/WAFFLE.md"); err == nil && strings.TrimSpace(raw) != "" {
		if p, perr := repopolicy.Parse(raw); perr != nil {
			return "", fmt.Errorf("repo policy: %w", perr)
		} else {
			toolPol = repopolicy.TightenTools(toolPol, p.Tools)
			repoPrompt = p.PromptBlock()
		}
	} else if raw, err := mgrBashCat(ctx, client, "/work/repo/AGENT.md"); err == nil && strings.TrimSpace(raw) != "" {
		if p, perr := repopolicy.Parse(raw); perr != nil {
			return "", fmt.Errorf("repo policy: %w", perr)
		} else {
			toolPol = repopolicy.TightenTools(toolPol, p.Tools)
			repoPrompt = p.PromptBlock()
		}
	}

	// Tools: workspace container queue + restricted issue agent tools.
	// Memory write tools are already denied by GroupIssue policy.
	baseTools := tool.Restrict(d.agent.Tools, toolPol)
	runTools := tool.Restrict(tool.Combine(sandbox.NewQueueToolbox(client), baseTools), toolPol)

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
		_ = d.sessions.AppendTurn(ctx, ws.SessionID, m)
	}

	// after_run is best-effort.
	if res, _ := mgr.RunHookFor(ctx, client, hooks.AfterRun, ws.ID, ws.SessionID); res.Err != nil {
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
	mgr := newWorkspaceManager(d.cfg, d.st, d.broker)
	mgr.BrokerURL = d.brokerURL
	_, err := mgr.Close(ctx, claim.WorkspaceID, true)
	return err
}

func mgrBashCat(ctx context.Context, client *sandbox.Client, path string) (string, error) {
	input := []byte(fmt.Sprintf(`{"command":%q}`, "cat "+path))
	out, isError, err := client.Exec(ctx, "bash", input)
	if err != nil {
		return "", err
	}
	if isError {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

// ensureIssueAgent builds or returns the restricted issue-tier agent.
func ensureIssueAgent(ctx context.Context, cfg config.Config, memWS memory.Workspace, skills []skill.Skill, sessions *session.Store, agents map[string]*agent.Agent) (*agent.Agent, func(), error) {
	if a := agents[config.GroupIssue]; a != nil {
		return a, func() {}, nil
	}
	return buildAgent(ctx, cfg, memWS, skills, sessions, config.GroupIssue)
}
