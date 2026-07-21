package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"

	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/hooks"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workspace"
)

func wsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	if len(args) == 0 {
		wsUsage(stderr)
		return errUsage
	}
	if args[0] == "open" {
		// Parse before checking process ownership so malformed invocations have
		// no filesystem side effects. Then refuse before opening the SQLite store
		// or starting Docker: broker tokens belong to the serve process (#48).
		if _, _, parseErr := parseWorkspaceOpenArgs(args[1:]); parseErr != nil {
			return parseErr
		}
		if lockErr := refuseWorkspaceOpenWhileServing(); lockErr != nil {
			return lockErr
		}
	}
	var closeID string
	var closeForce bool
	switch args[0] {
	case "close", "rm", "remove":
		closeID, closeForce, err = parseWorkspaceCloseArgs(args[1:])
		if err != nil {
			return err
		}
	}

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); err == nil {
			err = cerr
		}
	}()

	switch args[0] {
	case "ls", "list":
		mgr := newWorkspaceManager(cfg, st, nil)
		list, listErr := mgr.List(ctx)
		if listErr != nil {
			return listErr
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "no workspaces — open one with: waffle ws open <owner/repo>")
			return nil
		}
		for _, ws := range list {
			fmt.Fprintf(stdout, "%s  %-6s %-30s image=%s session=%s\n", ws.ID, ws.Status, ws.Repo, ws.Image, ws.SessionID)
		}
		return nil

	case "open":
		repoArg, profile, openParseErr := parseWorkspaceOpenArgs(args[1:])
		if openParseErr != nil {
			return openParseErr
		}
		if profile != "" {
			if !config.ValidProfileName(profile) && profile != "main" {
				return fmt.Errorf("invalid profile name %q", profile)
			}
			if _, ok := cfg.Profile(profile); !ok {
				return fmt.Errorf("unknown agent profile %q", profile)
			}
		}
		b, brokerURL, brokerErr := startWorkspaceBroker(ctx, cfg, st, stderr)
		if brokerErr != nil {
			return brokerErr
		}
		mgr := newWorkspaceManager(cfg, st, b)
		mgr.BrokerURL = brokerURL
		// allowlist and none: point HTTP(S)_PROXY at the broker so proxy-aware
		// clients cannot reach the wider internet without an allowlist entry.
		// none uses an empty broker allowlist (deny-all); allowlist is preloaded.
		// full leaves ProxyURL unset for unrestricted egress (#95).
		switch cfg.Workspace.Egress {
		case "allowlist", "none", "":
			mgr.ProxyURL = brokerURL + "/egress"
		}
		ws, client, openErr := mgr.OpenWithProfile(ctx, repoArg, profile)
		if openErr != nil {
			return openErr
		}
		defer func() {
			if cerr := client.Close(); err == nil {
				err = cerr
			}
		}()
		fmt.Fprintf(stdout, "workspace %s open: %s cloned in container %s (image %s)", ws.ID, ws.Repo, ws.Container, ws.Image)
		if ws.Profile != "" {
			fmt.Fprintf(stdout, " profile=%s", ws.Profile)
		}
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "work on it from chat with: /repo %s\n", ws.Repo)
		return nil

	case "idle":
		if len(args) != 2 {
			return fmt.Errorf("usage: waffle ws idle <id>")
		}
		mgr := newWorkspaceManager(cfg, st, nil)
		if idleErr := mgr.Idle(ctx, args[1]); idleErr != nil {
			return idleErr
		}
		fmt.Fprintf(stdout, "workspace %s stopped; volume kept\n", args[1])
		return nil

	case "close", "rm", "remove":
		mgr := newWorkspaceManager(cfg, st, nil)
		report, closeErr := mgr.Close(ctx, closeID, closeForce)
		if closeErr != nil {
			if report != nil && (report.Dirty != "" || report.Unpushed != "") {
				fmt.Fprintf(stderr, "dirty files:\n%s\nunpushed commits:\n%s\n", report.Dirty, report.Unpushed)
			}
			return closeErr
		}
		fmt.Fprintf(stdout, "workspace %s closed\n", closeID)
		return nil

	case "help", "-h", "--help":
		wsUsage(stdout)
		return nil
	default:
		wsUsage(stderr)
		return fmt.Errorf("unknown ws command %q", args[0])
	}
}

func parseWorkspaceCloseArgs(args []string) (id string, force bool, err error) {
	for _, arg := range args {
		switch {
		case arg == "--force":
			force = true
		case strings.HasPrefix(arg, "-"):
			return "", false, fmt.Errorf("unknown flag %q\nusage: waffle ws close|rm|remove <id> [--force]", arg)
		case id != "":
			return "", false, fmt.Errorf("expected one workspace id, got %q and %q\nusage: waffle ws close|rm|remove <id> [--force]", id, arg)
		default:
			id = arg
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("usage: waffle ws close|rm|remove <id> [--force]")
	}
	return id, force, nil
}

// parseWorkspaceOpenArgs parses: waffle ws open <owner/repo> [--profile name]
func parseWorkspaceOpenArgs(args []string) (repo, profile string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--profile":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("usage: waffle ws open <owner/repo> [--profile name]")
			}
			i++
			profile = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--profile="):
			profile = strings.TrimSpace(strings.TrimPrefix(arg, "--profile="))
		case strings.HasPrefix(arg, "-"):
			return "", "", fmt.Errorf("unknown flag %q\nusage: waffle ws open <owner/repo> [--profile name]", arg)
		case repo != "":
			return "", "", fmt.Errorf("expected one repo, got %q and %q\nusage: waffle ws open <owner/repo> [--profile name]", repo, arg)
		default:
			repo = arg
		}
	}
	if repo == "" {
		return "", "", fmt.Errorf("usage: waffle ws open <owner/repo> [--profile name]")
	}
	return repo, profile, nil
}

func wsUsage(w io.Writer) {
	fmt.Fprint(w, `Repo workspaces: a container per repository, git auth via the broker.

Usage:
  waffle ws open <owner/repo> [--profile name]
                                 clone the repo into a fresh container
  waffle ws ls|list              list workspaces
  waffle ws idle <id>            stop the container, keep the volume
  waffle ws close|rm|remove <id> [--force]
                                 tear down (refuses if work is unpushed)
`)
}

func newWorkspaceManager(cfg config.Config, st *store.Store, b *broker.Broker) *workspace.Manager {
	sessions := session.New(st)
	home, _ := config.Home()
	mgr := workspace.NewManager(st, sessions, workspace.DockerRuntime{}, filepath.Join(home, "sandboxes"))
	if cfg.Sandbox.Image != "" {
		mgr.DefaultImage = cfg.Sandbox.Image
	}
	mgr.Network = cfg.Sandbox.Network
	if cfg.Workspace.Egress == "full" {
		mgr.Network = "bridge"
	}
	mgr.Egress = cfg.Workspace.Egress
	mgr.EgressAllowlist = append([]string(nil), cfg.Workspace.Allowlist...)
	mgr.RunnerBinary = cfg.Sandbox.RunnerBinary
	mgr.Memory = cfg.Sandbox.Memory
	mgr.CPUs = cfg.Sandbox.CPUs
	mgr.PIDs = cfg.Sandbox.PIDs
	mgr.Disk = cfg.Sandbox.Disk
	mgr.Hooks = workspaceHooksFromConfig(cfg)
	if cfg.Workspace.IdleTimeout != "" {
		if d, err := config.ParseDuration(cfg.Workspace.IdleTimeout); err == nil {
			mgr.IdleTimeout = d
		}
	}
	if b != nil {
		limits := brokerLimits(cfg, config.GroupMain)
		mgr.MintToken = func(ctx context.Context, sessionID string) (string, error) {
			return b.MintScoped(ctx, sessionID, sessionID, limits)
		}
		mgr.RevokeSession = b.RevokeSession
		mgr.BindGitScope = b.BindGitRepo
		// none: allow the repo's git host through broker egress so clone works
		// via HTTP_PROXY while other hosts stay denied (#95).
		if cfg.Workspace.Egress == "none" || cfg.Workspace.Egress == "" {
			mgr.AllowGitHost = func(host string) {
				if host == "" {
					return
				}
				b.SetEgress([]broker.EgressTarget{{Host: host, BaseURL: "https://" + host}})
			}
		}
	}
	return mgr
}

func brokerLimits(cfg config.Config, group string) usagepkg.Limits {
	l := cfg.LimitsFor(group)
	return usagepkg.Limits{TokensPerDay: l.TokensPerDay, RequestsPerHour: l.RequestsPerHour, AlertThresholdPercent: l.AlertThresholdPercent}
}

func workspaceHooksFromConfig(cfg config.Config) hooks.Config {
	h := hooks.Config{
		AfterCreate:  cfg.Workspace.Hooks.AfterCreate,
		BeforeRun:    cfg.Workspace.Hooks.BeforeRun,
		AfterRun:     cfg.Workspace.Hooks.AfterRun,
		BeforeRemove: cfg.Workspace.Hooks.BeforeRemove,
	}
	if cfg.Workspace.Hooks.Timeout != "" {
		if d, err := config.ParseDuration(cfg.Workspace.Hooks.Timeout); err == nil {
			h.Timeout = d
		}
	}
	return h
}

// startWorkspaceBroker runs the credential broker for workspace git auth
// and returns the URL containers use to reach it.
func startWorkspaceBroker(ctx context.Context, cfg config.Config, st *store.Store, stderr io.Writer) (*broker.Broker, string, error) {
	listen := cfg.Broker.Listen
	if listen == "" {
		return nil, "", fmt.Errorf("workspaces need the credential broker: set [broker] listen in config.toml (e.g. \"127.0.0.1:8421\")")
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, "", fmt.Errorf("bad [broker] listen %q: %w", listen, err)
	}
	// Bind synchronously: if `waffle serve` already holds this address the
	// bind fails here, before any container/volume is created — rather than
	// failing in a background goroutine while the workspace proceeds against
	// a broker URL nothing in this process is serving (#48).
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, "", fmt.Errorf(
			"credential broker cannot bind %s: %w\nis `waffle serve` already running? work on the repo through the gateway (/repo) instead",
			listen, err)
	}

	b := broker.New(st, brokerUpstreams(cfg))
	// allowlist: configured hosts. none: empty here; Open adds the repo host
	// so git clone can use the proxy while other hosts stay denied (#95).
	if cfg.Workspace.Egress == "allowlist" {
		targets := make([]broker.EgressTarget, 0, len(cfg.Workspace.Allowlist))
		for _, host := range cfg.Workspace.Allowlist {
			targets = append(targets, broker.EgressTarget{Host: host, BaseURL: "https://" + host})
		}
		b.SetEgress(targets)
	}
	// Scope git credentials to the repo the requesting session opened.
	mgr := newWorkspaceManager(cfg, st, nil)
	scope := repoScopeResolver(b, mgr)
	if cfg.GitHub.App.PrivateKey != "" {
		if !strings.HasPrefix(cfg.GitHub.App.PrivateKey, "secret://") {
			_ = ln.Close()
			return nil, "", fmt.Errorf("github app private_key must be a secret:// reference")
		}
		key, err := resolveSecretValue(cfg.GitHub.App.PrivateKey, "")
		if err != nil {
			_ = ln.Close()
			return nil, "", fmt.Errorf("github app private key: %w", err)
		}
		app, err := gitcred.NewApp(cfg.GitHub.App.AppID, cfg.GitHub.App.InstallationID, []byte(key), cfg.GitHub.App.BaseURL, nil, nil)
		if err != nil {
			_ = ln.Close()
			return nil, "", err
		}
		b.GitBackend = "github-app"
		b.GitCredential = gitCredentialFromApp(scope, app)
	} else {
		b.GitBackend = "pat"
		b.GitCredential = gitCredentialFromSecrets(scope)
	}
	go func() {
		if err := b.ServeListener(ctx, ln); err != nil {
			fmt.Fprintf(stderr, "waffle: broker: %v\n", err)
		}
	}()

	// Containers reach the host through the host-gateway alias set up by
	// the runtime (--add-host waffle-host:host-gateway).
	return b, "http://waffle-host:" + port, nil
}

func gitCredentialFromApp(repoForSession func(context.Context, string) (string, error), app *gitcred.App) broker.GitCredentialFunc {
	return func(ctx context.Context, sessionID, host, path string) (string, string, error) {
		if host != "github.com" {
			return "", "", fmt.Errorf("no credentials for host %q", host)
		}
		bound, err := repoForSession(ctx, sessionID)
		if err != nil {
			return "", "", fmt.Errorf("session is not bound to a repo workspace; refusing git credentials: %w", err)
		}
		if canonRepoPath(path) != canonRepoPath(bound) {
			return "", "", fmt.Errorf("session is scoped to %q; refusing credentials for %q", bound, path)
		}
		return app.Credential(ctx, canonRepoPath(bound))
	}
}

// repoScopeResolver resolves a session to the repo its git credentials are
// scoped to. It checks the broker's in-memory binding first — set at
// workspace-open time, this covers the initial `git clone`, which runs before
// the durable workspaces row is written — then falls back to the workspaces
// table, which covers resumed and steady-state sessions (and survives a broker
// restart). A session with neither is unbound and gets no credential.
func repoScopeResolver(b *broker.Broker, mgr *workspace.Manager) func(ctx context.Context, sessionID string) (string, error) {
	return func(ctx context.Context, sessionID string) (string, error) {
		if repo, ok := b.GitRepoScope(sessionID); ok {
			return repo, nil
		}
		ws, err := mgr.ForSession(ctx, sessionID)
		if err != nil {
			return "", err
		}
		return ws.Repo, nil
	}
}

// gitCredentialFromSecrets serves the stored fine-grained PAT
// (secret://github/token, or GITHUB_TOKEN), scoped to the repo the requesting
// session opened. repoForSession resolves a session to its bound repo;
// deny-by-default when a session has no workspace binding or requests a repo
// other than its own — a compromised session must not be able to pull
// another repo's credential (docs/plan.md threat model). A GitHub App minting
// short-lived installation tokens replaces the PAT behind the same signature
// (#40) and narrows the credential at GitHub's side too.
func gitCredentialFromSecrets(repoForSession func(ctx context.Context, sessionID string) (string, error)) broker.GitCredentialFunc {
	return func(ctx context.Context, sessionID, host, path string) (string, string, error) {
		if host != "github.com" {
			return "", "", fmt.Errorf("no credentials for host %q", host)
		}
		boundRepo, err := repoForSession(ctx, sessionID)
		if err != nil {
			return "", "", fmt.Errorf("session is not bound to a repo workspace; refusing git credentials: %w", err)
		}
		if canonRepoPath(path) != canonRepoPath(boundRepo) {
			return "", "", fmt.Errorf("session is scoped to %q; refusing credentials for %q", boundRepo, path)
		}
		token, err := resolveSecretValue("secret://github/token", "GITHUB_TOKEN")
		if err != nil {
			return "", "", err
		}
		if token == "" {
			return "", "", fmt.Errorf("no github token: store one with `waffle secret set github/token` or set GITHUB_TOKEN")
		}
		return "x-access-token", token, nil
	}
}

// canonRepoPath normalizes an owner/repo identifier for scope comparison:
// git's credential path arrives as "owner/repo.git" (optionally slash-
// wrapped), while the stored binding is "owner/repo". GitHub treats owner and
// repo case-insensitively, so fold case to avoid a trivial "Owner/Repo"
// bypass.
func canonRepoPath(p string) string {
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	return strings.ToLower(p)
}
