package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/store"
)

// TestStartWorkspaceBrokerFailsFastOnBusyAddress verifies the bind is
// synchronous: when the configured address is already held (the normal state
// when `waffle serve` is running), startWorkspaceBroker returns an error
// instead of proceeding with a broker URL nothing in this process serves (#48).
func TestStartWorkspaceBrokerFailsFastOnBusyAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Occupy an address to stand in for a running serve.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	}()
	addr := held.Addr().String()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	cfg := config.Config{}
	cfg.Broker.Listen = addr

	var stderr bytes.Buffer
	b, url, err := startWorkspaceBroker(ctx, cfg, st, &stderr)
	if err == nil {
		t.Fatal("startWorkspaceBroker succeeded on a busy address; want a bind error")
	}
	if b != nil || url != "" {
		t.Errorf("on failure want nil broker and empty url, got b=%v url=%q", b, url)
	}
}

// TestStartWorkspaceBrokerBindsFreeAddress is the happy path: a free address
// binds and the container-facing URL is returned.
func TestStartWorkspaceBrokerBindsFreeAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Find a free port, then release it for the broker to claim.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(probe.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	addr := net.JoinHostPort("127.0.0.1", port)

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	cfg := config.Config{}
	cfg.Broker.Listen = addr

	var stderr bytes.Buffer
	b, url, err := startWorkspaceBroker(ctx, cfg, st, &stderr)
	if err != nil {
		t.Fatalf("startWorkspaceBroker on a free address: %v", err)
	}
	if b == nil {
		t.Fatal("want a broker, got nil")
	}
	if want := "http://waffle-host:" + port; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}

// TestRepoScopeResolverPrefersBrokerBinding is the regression for the review
// finding: during the initial `git clone` the workspaces row does not exist
// yet, so the resolver must answer from the broker's mint-time binding. It
// then falls back to the workspaces table for resumed/steady-state sessions.
func TestRepoScopeResolverPrefersBrokerBinding(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	b := broker.New(st, nil)
	mgr := newWorkspaceManager(config.Config{}, st, nil)
	resolve := repoScopeResolver(b, mgr)

	// (1) In-memory binding, no workspaces row yet (the clone window).
	b.BindGitRepo("s-cloning", "owner/A")
	if repo, err := resolve(ctx, "s-cloning"); err != nil || repo != "owner/A" {
		t.Fatalf("clone-window resolve = (%q, %v), want (owner/A, nil)", repo, err)
	}

	// (2) No binding, but a durable workspaces row (resumed session).
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at) VALUES ('s-resumed', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at)
		VALUES ('ws-1', 'owner/B', 'u', 'img', 'c', 'v', 's-resumed', 'open', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if repo, err := resolve(ctx, "s-resumed"); err != nil || repo != "owner/B" {
		t.Fatalf("db-fallback resolve = (%q, %v), want (owner/B, nil)", repo, err)
	}

	// (3) Neither: unbound session is refused.
	if _, err := resolve(ctx, "s-unknown"); err == nil {
		t.Fatal("unbound session resolved without error")
	}
}

func TestWorkspaceManagerWiresGitHostForBrokeredEgress(t *testing.T) {
	cases := []struct {
		name      string
		egress    string
		allowlist []string
		want      bool
	}{
		{name: "default", egress: "", want: true},
		{name: "none", egress: "none", want: true},
		{name: "allowlist", egress: "allowlist", allowlist: []string{"github.com"}, want: true},
		{name: "full", egress: "full", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := st.Close(); err != nil {
					t.Errorf("close store: %v", err)
				}
			}()

			cfg := config.Config{}
			cfg.Workspace.Egress = tc.egress
			cfg.Workspace.Allowlist = tc.allowlist
			mgr := newWorkspaceManager(cfg, st, broker.New(st, nil))

			if got := mgr.AllowGitHost != nil; got != tc.want {
				t.Fatalf("AllowGitHost present = %v, want %v", got, tc.want)
			}
		})
	}
}
