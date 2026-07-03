package main

import (
	"testing"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/secret"
)

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
