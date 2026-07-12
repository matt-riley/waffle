package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		line string
		cmd  string
		args string
	}{
		{"/skill foo", "/skill", "foo"},
		{"/skill foo bar baz", "/skill", "foo bar baz"},
		{"/skill", "/skill", ""},
		{"/repo owner/name", "/repo", "owner/name"},
		{"/repo", "/repo", ""},
		// Word boundary: these must NOT parse as /skill or /repo.
		{"/skills", "/skills", ""},
		{"/report the bug", "/report", "the bug"},
		{"/repository", "/repository", ""},
		// Plain messages keep their leading word intact.
		{"hello world", "hello", "world"},
		{"", "", ""},
	}
	for _, tt := range tests {
		cmd, args := splitCommand(tt.line)
		if cmd != tt.cmd || args != tt.args {
			t.Errorf("splitCommand(%q) = (%q, %q), want (%q, %q)", tt.line, cmd, args, tt.cmd, tt.args)
		}
	}
}

// TestSplitCommandDoesNotMisrouteNearMisses pins the issue #28 regression:
// dispatch matches on the whole leading word, so inputs that merely share a
// prefix with a command fall through to the default (plain message) case.
func TestSplitCommandDoesNotMisrouteNearMisses(t *testing.T) {
	for _, line := range []string{"/skills", "/report the bug", "/skillful advice", "/repos please"} {
		cmd, _ := splitCommand(line)
		if cmd == "/skill" || cmd == "/repo" {
			t.Errorf("splitCommand(%q) routed to %q; want fallthrough to default", line, cmd)
		}
	}
	for line, want := range map[string]string{
		"/skill foo":       "/skill",
		"/skill":           "/skill",
		"/repo owner/name": "/repo",
		"/repo":            "/repo",
	} {
		if cmd, _ := splitCommand(line); cmd != want {
			t.Errorf("splitCommand(%q) = %q, want %q", line, cmd, want)
		}
	}
}

func TestBareSkillAndRepoStillGiveUsage(t *testing.T) {
	c := &chat{}
	if _, err := c.skillMessage(""); err == nil || !strings.Contains(err.Error(), "usage: /skill") {
		t.Errorf("skillMessage(\"\") err = %v, want usage error", err)
	}
	if err := c.repoCommand(context.Background(), "", io.Discard); err == nil || !strings.Contains(err.Error(), "usage: /repo") {
		t.Errorf("repoCommand(\"\") err = %v, want usage error", err)
	}
}

func newTestSessions(t *testing.T) (*session.Store, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return session.New(st), st
}

func TestSwitchToWorkspaceSessionKeepsStateOnTurnsError(t *testing.T) {
	ctx := context.Background()
	sessions, st := newTestSessions(t)

	current, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The workspace session exists but its history won't load: a corrupt
	// turn makes Turns fail, standing in for any transient load error.
	wsSess, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		VALUES (?, 1, 'user', 'not json', '', ?)`,
		wsSess.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert corrupt turn: %v", err)
	}

	c := &chat{
		sessions:  sessions,
		current:   current,
		history:   []llm.Message{llm.UserText("hello"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}},
		persisted: 2,
	}
	if err := c.switchToWorkspaceSession(ctx, wsSess.ID); err == nil {
		t.Fatal("switchToWorkspaceSession = nil, want error")
	}
	if c.current.ID != current.ID {
		t.Errorf("current = %s, want %s (unchanged)", c.current.ID, current.ID)
	}
	if len(c.history) != 2 || c.persisted != 2 {
		t.Errorf("history = %d turns, persisted = %d, want both unchanged at 2", len(c.history), c.persisted)
	}
}

func TestSwitchToWorkspaceSessionLoadsHistory(t *testing.T) {
	ctx := context.Background()
	sessions, _ := newTestSessions(t)

	current, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wsSess, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sessions.AppendTurn(ctx, wsSess.ID, llm.UserText("earlier workspace turn")); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	c := &chat{sessions: sessions, current: current, history: []llm.Message{llm.UserText("old")}, persisted: 1}
	if err := c.switchToWorkspaceSession(ctx, wsSess.ID); err != nil {
		t.Fatalf("switchToWorkspaceSession: %v", err)
	}
	if c.current.ID != wsSess.ID {
		t.Errorf("current = %s, want %s", c.current.ID, wsSess.ID)
	}
	if len(c.history) != 1 || c.history[0].Text() != "earlier workspace turn" {
		t.Errorf("history = %+v, want the workspace session's turn", c.history)
	}
	if c.persisted != len(c.history) {
		t.Errorf("persisted = %d, want %d", c.persisted, len(c.history))
	}
}

func TestResolveAPIKeyRedactsEnvFallbackWithoutStore(t *testing.T) {
	t.Setenv(secret.EnvIdentity, "not-an-age-identity")
	t.Setenv(envName("anthropic"), "sk-ant-env-secret")

	key, redact, err := resolveAPIKey(config.Provider{
		Name:   "anthropic",
		APIKey: "secret://anthropic/api-key",
	})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if key != "sk-ant-env-secret" {
		t.Fatalf("key = %q, want env fallback", key)
	}
	if redact == nil {
		t.Fatal("redact = nil, want runtime redactor")
	}
	got := redact("token sk-ant-env-secret leaked")
	want := "token [redacted:anthropic/api-key] leaked"
	if got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
}

func TestResolveAPIKeyRedactsEnvFallbackWithStore(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvIdentity, id.String())
	t.Setenv(envName("openai"), "sk-openai-env-secret")

	key, redact, err := resolveAPIKey(config.Provider{
		Name:   "openai",
		APIKey: "secret://openai/api-key",
	})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if key != "sk-openai-env-secret" {
		t.Fatalf("key = %q, want env fallback", key)
	}
	if redact == nil {
		t.Fatal("redact = nil, want runtime redactor")
	}
	got := redact("Authorization: Bearer sk-openai-env-secret")
	want := "Authorization: Bearer [redacted:openai/api-key]"
	if got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
}
