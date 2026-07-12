package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/instance"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestWorkspaceOpenRefusesLiveServeBeforeMutation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[broker]\nlisten = \"127.0.0.1:9842\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := instance.Default(filepath.Join(home, "serve.lock")).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	var stdout, stderr bytes.Buffer
	err = run(context.Background(), []string{"ws", "open", "owner/repo"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("ws open succeeded while serve owner lock was held")
	}
	for _, want := range []string{"waffle serve", "127.0.0.1:9842", "gateway", "/repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(home, "waffle.db")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused ws open mutated database: %v", statErr)
	}
}

// TestWorkspaceCloseParseFailureStopsBeforeStoreMutation exercises the command
// boundary (run -> wsCmd), not just the parser. A rejected invocation returns
// an error (main maps that to exit 1) before WAFFLE_HOME or its database exists.
func TestWorkspaceCloseParseFailureStopsBeforeStoreMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("WAFFLE_HOME", home)
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"ws", "close", "a", "b"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("run accepted ambiguous workspace close; main would exit zero")
	}
	if !strings.Contains(err.Error(), `got "a" and "b"`) {
		t.Fatalf("error = %q, want both conflicting tokens", err)
	}
	if _, statErr := os.Stat(home); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("parse rejection mutated workspace state: stat %s = %v", home, statErr)
	}
}

func TestParseWorkspaceCloseArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantID    string
		wantForce bool
		wantErr   string
	}{
		{name: "one id", args: []string{"a"}, wantID: "a"},
		{name: "force after id", args: []string{"a", "--force"}, wantID: "a", wantForce: true},
		{name: "force before id", args: []string{"--force", "a"}, wantID: "a", wantForce: true},
		{name: "duplicate force", args: []string{"a", "--force", "--force"}, wantID: "a", wantForce: true},
		{name: "no id", args: nil, wantErr: "usage: waffle ws close <id> [--force]"},
		{name: "force without id", args: []string{"--force"}, wantErr: "usage: waffle ws close <id> [--force]"},
		{name: "extra id", args: []string{"a", "b"}, wantErr: "expected one workspace id, got \"a\" and \"b\""},
		{name: "unknown flag", args: []string{"a", "--froce"}, wantErr: "unknown flag \"--froce\""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, force, err := parseWorkspaceCloseArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseWorkspaceCloseArgs(%q) error = %v, want %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWorkspaceCloseArgs(%q): %v", tt.args, err)
			}
			if id != tt.wantID || force != tt.wantForce {
				t.Errorf("parseWorkspaceCloseArgs(%q) = (%q, %t), want (%q, %t)", tt.args, id, force, tt.wantID, tt.wantForce)
			}
		})
	}
}

// TestGitCredentialScopesToBoundRepo verifies the broker's git-credential
// func refuses any repo other than the one the requesting session opened,
// and denies sessions with no workspace binding (#32).
func TestGitCredentialScopesToBoundRepo(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok-secret")

	// s1 is bound to owner/A; every other session is unbound.
	repoForSession := func(_ context.Context, sessionID string) (string, error) {
		if sessionID == "s1" {
			return "owner/A", nil
		}
		return "", workspace.ErrWorkspaceNotFound
	}
	fn := gitCredentialFromSecrets(repoForSession)
	ctx := context.Background()

	tests := []struct {
		name      string
		session   string
		path      string
		wantAllow bool
	}{
		{"bound repo exact", "s1", "owner/A", true},
		{"bound repo .git suffix", "s1", "owner/A.git", true},
		{"bound repo leading slash", "s1", "/owner/A.git", true},
		{"bound repo case fold", "s1", "owner/a", true},
		{"different repo", "s1", "owner/B", false},
		{"different repo .git", "s1", "owner/B.git", false},
		{"unbound session", "ghost", "owner/A", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, pass, err := fn(ctx, tc.session, "github.com", tc.path)
			if tc.wantAllow {
				if err != nil {
					t.Fatalf("want credential, got error: %v", err)
				}
				if user != "x-access-token" || pass != "tok-secret" {
					t.Fatalf("got user=%q pass=%q, want x-access-token/tok-secret", user, pass)
				}
			} else {
				if err == nil {
					t.Fatalf("want refusal for path %q on session %q, got credential", tc.path, tc.session)
				}
				if pass != "" {
					t.Fatalf("refusal leaked a password: %q", pass)
				}
			}
		})
	}
}

// A non-github host is refused regardless of binding (regression).
func TestGitCredentialRefusesOtherHosts(t *testing.T) {
	fn := gitCredentialFromSecrets(func(context.Context, string) (string, error) { return "owner/A", nil })
	if _, _, err := fn(context.Background(), "s1", "gitlab.com", "owner/A"); err == nil {
		t.Fatal("want refusal for non-github host")
	}
}

func TestCanonRepoPath(t *testing.T) {
	cases := map[string]string{
		"owner/repo":      "owner/repo",
		"/owner/repo":     "owner/repo",
		"owner/repo.git":  "owner/repo",
		"/owner/repo.git": "owner/repo",
		"Owner/Repo":      "owner/repo",
	}
	for in, want := range cases {
		if got := canonRepoPath(in); got != want {
			t.Errorf("canonRepoPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The scope check must treat a transient lookup error (not "no workspace") as
// a refusal too — never fall through to handing out the credential.
func TestGitCredentialRefusesOnLookupError(t *testing.T) {
	boom := errors.New("db is busy")
	fn := gitCredentialFromSecrets(func(context.Context, string) (string, error) { return "", boom })
	_, _, err := fn(context.Background(), "s1", "github.com", "owner/A")
	if err == nil {
		t.Fatal("want refusal when the binding lookup fails")
	}
	if !strings.Contains(err.Error(), "db is busy") {
		t.Errorf("error should wrap the lookup failure, got: %v", err)
	}
}
