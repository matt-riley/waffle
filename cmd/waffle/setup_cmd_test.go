package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/zalando/go-keyring"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/secret"
)

func TestSetupFreshInstallEndToEnd(t *testing.T) {
	home := installSetupHome(t)
	installSetupProviderManager(t, home)
	installFakeProviderCatalogue(t, &fakeProviderCatalogue{result: discoveredCatalogue(
		modelcatalog.Model{ID: "gpt-test", DisplayName: "GPT Test"},
	)})
	installProviderSecretReader(t, "setup-test-key", nil)

	// Guided provider prompts + accept default system prompt.
	input := strings.NewReader("openai\n\n1\n-\nn\n\n")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"setup"}, input, &stdout, &stderr); err != nil {
		t.Fatalf("setup: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	out := stdout.String() + stderr.String()
	for _, want := range []string{
		"Creating secret-store identity",
		"Adding a model provider",
		"Configuring agent.profile.main",
		"Setup complete",
		"Active model alias: gpt-test",
		"waffle chat",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "setup-test-key") {
		t.Fatal("API key leaked in setup output")
	}

	// Identity exists in the mock keyring.
	if _, err := secret.LoadIdentity(); err != nil {
		t.Fatalf("LoadIdentity after setup: %v", err)
	}

	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if len(cfg.Providers) == 0 {
		t.Fatal("expected at least one provider after setup")
	}
	if _, ok := cfg.Providers["openai"]; !ok {
		t.Fatalf("providers = %#v, want openai", cfg.Providers)
	}
	if cfg.Agent.DefaultModel != "gpt-test" {
		t.Fatalf("default_model = %q, want gpt-test", cfg.Agent.DefaultModel)
	}
	main, ok := cfg.Agent.Profiles["main"]
	if !ok {
		t.Fatal("agent.profile.main missing")
	}
	if main.System != defaultMainSystemPrompt || main.Model != "default" || main.Sandbox != "host" {
		t.Fatalf("profile.main = %#v", main)
	}
}

func TestSetupFullyConfiguredRerunSkipsSteps(t *testing.T) {
	home := installSetupHome(t)
	id := seedSetupIdentity(t)
	writeSetupConfiguredConfig(t, home, id)

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"setup"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("setup re-run: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Secret-store identity already configured",
		"Provider already configured",
		"agent.profile.main already configured",
		"Setup complete",
		"Active model alias: gpt",
		"waffle chat",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, skip := range []string{
		"Creating secret-store identity",
		"Adding a model provider",
		"Configuring agent.profile.main",
	} {
		if strings.Contains(out, skip) {
			t.Fatalf("re-run should not %q:\n%s", skip, out)
		}
	}

	// Config body must stay stable on a pure skip path.
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `model = "gpt-exact"`) || !strings.Contains(string(raw), "[agent.profile.main]") {
		t.Fatalf("config unexpectedly changed:\n%s", raw)
	}
}

func TestSetupPartialIdentityOnlyAddsProviderAndProfile(t *testing.T) {
	home := installSetupHome(t)
	_ = seedSetupIdentity(t)
	installSetupProviderManager(t, home)
	installFakeProviderCatalogue(t, &fakeProviderCatalogue{result: discoveredCatalogue(
		modelcatalog.Model{ID: "claude-test", DisplayName: "Claude Test"},
	)})
	installProviderSecretReader(t, "partial-key", nil)

	// Identity exists; provider guided + custom system prompt.
	input := strings.NewReader("anthropic\n\n1\n-\nn\nYou are a careful assistant.\n")
	var stdout, stderr bytes.Buffer
	if err := setupCmd(context.Background(), nil, input, &stdout, &stderr); err != nil {
		t.Fatalf("setup partial: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Secret-store identity already configured") {
		t.Fatalf("want identity skip:\n%s", out)
	}
	if !strings.Contains(out, "Adding a model provider") {
		t.Fatalf("want provider step:\n%s", out)
	}
	if !strings.Contains(out, "Configuring agent.profile.main") {
		t.Fatalf("want profile step:\n%s", out)
	}
	if !strings.Contains(out, "Active model alias: claude-test") {
		t.Fatalf("want model alias:\n%s", out)
	}
	if strings.Contains(out, "Creating secret-store identity") {
		t.Fatalf("must not re-init identity:\n%s", out)
	}

	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["anthropic"]; !ok {
		t.Fatalf("providers = %#v, want anthropic", cfg.Providers)
	}
	main, ok := cfg.Agent.Profiles["main"]
	if !ok {
		t.Fatal("agent.profile.main missing")
	}
	if main.System != "You are a careful assistant." {
		t.Fatalf("system = %q", main.System)
	}
}

func TestSetupChatMessageDirectsToSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := chatCmd(context.Background(), nil, strings.NewReader("hey\n"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "waffle setup") {
		t.Fatalf("chat error = %v, want waffle setup guidance", err)
	}
}

func installSetupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	t.Setenv(secret.EnvIdentity, "")
	keyring.MockInit()
	return home
}

func seedSetupIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := secret.InitIdentity(false)
	if err != nil {
		t.Fatalf("InitIdentity: %v", err)
	}
	return id
}

func installSetupProviderManager(t *testing.T, home string) *providerconfig.Manager {
	t.Helper()
	active := false
	manager := &providerconfig.Manager{
		ConfigPath:  filepath.Join(home, "config.toml"),
		SecretsPath: filepath.Join(home, "secrets.age"),
		LockPath:    filepath.Join(home, "provider-config.lock"),
		// Identity is resolved at open time so setup's secret init can run first.
		Probe: func(context.Context, config.ResolvedModel, string) error { return nil },
		Restart: func(context.Context) error {
			active = true
			return nil
		},
		Stop: func(context.Context) error {
			active = false
			return nil
		},
		Health:        func(context.Context) error { return nil },
		ServiceActive: func(context.Context) (bool, error) { return active, nil },
		RestoreService: func(_ context.Context, wasActive bool) error {
			active = wasActive
			return nil
		},
	}
	old := openProviderManager
	openProviderManager = func() (providerManager, error) {
		id, err := secret.LoadIdentity()
		if err != nil {
			return nil, err
		}
		manager.Identity = id
		return manager, nil
	}
	t.Cleanup(func() { openProviderManager = old })
	return manager
}

func writeSetupConfiguredConfig(t *testing.T, home string, id *age.X25519Identity) {
	t.Helper()
	body := `# setup re-run fixture
[providers.openai]
type = "openai"
base_url = "https://api.example.invalid/v1"
api_key = "secret://provider/openai/api-key"

[models.gpt]
provider = "openai"
model = "gpt-exact"

[agent]
default_model = "gpt"

[agent.profile.main]
system = "You are the owner's personal assistant."
model = "default"
sandbox = "host"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Secrets file is optional for the skip path; create a placeholder store.
	store := secret.OpenFile(filepath.Join(home, "secrets.age"), id)
	if err := store.Set("provider/openai/api-key", "already-enrolled-key"); err != nil {
		t.Fatal(err)
	}
}
