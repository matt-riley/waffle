// Package agentbuild assembles the trust-boundary agent: group policy →
// action policy engine → host tools (memory/workset/spill/PR) → sandbox
// start → MCP ConnectRestricted → codeintel → subagent wrapper →
// agent.Agent.
//
// The composition used to live in package main (cmd/waffle/chat_cmd.go),
// which meant every new agent surface re-imported main-local helpers and the
// policy produced by a full build could drift from the policy re-derived by
// post-/repo overlays. The Builder here is the single composition root;
// cmd keeps flags, open/close, and wiring only (#287).
package agentbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/codeintel"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/policy"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

// Runtime is the model runtime surface the builder needs: the provider the
// agent calls plus the connection resolution and transcript redaction that
// come with it. cmd/waffle's modelRuntimeResolver implements it.
type Runtime interface {
	llm.Provider
	// Resolve resolves an alias to its connection settings (for token limits).
	Resolve(alias string) (config.ResolvedModel, error)
	// Redact applies every enrolled provider credential redactor.
	Redact(s string) string
}

// Builder assembles agents. Dependencies are immutable for the Builder's
// lifetime; Build may be called many times (serve builds one agent per
// group/profile combination).
type Builder struct {
	Config    config.Config
	Sessions  *session.Store
	Workspace memory.Workspace
	Skills    []skill.Skill
	// Runtime resolves and serves model calls. Must be non-nil.
	Runtime Runtime
	// GitHubApp constructs the pull-request tool's app on demand; nil
	// disables the tool (the token never enters a container).
	GitHubApp func() (*gitcred.App, error)
}

// Build assembles the agent for one agent group/profile. The returned
// cleanup stops any sandbox container or MCP server; it is never nil and
// must be called when the agent is done. It honors the caller's context
// (a bounded shutdown window stops Docker/MCP with that deadline).
func (b *Builder) Build(ctx context.Context, group, profileName string) (*agent.Agent, Cleanup, error) {
	if b.Runtime == nil {
		return nil, Cleanup(func(context.Context) error { return nil }), errors.New("agentbuild: Runtime is required")
	}
	cleanup := Cleanup(func(context.Context) error { return nil })
	pol := b.Config.AgentPolicy(group)
	profileName = strings.TrimSpace(profileName)
	profile, ok := b.Config.Profile(profileName)
	if profileName != "" && profileName != "main" && !ok {
		return nil, cleanup, fmt.Errorf("unknown agent profile %q", profileName)
	}
	// Effective profile name for denials/logs (empty → main).
	effectiveProfile := strings.TrimSpace(profileName)
	if effectiveProfile == "" {
		effectiveProfile = "main"
	}
	// The file-tool boundary (#269), resolved from the group policy before the
	// profile is merged in. Narrowing and the "may only tighten" rule live in
	// config.ApplyProfilePolicy / ValidateProfileFileRoots — the same pair Desk
	// and the profile editor validate through — so the runtime cannot drift
	// from what the editor accepted.
	groupRoots, err := tool.NewFileRoots(pol.FileRoots)
	if err != nil {
		return nil, cleanup, fmt.Errorf("agent group %q file_roots: %w", group, err)
	}
	if err := config.ValidateProfileFileRoots(pol, profile); err != nil {
		return nil, cleanup, fmt.Errorf("agent profile %q: %w", effectiveProfile, err)
	}
	fileRoots, err := tool.NewFileRoots(config.ApplyProfilePolicy(pol, profile).FileRoots)
	if err != nil {
		return nil, cleanup, fmt.Errorf("agent profile %q file_roots: %w", effectiveProfile, err)
	}
	// Belt and braces: the lexical config check cannot see symlinks, so hold
	// the resolved roots to the group's resolved roots too.
	if !groupRoots.Confines(fileRoots) {
		return nil, cleanup, fmt.Errorf("agent profile %q file_roots resolve outside the group's roots %v", effectiveProfile, groupRoots.Roots())
	}

	toolPolicy, sandboxMode := ApplyProfile(pol, profile)
	toolPolicy.Profile = effectiveProfile

	// Action-level [[policy.rule]] evaluation + optional policy_audit (#66).
	// require rules use a shared SessionEvents log (write → predicate → allow).
	if policyRules := b.Config.Policy.PolicyRules(); len(policyRules) > 0 {
		rules := make([]policy.Rule, 0, len(policyRules))
		for _, r := range policyRules {
			rules = append(rules, policy.Rule{
				Name: r.Name, Tool: r.Tool, Match: r.Match,
				Regex: r.Regex, Action: r.Action, Requires: r.Requires,
				Guidance: r.Guidance,
			})
		}
		engine, err := policy.NewEngineFromStore(&store.Store{DB: b.Sessions.DB()}, rules, b.Config.Sandbox.Enforcer)
		if err != nil {
			return nil, cleanup, fmt.Errorf("policy engine: %w", err)
		}
		// Every matching decision is documented as audited; a lost audit
		// write is reported rather than discarded (#297).
		engine.Log = slog.Default()
		toolPolicy.CheckAction = func(ctx context.Context, name string, input json.RawMessage) error {
			d := engine.CheckAndAuditSession(ctx, session.IDFromContext(ctx), name, input)
			if !d.Allowed {
				return tool.NewPolicyDenial(effectiveProfile, "policy.rule", d.Rule, d.Message)
			}
			return nil
		}
		toolPolicy.ObserveSuccess = func(ctx context.Context, name string, input json.RawMessage) {
			engine.ObserveSuccess(session.IDFromContext(ctx), name, input)
		}
	}
	var (
		cleanupMu sync.Mutex
		closers   []Cleanup
	)
	cleanup = func(ctx context.Context) error {
		cleanupMu.Lock()
		defer cleanupMu.Unlock()
		var cleanupErr error
		for i := len(closers) - 1; i >= 0; i-- {
			if closers[i] == nil {
				continue
			}
			if err := ctx.Err(); err != nil {
				return errors.Join(cleanupErr, err)
			}
			if err := closers[i](ctx); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
				continue
			}
			closers[i] = nil
		}
		return cleanupErr
	}

	spillStore := &spill.Store{DB: b.Sessions.DB()}
	wsStore := &workset.Store{DB: b.Sessions.DB()}
	notesIdx := &memory.NotesIndex{DB: b.Sessions.DB()}

	// Host tools execute on the host regardless of sandbox mode — memory
	// is waffle's own state, and learning writes to the workspace.
	ws := b.Workspace
	ws.InjectBudget = b.Config.Memory.InjectBudget
	ws.Notes = notesIdx
	// Best-effort: reindex on-disk notes into FTS after upgrades (#60).
	agentName := ws.Agent
	if agentName == "" {
		agentName = memory.DefaultAgent
	}
	syncWorkspaceOnce(notesIdx, agentName, ws)
	hostToolList := []tool.Tool{
		memory.RememberTool{WS: ws, Notes: notesIdx, Gate: &memory.Gate{Mode: b.Config.Memory.WriteGate, WS: ws}, Provenance: memory.Provenance{TrustClass: "owner_stated"}},
		memory.MemoryUpdateTool{WS: ws, Notes: notesIdx, Provenance: memory.Provenance{TrustClass: "owner_stated"}},
		memory.RecallTool{Sessions: b.Sessions, WS: ws, Notes: notesIdx, Spills: spillStore},
		workset.UpdateTool{Store: wsStore},
		spill.ExpandTool{Store: spillStore},
		session.ExpandContextTool{Sessions: b.Sessions},
	}
	if b.Config.Agent.Learn {
		hostToolList = append(hostToolList, memory.DistillTool{WS: ws, Gate: &memory.Gate{Mode: b.Config.Memory.WriteGate, WS: ws}})
	}
	// Opening a pull request needs pull_requests:write, which the workspace git
	// credential deliberately never carries — anything inside a container can
	// read its credentials back out with `git credential fill`. So the tool runs
	// on the host, mints a token for one call, and scopes it to the repo the
	// session's workspace is bound to. A missing or misconfigured app is not an
	// error here: the tool simply is not offered.
	if b.GitHubApp != nil {
		if app, appErr := b.GitHubApp(); appErr == nil && app != nil {
			bindings := &workspace.Manager{DB: b.Sessions.DB()}
			hostToolList = append(hostToolList, gitcred.PullRequestTool{
				App: app,
				Repo: func(ctx context.Context, sessionID string) (string, error) {
					// Reuse the manager's own lookup rather than repeating the
					// query: this is the same binding the broker scopes git
					// credentials by, and a second copy could drift from it.
					bound, err := bindings.ForSession(ctx, sessionID)
					if err != nil {
						return "", err
					}
					return bound.Repo, nil
				},
			})
		}
	}
	hostTools := tool.NewRegistry(hostToolList...)

	// Optional code intelligence (#79): in-process text-fallback tools.
	// Absence is fine — agent keeps search/read. MCP codeintel servers are
	// validated at config load and launched via ConnectRestricted (#77);
	// when MCP is unavailable the agent keeps this go/parser fallback
	// (see docs/code-intelligence.md).
	var codeTools tool.Toolbox
	codeIntelRoot := b.Config.CodeIntel.Root
	if codeIntelRoot == "" {
		codeIntelRoot = b.Config.Sandbox.WorkDir
	}
	if codeIntelRoot == "" {
		codeIntelRoot, _ = os.Getwd()
	}
	if b.Config.CodeIntelEnabled() {
		svc := codeintel.NewService(codeIntelRoot, "", "")
		codeTools = codeintel.Toolbox(svc)
	} else if b.Config.CodeIntel.Required {
		return nil, cleanup, fmt.Errorf("codeintel.required but codeintel.enabled is false")
	}

	// The execution toolbox: builtins on the host, or proxied to a docker
	// sandbox.
	var execTools tool.Toolbox
	switch sandboxMode {
	case "host", "":
		execTools = tool.BuiltinsWith(tool.BuiltinOptions{
			FetchAllowPrivate: b.Config.Tools.Fetch.AllowPrivate,
			FileRoots:         fileRoots,
		})
	case "docker":
		home, err := config.Home()
		if err != nil {
			return nil, cleanup, err
		}
		sandboxID, err := id.NewBytes(4)
		if err != nil {
			return nil, cleanup, fmt.Errorf("new sandbox id: %w", err)
		}
		executor, err := sandbox.StartDocker(ctx, sandbox.DockerOpts{
			Image:             b.Config.Sandbox.Image,
			Network:           b.Config.Sandbox.Network,
			Memory:            b.Config.Sandbox.Memory,
			CPUs:              b.Config.Sandbox.CPUs,
			PIDs:              b.Config.Sandbox.PIDs,
			Disk:              b.Config.Sandbox.Disk,
			WorkDir:           b.Config.Sandbox.WorkDir,
			QueueDir:          filepath.Join(home, "sandboxes", sandboxID),
			SelfPath:          b.Config.Sandbox.RunnerBinary,
			FetchAllowPrivate: b.Config.Tools.Fetch.AllowPrivate,
		})
		if err != nil {
			return nil, cleanup, fmt.Errorf("start sandbox: %w", err)
		}
		closers = append(closers, func(cleanupCtx context.Context) error {
			if _, hasDeadline := cleanupCtx.Deadline(); !hasDeadline {
				return executor.Close()
			}
			return executor.CloseContext(cleanupCtx)
		})
		execTools = executor
	default:
		return nil, cleanup, fmt.Errorf("unknown sandbox mode %q for agent group %q (want \"host\" or \"docker\")", sandboxMode, group)
	}

	boxes := []tool.Toolbox{execTools, hostTools}
	// MCP before codeintel fallback so a real language server wins on name clash.
	// MCP servers contribute their tools (the long tail). All launches use the
	// #77 restricted executor (ConnectRestricted / BuildProcessEnv) — never
	// ambient gateway env. execution=sandbox docker-wraps when the agent group
	// is docker mode; host-mode groups use ConnectRestricted with work dir.
	workDir := b.Config.Sandbox.WorkDir
	if workDir == "" {
		workDir = codeIntelRoot
	}
	for _, s := range b.Config.MCP {
		if !ServerInGroup(s, group) || !ServerPermitted(s, toolPolicy) {
			continue
		}
		execution := s.Execution
		if execution == "" {
			execution = "host"
		}
		isCodeIntel := strings.HasPrefix(strings.ToLower(s.Name), "codeintel") || mcpDeclaresCodeIntel(s.Tools)

		// Host execution (including empty default) on a docker agent group
		// requires explicit opt-in: execution must be the literal "host" and
		// groups must include this group. Sandbox execution is the intended
		// path for docker groups (#77 / #79).
		if execution == "host" {
			if isCodeIntel && !b.Config.CodeIntel.AllowHostMCP {
				return nil, cleanup, fmt.Errorf("mcp %q: code-intelligence host launch requires [codeintel] allow_host_mcp = true", s.Name)
			}
			if sandboxMode == "docker" && (s.Execution != "host" || !slices.Contains(s.Groups, group)) {
				return nil, cleanup, fmt.Errorf("mcp %q: docker group %q requires explicit host opt-in (execution = \"host\" and groups includes %q)", s.Name, group, group)
			}
		}

		srv := mcp.Server{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env}
		network := b.Config.Sandbox.Network
		if network == "" {
			network = "none"
		}
		launch, ropts := mcp.PlanLaunch(srv, execution, sandboxMode, workDir, b.Config.Sandbox.Image, network)

		client, err := mcp.ConnectRestricted(ctx, launch, ropts)
		if err != nil {
			// Codeintel MCP is optional unless required: degrade to go/parser
			// fallback already registered above. Other servers fail closed.
			if isCodeIntel && !b.Config.CodeIntel.Required {
				continue
			}
			return nil, cleanup, fmt.Errorf("mcp %q: %w", s.Name, err)
		}
		closers = append(closers, func(cleanupCtx context.Context) error {
			if _, hasDeadline := cleanupCtx.Deadline(); !hasDeadline {
				return client.Close()
			}
			return client.CloseContext(cleanupCtx)
		})
		tb, err := client.Toolbox(ctx)
		if err != nil {
			_ = client.Close()
			if isCodeIntel && !b.Config.CodeIntel.Required {
				continue
			}
			return nil, cleanup, fmt.Errorf("mcp %q tools: %w", s.Name, err)
		}
		boxes = append(boxes, tb)
	}
	if codeTools != nil {
		boxes = append(boxes, codeTools)
	}

	// Model selection: default | utility | explicit (#71).
	model, err := resolveRuntimeProfileModel(b.Config, profile)
	if err != nil {
		return nil, cleanup, err
	}
	resolvedModel, err := b.Runtime.Resolve(model)
	if err != nil {
		return nil, cleanup, err
	}
	maxTokens := resolvedModel.MaxTokens
	if profile.MaxTokens > 0 {
		maxTokens = profile.MaxTokens
	}
	maxIter := 0
	if profile.MaxIterations > 0 {
		maxIter = profile.MaxIterations
	}

	usageStore := usagepkg.New(&store.Store{DB: b.Sessions.DB()})
	limits := func() usagepkg.Limits {
		l := b.Config.LimitsFor(group)
		return usagepkg.Limits{TokensPerDay: l.TokensPerDay, RequestsPerHour: l.RequestsPerHour, AlertThresholdPercent: l.AlertThresholdPercent}
	}()

	// Subagents get the execution + MCP tools, but not the ability to
	// spawn further subagents (their toolbox omits spawn_subagent).
	// Working-set broadcast is filled per-run when the parent has entries (#68).
	if b.Config.Agent.Subagents {
		subTools := tool.Restrict(tool.Combine(boxes...), toolPolicy)
		sub := agent.SubagentTool{
			Provider:            b.Runtime,
			Tools:               subTools,
			Model:               model,
			MaxTokens:           maxTokens,
			Redact:              b.Runtime.Redact,
			BroadcastWorkingSet: true,
			WorkingSetBroadcast: "", // filled below if non-empty at build time; runtime inject via note
			Usage:               usageStore,
			Limits:              limits,
			Log:                 slog.Default(),
		}
		// Pointer wrapper freezes working-set broadcast across parallel
		// spawn_subagent calls in one turn (#68).
		sub.Spill = spillStore
		boxes = append(boxes, tool.NewRegistry(&workingSetSubagent{
			inner:      sub,
			store:      wsStore,
			spill:      spillStore,
			sessions:   b.Sessions,
			cfg:        b.Config,
			parentDeny: append([]string{}, toolPolicy.Deny...),
			// Parent profile's allowed_children gates spawn profile= (#71).
			allowedChildren: append([]string{}, profile.AllowedChildren...),
		}))
	}

	toolbox := tool.Restrict(tool.Combine(boxes...), toolPolicy)

	sys, err := systemPrompt(ws, b.Skills)
	if err != nil {
		return nil, cleanup, err
	}
	if profile.System != "" {
		extra, err := loadProfileSystem(profile.System)
		if err != nil {
			return nil, cleanup, err
		}
		if extra != "" {
			sys = sys + "\n\n" + extra
		}
	}
	return &agent.Agent{
		Provider:      b.Runtime,
		Tools:         toolbox,
		System:        sys,
		Model:         model,
		UtilityModel:  runtimeUtilityModel(b.Config),
		Profile:       effectiveProfile,
		MaxTokens:     maxTokens,
		MaxIterations: maxIter,
		Redact:        b.Runtime.Redact,
		Spill:         spillStore,
		Usage:         usageStore,
		Limits:        limits,
		Log:           slog.Default(),
	}, cleanup, nil
}

// Cleanup is a context-bounded teardown step; Stop adapts it to the
// func() error the Agent lifecycle expects.
type Cleanup func(context.Context) error

// Stop runs the cleanup with a background context and returns its error.
func (c Cleanup) Stop() error {
	if c == nil {
		return nil
	}
	return c(context.Background())
}
