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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/apiface"
	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/codeintel"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/notify"
	"github.com/matt-riley/waffle/internal/plugin"
	"github.com/matt-riley/waffle/internal/policy"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/secret"
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
	// Secrets is the secret store for remote MCP credentials (#249):
	// static token refs resolve through it, OAuth tokens (from `waffle mcp
	// login`) are read and refreshed through it, and its values feed the
	// transcript redactor. Nil means no store: remote MCP servers that
	// reference credentials fail closed at build.
	Secrets secret.Store
	// RemoteEgress, when set, lets remote MCP traffic from docker-mode
	// groups traverse the gateway broker's egress proxy (allowlist + audit
	// rows). Nil means broker egress is unavailable and docker-mode groups
	// cannot use remote MCP servers (#249).
	RemoteEgress *mcp.RemoteEgress
	// Broker, when non-nil, enables per-face API tools (#254): each
	// configured face whose tool name the effective tool policy explicitly
	// allows becomes an api_<name> host tool that mints a short-lived
	// session-scoped broker token per call. Nil disables the tools.
	Broker *broker.Broker
	// BrokerURL is the address host-side tools use to reach Broker
	// (e.g. "http://127.0.0.1:8421"). Required when Broker is set.
	BrokerURL string
	// APIFaces are the configured credentialed faces, metadata only — no
	// credential values ever enter the agent.
	APIFaces []apiface.Face
	// APIRedact, when set, scrubs credential material from API tool output
	// and errors; nil falls back to Runtime.Redact.
	APIRedact func(string) string
	// Search, when set, describes the effective web_search provider (#245).
	// The tool is offered only when a broker is wired and the effective tool
	// policy permits "web_search"; absent Search disables it.
	Search *tool.WebSearchSpec
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
		// notify (#253) runs on the host: the owner-channel adapter exists
		// only host-side. Tier availability is policy-controlled (see
		// config.AgentPolicy): cron/issue keep it, group denies it by
		// default. Sessions without a channel origin (terminal chat, eval)
		// no-op it — the gateway is the only place a sender is attached.
		notify.Tool{Log: slog.Default()},
		memory.RememberTool{WS: ws, Notes: notesIdx, Gate: &memory.Gate{Mode: b.Config.Memory.WriteGate, WS: ws}, Provenance: memory.Provenance{TrustClass: "owner_stated"}},
		// Model-invoked memory updates cross the same gate as remember (#417).
		// No owner_stated claim here: provenance is derived from the run context
		// and defaults to model_derived unless the context is untrusted.
		memory.MemoryUpdateTool{WS: ws, Notes: notesIdx, Gate: &memory.Gate{Mode: b.Config.Memory.WriteGate, WS: ws}},
		memory.RecallTool{Sessions: b.Sessions, WS: ws, Notes: notesIdx, Spills: spillStore},
		workset.UpdateTool{Store: wsStore},
		spill.ExpandTool{Store: spillStore},
		session.ExpandContextTool{Sessions: b.Sessions},
	}
	if b.Config.Agent.Learn {
		hostToolList = append(hostToolList, memory.DistillTool{WS: ws, Gate: &memory.Gate{Mode: b.Config.Memory.WriteGate, WS: ws}})
	}
	// The GitHub tools (github_pr and the #252 read/comment surface) need
	// permissions the workspace git credential deliberately never carries —
	// anything inside a container can read its credentials back out with `git
	// credential fill`. So they all run on the host: each mints a token for one
	// call carrying only the permission it needs, scopes it to the repo the
	// session's workspace is bound to, and never lets it near a container. A
	// missing or misconfigured app is not an error here: the tools are simply
	// not offered.
	if b.GitHubApp != nil {
		if app, appErr := b.GitHubApp(); appErr == nil && app != nil {
			bindings := &workspace.Manager{DB: b.Sessions.DB()}
			repoForSession := func(ctx context.Context, sessionID string) (string, error) {
				// Reuse the manager's own lookup rather than repeating the
				// query: this is the same binding the broker scopes git
				// credentials by, and a second copy could drift from it.
				bound, err := bindings.ForSession(ctx, sessionID)
				if err != nil {
					return "", err
				}
				return bound.Repo, nil
			}
			host := gitcred.HostTool{App: app, Repo: repoForSession}
			hostToolList = append(hostToolList,
				gitcred.PullRequestTool{App: app, Repo: repoForSession},
				gitcred.PRGetTool{HostTool: host},
				gitcred.PRDiffTool{HostTool: host},
				gitcred.PRCommentsTool{HostTool: host},
				gitcred.CommentTool{HostTool: host},
				gitcred.ChecksTool{HostTool: host},
				gitcred.IssueGetTool{HostTool: host},
			)
		}
	}
	// Per-face API tools (#254): offered only to builds whose effective
	// tool policy explicitly allows the api_<name> tool (a literal allow
	// entry; the "*" wildcard does not grant faces). The same policy
	// decision drives the broker token grants, so the model-facing toolbox
	// and the broker's per-session enforcement cannot drift.
	//
	// tierLimits is initialized for any broker (not just when faces exist)
	// because web_search mints scoped tokens through the same budget and must
	// stay metered even when no [[api.upstream]] face is configured (#387
	// review): a zero limits value would disable usage.Check's caps entirely.
	var tierLimits usagepkg.Limits
	if b.Broker != nil {
		tierLimits = usagepkg.Limits{
			TokensPerDay:          b.Config.LimitsFor(group).TokensPerDay,
			RequestsPerHour:       b.Config.LimitsFor(group).RequestsPerHour,
			AlertThresholdPercent: b.Config.LimitsFor(group).AlertThresholdPercent,
			TunnelBytesPerSession: b.Config.LimitsFor(group).TunnelBytesPerSession,
		}
	}
	if b.Broker != nil && len(b.APIFaces) > 0 {
		redact := b.APIRedact
		if redact == nil {
			redact = b.Runtime.Redact
		}
		client := &apiface.Client{
			Faces:     b.APIFaces,
			BrokerURL: b.BrokerURL,
			Mint: func(ctx context.Context, sessionID string, faces []string) (string, error) {
				return b.Broker.MintScopedFaces(ctx, sessionID, sessionID, tierLimits, faces)
			},
			Revoke: b.Broker.Revoke,
			Redact: redact,
		}
		hostToolList = append(hostToolList, client.ToolsFor(toolPolicy)...)
	}

	// web_search (#245): a host-side tool routed through the broker's
	// credentialed API faces, offered only when [search] config exists, a
	// broker is wired, and the effective policy permits the tool. Unlike the
	// api_<name> faces, the normal allow semantics apply (an empty allow list
	// or "*" grants it for the main tier); the restricted tiers deny it by
	// default in config.AgentPolicy and opt in explicitly.
	if b.Search != nil && b.Broker != nil && b.BrokerURL != "" && toolPolicy.Permits("web_search") {
		hostToolList = append(hostToolList, &tool.WebSearch{
			Type:       b.Search.Type,
			Face:       b.Search.Face,
			MaxResults: b.Search.MaxResults,
			BrokerURL:  b.BrokerURL,
			Mint: func(ctx context.Context, sessionID string, faces []string) (string, error) {
				return b.Broker.MintScopedFaces(ctx, sessionID, sessionID, tierLimits, faces)
			},
			Revoke:    b.Broker.Revoke,
			SessionID: session.IDFromContext,
		})
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
			BashPIDs:          b.Config.Sandbox.PIDs,
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

	// Remote MCP servers contribute their credentials to the transcript
	// redactor: a tool result echoing a token must not reach the model.
	var mcpRedactors []func(string) string

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
		if s.URL != "" {
			if !RemoteServerInGroup(s, group) {
				// Unattended tiers are deny-by-default for remote servers
				// (#249): availability requires the server to name the
				// group explicitly.
				continue
			}
			tb, closer, redact, err := b.connectRemoteMCP(ctx, s, group, sandboxMode, nil)
			if err != nil {
				return nil, cleanup, fmt.Errorf("mcp %q: %w", s.Name, err)
			}
			closers = append(closers, closer)
			if redact != nil {
				mcpRedactors = append(mcpRedactors, redact)
			}
			boxes = append(boxes, tb)
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

		srv := mcp.Server{Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env, Optional: isCodeIntel}
		network := b.Config.Sandbox.Network
		if network == "" {
			network = "none"
		}
		launch, ropts := mcp.PlanLaunch(srv, execution, sandboxMode, workDir, b.Config.Sandbox.Image, network)

		client, err := mcp.ConnectRestricted(ctx, launch, ropts)
		if err != nil {
			// Codeintel MCP is optional unless required: degrade to go/parser
			// fallback already registered above. Other servers fail closed.
			if srv.Optional && !b.Config.CodeIntel.Required {
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
			if srv.Optional && !b.Config.CodeIntel.Required {
				continue
			}
			return nil, cleanup, fmt.Errorf("mcp %q tools: %w", s.Name, err)
		}
		boxes = append(boxes, tb)
	}

	// Plugin components (#393): every installed plugin contributes its
	// skills and MCP servers with spec §11.3 failure isolation — a rejected
	// plugin, a disabled component type, or a failed server is skipped and
	// reported, never fatal to the build. Native [[mcp]] servers above keep
	// fail-fast (operator config errors stay loud); plugin data is
	// untrusted package content and degrades per-entry.
	var pluginSkills []skill.Skill
	if home, herr := config.Home(); herr != nil {
		slog.Warn("plugin components: resolve waffle home", "reason", herr)
	} else if installed, rejects, ierr := plugin.Installed(home); ierr != nil {
		slog.Warn("plugin components: enumerate installed plugins", "reason", ierr)
	} else {
		for _, reject := range rejects {
			slog.Warn("plugin rejected", "dir", reject.Dir, "reason", reject.Reason)
		}
		for _, result := range installed {
			pluginName := result.Plugin.Manifest.Name
			ext, extWarnings, _ := plugin.LoadWaffleExtension(result.Plugin.Manifest)
			for _, w := range extWarnings {
				slog.Warn("plugin waffle extension", "plugin", pluginName, "reason", w)
			}
			for _, skip := range result.SkillSkips {
				slog.Warn("plugin skill skipped", "plugin", pluginName, "skill", skip.Dir, "reason", skip.Reason)
			}
			for _, s := range result.Skills {
				pluginSkills = append(pluginSkills, skill.Skill{
					Name:        s.Name,
					Description: s.Description,
					Path:        s.Path,
				})
			}
			// Waffle-extension activation overrides for plugin skills; the
			// skill_status table still wins, and a failed override read is
			// fail-closed (deny-by-default, matching the workspace path).
			if len(ext.Skills) > 0 {
				overrides := make(map[string]string, len(ext.Skills))
				for name, policy := range ext.Skills {
					overrides[name] = policy.Status
				}
				filtered, err := skill.FilterActiveWithExtension(pluginSkills, b.Sessions.DB(), overrides)
				if err != nil {
					slog.Warn("plugin skill activation", "plugin", pluginName, "reason", err)
					pluginSkills = nil
				} else {
					pluginSkills = filtered
				}
			}
			if result.MCP.Disabled != "" {
				slog.Warn("plugin mcp disabled", "plugin", pluginName, "reason", result.MCP.Disabled)
			}
			for _, skip := range result.MCP.Skips {
				slog.Warn("plugin mcp server skipped", "plugin", pluginName, "server", skip.Name, "reason", skip.Reason)
			}
			for _, srv := range result.MCP.Servers {
				b.wirePluginMCPServer(ctx, &boxes, &closers, &mcpRedactors, result, srv, ext.MCP[srv.Name], home, sandboxMode, group)
			}
		}
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
		return usagepkg.Limits{TokensPerDay: l.TokensPerDay, RequestsPerHour: l.RequestsPerHour, AlertThresholdPercent: l.AlertThresholdPercent, TunnelBytesPerSession: l.TunnelBytesPerSession}
	}()

	// Remote MCP servers contribute their credentials to the transcript
	// redactor: a tool result echoing a token must not reach the model.
	redact := b.Runtime.Redact
	if len(mcpRedactors) > 0 {
		base := redact
		redact = func(s string) string {
			for _, r := range mcpRedactors {
				s = r(s)
			}
			return base(s)
		}
	}

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
			Redact:              redact,
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

	var sys string
	if len(pluginSkills) > 0 {
		skills := append(append([]skill.Skill{}, b.Skills...), pluginSkills...)
		sys, err = systemPrompt(ws, skills)
	} else {
		sys, err = systemPrompt(ws, b.Skills)
	}
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
		Redact:        redact,
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

// connectRemoteMCP launches one URL-based (remote) MCP server (#249):
// egress posture first (docker-mode groups go through the broker or are
// refused), then credential resolution (static secret:// token or OAuth
// tokens from `waffle mcp login`), then the streamable-HTTP handshake.
// headers carries fixed extra headers for plugin-sourced servers; nil for
// native config servers.
func (b *Builder) connectRemoteMCP(ctx context.Context, s config.MCPServer, group, sandboxMode string, headers http.Header) (tool.Toolbox, Cleanup, func(string) string, error) {
	// Egress posture. "broker" is the default for docker-mode groups: their
	// remote MCP traffic must traverse the broker (allowlist + audit) — an
	// unaudited direct side channel out of a sandboxed tier is refused.
	egress := s.Egress
	if egress == "" {
		if sandboxMode == "docker" {
			egress = "broker"
		} else {
			egress = "direct"
		}
	}
	var proxyAuth func() (string, error)
	switch egress {
	case "direct":
		if sandboxMode == "docker" {
			return nil, nil, nil, fmt.Errorf(
				"egress=direct is refused for docker-mode group %q (remote MCP would be an unaudited side channel out of the sandbox); use egress=\"broker\" or a host-mode group", group)
		}
	case "broker":
		if b.RemoteEgress == nil || b.RemoteEgress.ProxyURL == "" || b.RemoteEgress.MintToken == nil {
			return nil, nil, nil, fmt.Errorf(
				"egress=broker requires the gateway credential broker (start under `waffle serve` with [broker] listen); refused for group %q", group)
		}
		mint := b.RemoteEgress.MintToken
		proxyAuth = func() (string, error) {
			token, err := mint(ctx, group)
			if err != nil {
				return "", err
			}
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(token+":")), nil
		}
	default:
		return nil, nil, nil, fmt.Errorf("unknown egress %q (config validation should have rejected this)", egress)
	}

	// Credential resolution. Tokens live in internal/secret; config holds
	// references only.
	opts := mcp.HTTPOpts{
		ProxyURL:  egressURLFor(egress, b.RemoteEgress),
		ProxyAuth: proxyAuth,
		Headers:   headers,
	}
	if s.Token != "" {
		if b.Secrets == nil {
			return nil, nil, nil, errors.New("token references the secret store but none is available (run `waffle secret init`)")
		}
		value, err := secret.Resolve(b.Secrets, s.Token)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("token: %w", err)
		}
		opts.BearerToken = value
	} else if b.Secrets != nil {
		// Credential refresh is egress traffic too: build the token
		// client from the same proxy configuration as the MCP connection,
		// so refresh tokens traverse the broker allowlist and audit rows
		// instead of bypassing them on http.DefaultClient (#249).
		tm := &mcp.TokenManager{
			Store:  b.Secrets,
			Server: s.Name,
			HTTP:   mcp.NewTokenHTTPClient(opts.ProxyURL, proxyAuth),
		}
		if err := tm.Load(); err == nil {
			opts.Token = tm
		} else if !errors.Is(err, mcp.ErrNoToken) {
			return nil, nil, nil, fmt.Errorf("oauth: %w", err)
		}
	}

	client, err := mcp.ConnectHTTP(ctx, s.Name, s.URL, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	closer := Cleanup(func(cleanupCtx context.Context) error {
		return client.Close()
	})
	// The transcript redactor picks up every stored secret (including this
	// server's tokens) so a tool result echoing one never reaches the model.
	var redact func(string) string
	if b.Secrets != nil {
		r, err := secret.NewRedactorWith(b.Secrets)
		if err != nil {
			_ = client.Close()
			return nil, nil, nil, fmt.Errorf("redactor: %w", err)
		}
		redact = r.Redact
	}
	tb, err := client.Toolbox(ctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, nil, fmt.Errorf("tools: %w", err)
	}
	return tb, closer, redact, nil
}

// egressURLFor returns the broker egress proxy URL when egress is "broker",
// else empty (direct dialing).
func egressURLFor(egress string, e *mcp.RemoteEgress) string {
	if egress == "broker" && e != nil {
		return e.ProxyURL
	}
	return ""
}
