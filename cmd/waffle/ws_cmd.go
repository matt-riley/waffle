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
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/workspace"
)

func wsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		wsUsage(stderr)
		return errUsage
	}

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // process is exiting

	switch args[0] {
	case "ls":
		mgr := newWorkspaceManager(cfg, st, nil)
		list, err := mgr.List(ctx)
		if err != nil {
			return err
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
		if len(args) != 2 {
			return fmt.Errorf("usage: waffle ws open <owner/repo>")
		}
		b, brokerURL, err := startWorkspaceBroker(ctx, cfg, st, stderr)
		if err != nil {
			return err
		}
		mgr := newWorkspaceManager(cfg, st, b)
		mgr.BrokerURL = brokerURL
		ws, client, err := mgr.Open(ctx, args[1])
		if err != nil {
			return err
		}
		defer client.Close() //nolint:errcheck // process is exiting
		fmt.Fprintf(stdout, "workspace %s open: %s cloned in container %s (image %s)\n", ws.ID, ws.Repo, ws.Container, ws.Image)
		fmt.Fprintf(stdout, "work on it from chat with: /repo %s\n", ws.Repo)
		return nil

	case "idle":
		if len(args) != 2 {
			return fmt.Errorf("usage: waffle ws idle <id>")
		}
		mgr := newWorkspaceManager(cfg, st, nil)
		if err := mgr.Idle(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "workspace %s stopped; volume kept\n", args[1])
		return nil

	case "close":
		force := false
		id := ""
		for _, a := range args[1:] {
			if a == "--force" {
				force = true
			} else {
				id = a
			}
		}
		if id == "" {
			return fmt.Errorf("usage: waffle ws close <id> [--force]")
		}
		mgr := newWorkspaceManager(cfg, st, nil)
		report, err := mgr.Close(ctx, id, force)
		if err != nil {
			if report != nil && (report.Dirty != "" || report.Unpushed != "") {
				fmt.Fprintf(stderr, "dirty files:\n%s\nunpushed commits:\n%s\n", report.Dirty, report.Unpushed)
			}
			return err
		}
		fmt.Fprintf(stdout, "workspace %s closed\n", id)
		return nil

	case "help", "-h", "--help":
		wsUsage(stdout)
		return nil
	default:
		wsUsage(stderr)
		return fmt.Errorf("unknown ws command %q", args[0])
	}
}

func wsUsage(w io.Writer) {
	fmt.Fprint(w, `Repo workspaces: a container per repository, git auth via the broker.

Usage:
  waffle ws open <owner/repo>    clone the repo into a fresh container
  waffle ws ls                   list workspaces
  waffle ws idle <id>            stop the container, keep the volume
  waffle ws close <id> [--force] tear down (refuses if work is unpushed)
`)
}

func newWorkspaceManager(cfg config.Config, st *store.Store, b *broker.Broker) *workspace.Manager {
	sessions := session.New(st)
	home, _ := config.Home()
	mgr := workspace.NewManager(st, sessions, workspace.DockerRuntime{}, filepath.Join(home, "sandboxes"))
	if cfg.Sandbox.Image != "" {
		mgr.DefaultImage = cfg.Sandbox.Image
	}
	if b != nil {
		mgr.MintToken = func(ctx context.Context, sessionID string) (string, error) { return b.Mint(ctx, sessionID) }
		mgr.RevokeSession = b.RevokeSession
		mgr.BindGitScope = b.BindGitRepo
	}
	return mgr
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
	// Scope git credentials to the repo the requesting session opened.
	mgr := newWorkspaceManager(cfg, st, nil)
	b.GitCredential = gitCredentialFromSecrets(repoScopeResolver(b, mgr))
	go func() {
		if err := b.ServeListener(ctx, ln); err != nil {
			fmt.Fprintf(stderr, "waffle: broker: %v\n", err)
		}
	}()

	// Containers reach the host through the host-gateway alias set up by
	// the runtime (--add-host waffle-host:host-gateway).
	return b, "http://waffle-host:" + port, nil
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
