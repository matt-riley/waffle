package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/secret"
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
