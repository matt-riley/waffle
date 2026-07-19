package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

type runtimeRecordingProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	response string
}

func (p *runtimeRecordingProvider) Complete(_ context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return &llm.Response{
		StopReason: llm.StopEndTurn,
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
			Type: llm.BlockText,
			Text: p.response,
		}}},
	}, nil
}

func (p *runtimeRecordingProvider) lastRequest(t *testing.T) llm.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("provider received no requests")
	}
	return p.requests[len(p.requests)-1]
}

func runtimeRegistryConfig() config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConnection{
		"claude-cloud": {Type: "anthropic", APIKey: "secret://provider/claude-cloud/api-key", BaseURL: "https://anthropic.invalid", MaxTokens: 4096},
		"openrouter":   {Type: "openai", APIKey: "secret://provider/openrouter/api-key", BaseURL: "https://openrouter.invalid/v1", MaxTokens: 2048},
	}
	cfg.Models = map[string]config.ModelTarget{
		"writer":     {Provider: "claude-cloud", Model: "claude-upstream"},
		"reviewer":   {Provider: "claude-cloud", Model: "claude-review", MaxTokens: 768},
		"utility":    {Provider: "openrouter", Model: "openai/gpt-utility"},
		"researcher": {Provider: "openrouter", Model: "google/gemini-research"},
	}
	cfg.Agent.DefaultModel = "writer"
	cfg.Agent.UtilityModel = "utility"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	return cfg
}

func TestModelRuntimeResolverRoutesAliasesCachesConnectionsAndRedacts(t *testing.T) {
	cfg := runtimeRegistryConfig()
	created := map[string]int{}
	providers := map[string]*runtimeRecordingProvider{}
	var mu sync.Mutex
	factories := map[string]providerFactory{}
	for _, providerType := range []string{"anthropic", "openai"} {
		providerType := providerType
		factories[providerType] = func(apiKey, baseURL string) llm.Provider {
			mu.Lock()
			defer mu.Unlock()
			created[providerType]++
			p := &runtimeRecordingProvider{}
			providers[baseURL] = p
			wantKey := providerType + "-secret"
			if apiKey != wantKey {
				t.Errorf("%s factory api key = %q, want %q", providerType, apiKey, wantKey)
			}
			return p
		}
	}
	secretCalls := map[string]int{}
	resolver := newModelRuntimeResolverWith(cfg, factories, func(connection config.ProviderConnection) (string, func(string) string, error) {
		secretCalls[connection.Type]++
		key := connection.Type + "-secret"
		return key, func(s string) string { return strings.ReplaceAll(s, key, "[redacted:"+connection.Type+"]") }, nil
	})

	for _, tc := range []struct {
		alias       string
		wantModel   string
		wantTokens  int
		wantBaseURL string
	}{
		{alias: "writer", wantModel: "claude-upstream", wantTokens: 4096, wantBaseURL: "https://anthropic.invalid"},
		{alias: "reviewer", wantModel: "claude-review", wantTokens: 768, wantBaseURL: "https://anthropic.invalid"},
		{alias: "utility", wantModel: "openai/gpt-utility", wantTokens: 2048, wantBaseURL: "https://openrouter.invalid/v1"},
	} {
		if _, err := resolver.Complete(context.Background(), llm.Request{Model: tc.alias}, nil); err != nil {
			t.Fatalf("Complete(%q): %v", tc.alias, err)
		}
		got := providers[tc.wantBaseURL].lastRequest(t)
		if got.Model != tc.wantModel || got.MaxTokens != tc.wantTokens {
			t.Fatalf("Complete(%q) upstream request = model %q tokens %d, want %q/%d", tc.alias, got.Model, got.MaxTokens, tc.wantModel, tc.wantTokens)
		}
	}
	if created["anthropic"] != 1 || created["openai"] != 1 {
		t.Fatalf("factory calls = %#v, want one client per connection", created)
	}
	if secretCalls["anthropic"] != 1 || secretCalls["openai"] != 1 {
		t.Fatalf("secret calls = %#v, want one resolution per connection", secretCalls)
	}
	if got := resolver.redactFor("writer")("anthropic-secret and openai-secret"); got != "[redacted:anthropic] and openai-secret" {
		t.Fatalf("writer redactor = %q, want connection-scoped redaction", got)
	}
	if got := resolver.redactFor("utility")("anthropic-secret and openai-secret"); got != "anthropic-secret and [redacted:openai]" {
		t.Fatalf("utility redactor = %q, want connection-scoped redaction", got)
	}
}

func TestModelRuntimeResolverIsolatesConnectionsOfSameType(t *testing.T) {
	cfg := runtimeRegistryConfig()
	cfg.Providers["other-openai"] = config.ProviderConnection{Type: "openai", BaseURL: "https://openai.invalid/v1"}
	cfg.Models["other"] = config.ModelTarget{Provider: "other-openai", Model: "gpt-upstream"}
	created := 0
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { return &runtimeRecordingProvider{} },
		"openai": func(_, _ string) llm.Provider {
			created++
			return &runtimeRecordingProvider{}
		},
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })

	for _, alias := range []string{"utility", "other", "utility"} {
		if _, err := resolver.Complete(context.Background(), llm.Request{Model: alias}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if created != 2 {
		t.Fatalf("openai factory calls = %d, want one per distinct connection", created)
	}
}

func TestModelRuntimeResolverCachesConnectionConcurrently(t *testing.T) {
	cfg := runtimeRegistryConfig()
	provider := &runtimeRecordingProvider{}
	created := 0
	var mu sync.Mutex
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider {
			mu.Lock()
			created++
			mu.Unlock()
			return provider
		},
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })

	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := resolver.Complete(context.Background(), llm.Request{Model: "writer"}, nil); err != nil {
				t.Errorf("Complete: %v", err)
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("factory calls = %d, want one under concurrent resolution", created)
	}
}

func TestChatModelAliasBuildsDefaultUtilityAndProfileRouting(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := runtimeRegistryConfig()
	cfg.Agent.Profiles = map[string]config.AgentProfile{"research": {Model: "researcher"}}
	upstreams := map[string]*runtimeRecordingProvider{}
	runtime := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, base string) llm.Provider { p := &runtimeRecordingProvider{}; upstreams[base] = p; return p },
		"openai":    func(_, base string) llm.Provider { p := &runtimeRecordingProvider{}; upstreams[base] = p; return p },
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })

	a, cleanup, err := buildAgentWithProfileRuntime(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "research", runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if a.Model != "researcher" || a.UtilityModel != "utility" {
		t.Fatalf("agent models = main %q utility %q, want aliases researcher/utility", a.Model, a.UtilityModel)
	}
	if _, err := a.Provider.Complete(ctx, llm.Request{Model: a.Model}, nil); err != nil {
		t.Fatal(err)
	}
	if got := upstreams["https://openrouter.invalid/v1"].lastRequest(t).Model; got != "google/gemini-research" {
		t.Fatalf("profile upstream model = %q", got)
	}
	if _, err := a.Provider.Complete(ctx, llm.Request{Model: a.UtilityModel}, nil); err != nil {
		t.Fatal(err)
	}
	if got := upstreams["https://openrouter.invalid/v1"].lastRequest(t).Model; got != "openai/gpt-utility" {
		t.Fatalf("utility upstream model = %q", got)
	}
	if _, err := a.Provider.Complete(ctx, llm.Request{Model: cfg.Agent.DefaultModel}, nil); err != nil {
		t.Fatal(err)
	}
	if got := upstreams["https://anthropic.invalid"].lastRequest(t).Model; got != "claude-upstream" {
		t.Fatalf("default upstream model = %q", got)
	}
}

func TestServeModelAliasSharesRuntimeAcrossAgents(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := runtimeRegistryConfig()
	created := 0
	runtime := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { created++; return &runtimeRecordingProvider{} },
		"openai":    func(_, _ string) llm.Provider { created++; return &runtimeRecordingProvider{} },
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })

	agents, _, _, _, _, cleanup, err := buildGatewayAgentsWithRuntime(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for group, a := range agents {
		if _, err := a.Provider.Complete(ctx, llm.Request{Model: a.Model}, nil); err != nil {
			t.Fatalf("group %s: %v", group, err)
		}
	}
	if created != 1 {
		t.Fatalf("provider clients created = %d, want one shared default connection", created)
	}
}

func TestLearnModelAliasUsesUtilityConnection(t *testing.T) {
	cfg := runtimeRegistryConfig()
	provider := &runtimeRecordingProvider{}
	runtime := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { return &runtimeRecordingProvider{} },
		"openai":    func(_, _ string) llm.Provider { return provider },
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })

	model, routedProvider, err := learnRuntime(cfg, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if model != "utility" {
		t.Fatalf("learn model = %q, want utility alias", model)
	}
	if _, err := routedProvider.Complete(context.Background(), llm.Request{Model: model, MaxTokens: 64}, nil); err != nil {
		t.Fatal(err)
	}
	got := provider.lastRequest(t)
	if got.Model != "openai/gpt-utility" || got.MaxTokens != 64 {
		t.Fatalf("learn upstream = %s/%d", got.Model, got.MaxTokens)
	}
}

func TestModelRuntimeResolverReportsAliasContext(t *testing.T) {
	cfg := runtimeRegistryConfig()
	resolver := newModelRuntimeResolverWith(cfg, nil, func(config.ProviderConnection) (string, func(string) string, error) {
		return "", nil, fmt.Errorf("secret unavailable")
	})
	_, err := resolver.Complete(context.Background(), llm.Request{Model: "writer"}, nil)
	if err == nil || !strings.Contains(err.Error(), `model alias "writer"`) || !strings.Contains(err.Error(), `connection "claude-cloud"`) {
		t.Fatalf("error = %v, want alias and connection context", err)
	}
}

func TestModelRuntimeResolverRejectsUnknownAliasWithoutSelectingProvider(t *testing.T) {
	cfg := runtimeRegistryConfig()
	factoryCalls := 0
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { factoryCalls++; return &runtimeRecordingProvider{} },
		"openai":    func(_, _ string) llm.Provider { factoryCalls++; return &runtimeRecordingProvider{} },
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })

	_, err := resolver.Complete(context.Background(), llm.Request{Model: "not-enrolled"}, nil)
	if err == nil || !strings.Contains(err.Error(), `unknown model alias "not-enrolled"`) {
		t.Fatalf("error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("unknown alias selected %d providers", factoryCalls)
	}
}

func TestModelRuntimeResolverKeylessConnectionDoesNotInheritProviderEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-reach-local-provider")
	key, _, err := resolveProviderConnectionSecret(config.ProviderConnection{
		Type:    "openai",
		BaseURL: "http://127.0.0.1:11434/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("keyless connection inherited %q from provider environment", key)
	}
}

func TestModelRuntimeResolverLegacyConnectionKeepsProviderEnvironmentFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "legacy-openai-key")
	cfg := config.Default()
	cfg.Provider = config.Provider{Name: "openai", Model: "gpt-legacy"}
	var factoryKey string
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"openai": func(apiKey, _ string) llm.Provider {
			factoryKey = apiKey
			return &runtimeRecordingProvider{}
		},
	}, resolveProviderConnectionSecret)
	if _, err := resolver.Complete(context.Background(), llm.Request{Model: "gpt-legacy"}, nil); err != nil {
		t.Fatal(err)
	}
	if factoryKey != "legacy-openai-key" {
		t.Fatalf("legacy factory key = %q", factoryKey)
	}
}

func TestModelRuntimeResolverNamedSecretReferenceFailsClosedWithoutStore(t *testing.T) {
	t.Setenv("WAFFLE_AGE_IDENTITY", "invalid-identity")
	t.Setenv("OPENAI_API_KEY", "must-not-replace-named-secret")
	key, _, err := resolveProviderConnectionSecret(config.ProviderConnection{
		Type:   "openai",
		APIKey: "secret://provider/openrouter/api-key",
	})
	if err == nil {
		t.Fatalf("named secret reference fell back to environment key %q", key)
	}
	if key != "" {
		t.Fatalf("failed named secret resolution returned key %q", key)
	}
}

func TestModelRuntimeResolverLoadedMixedRegistryNeverFallsBackToLegacy(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-reach-keyless-local")
	cfg := loadRuntimeConfig(t, `
[provider]
name = "anthropic"
model = "legacy-model"
utility_model = "legacy-utility"
api_key = "legacy-literal"

[providers.local]
type = "openai"
base_url = "http://127.0.0.1:11434/v1"

[models.local]
provider = "local"
model = "qwen3:32b"

[agent]
default_model = "local"
`)
	var factoryKey string
	factoryCalls := 0
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { factoryCalls++; return &runtimeRecordingProvider{} },
		"openai": func(key, _ string) llm.Provider {
			factoryCalls++
			factoryKey = key
			return &runtimeRecordingProvider{}
		},
	}, resolveProviderConnectionSecret)
	if _, err := resolver.Complete(context.Background(), llm.Request{Model: "local"}, nil); err != nil {
		t.Fatal(err)
	}
	if factoryKey != "" {
		t.Fatalf("keyless named connection inherited %q", factoryKey)
	}
	if _, err := resolver.Complete(context.Background(), llm.Request{Model: "not-enrolled"}, nil); err == nil || !strings.Contains(err.Error(), "unknown model alias") {
		t.Fatalf("unknown alias error = %v", err)
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d, unknown alias selected legacy provider", factoryCalls)
	}
	profileModel, err := resolveRuntimeProfileModel(cfg, config.AgentProfile{Model: "default"})
	if err != nil || profileModel != "local" {
		t.Fatalf("default profile model = %q, %v", profileModel, err)
	}
}

func TestModelRuntimeResolverLoadedExplicitEmptyRegistrySuppressesLegacy(t *testing.T) {
	cfg := loadRuntimeConfig(t, `
[provider]
name = "anthropic"
model = "legacy-model"
utility_model = "legacy-utility"
api_key = "legacy-literal"

[providers]
`)
	factoryCalls := 0
	resolver := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { factoryCalls++; return &runtimeRecordingProvider{} },
	}, resolveProviderConnectionSecret)
	if _, err := resolver.Complete(context.Background(), llm.Request{Model: "legacy-model"}, nil); err == nil || !strings.Contains(err.Error(), "unknown model alias") {
		t.Fatalf("explicit-empty registry error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("explicit-empty registry selected %d legacy providers", factoryCalls)
	}
	if got := runtimeUtilityModel(cfg); got != "" {
		t.Fatalf("explicit-empty registry resurrected legacy utility model %q", got)
	}
}

func TestServeModelAliasBrokerUsesOnlyEffectiveNamedConnections(t *testing.T) {
	cfg := runtimeRegistryConfig()
	cfg.Provider = config.Provider{Name: "anthropic", APIKey: "secret://anthropic/api-key", BaseURL: "https://stale.invalid"}
	cfg.Providers["local"] = config.ProviderConnection{Type: "openai", BaseURL: "http://127.0.0.1:11434/v1"}
	var resolved []string
	upstreams := brokerUpstreamsWithSecretResolver(cfg, func(connection config.ProviderConnection) (string, func(string) string, error) {
		resolved = append(resolved, connection.APIKey)
		switch connection.APIKey {
		case "secret://provider/claude-cloud/api-key":
			return "anthropic-key", nil, nil
		case "secret://provider/openrouter/api-key":
			return "openrouter-key", nil, nil
		case "":
			return "", nil, nil
		default:
			return "", nil, fmt.Errorf("unexpected secret %q", connection.APIKey)
		}
	})
	if len(upstreams) != 3 {
		t.Fatalf("upstreams = %#v", upstreams)
	}
	want := map[string]struct {
		base, header, value string
	}{
		"claude-cloud": {base: "https://anthropic.invalid", header: "x-api-key", value: "anthropic-key"},
		"openrouter":   {base: "https://openrouter.invalid/v1", header: "Authorization", value: "Bearer openrouter-key"},
		"local":        {base: "http://127.0.0.1:11434/v1"},
	}
	for _, upstream := range upstreams {
		expected, ok := want[upstream.Name]
		if !ok {
			t.Fatalf("unexpected upstream %#v", upstream)
		}
		if upstream.BaseURL != expected.base || upstream.Header != expected.header || upstream.Value != expected.value {
			t.Fatalf("upstream %q = %#v, want %#v", upstream.Name, upstream, expected)
		}
	}
	for _, ref := range resolved {
		if ref == "secret://anthropic/api-key" {
			t.Fatalf("stale legacy secret was resolved: %#v", resolved)
		}
	}
}

func TestServeModelAliasBrokerRetainsOnlyNormalizedLegacyEnvironmentFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "legacy-broker-key")
	cfg := loadRuntimeConfig(t, `
[provider]
name = "openai"
model = "gpt-legacy"
api_key = ""
`)
	var resolved config.ProviderConnection
	upstreams := brokerUpstreamsWithSecretResolver(cfg, func(connection config.ProviderConnection) (string, func(string) string, error) {
		resolved = connection
		return connection.APIKey, nil, nil
	})
	if resolved.APIKey != "legacy-broker-key" {
		t.Fatalf("normalized legacy broker api key = %q", resolved.APIKey)
	}
	if len(upstreams) != 1 || upstreams[0].Name != "default" || upstreams[0].Value != "Bearer legacy-broker-key" {
		t.Fatalf("legacy upstreams = %#v", upstreams)
	}
}

func TestChatModelAliasChildProfileUsesResolvedTokenDefault(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := runtimeRegistryConfig()
	cfg.Models["writer"] = config.ModelTarget{Provider: "claude-cloud", Model: "claude-upstream", MaxTokens: 111}
	cfg.Models["researcher"] = config.ModelTarget{Provider: "openrouter", Model: "research-upstream", MaxTokens: 333}
	cfg.Agent.Subagents = true
	cfg.Agent.Profiles = map[string]config.AgentProfile{"research": {Model: "researcher"}}
	childProvider := &runtimeRecordingProvider{response: "```json\n{\"status\":\"done\",\"summary\":\"reviewed\"}\n```"}
	runtime := newModelRuntimeResolverWith(cfg, map[string]providerFactory{
		"anthropic": func(_, _ string) llm.Provider { return &runtimeRecordingProvider{} },
		"openai":    func(_, _ string) llm.Provider { return childProvider },
	}, func(config.ProviderConnection) (string, func(string) string, error) { return "", nil, nil })
	a, cleanup, err := buildAgentWithProfileRuntime(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "", runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := a.Tools.Run(ctx, "spawn_subagent", json.RawMessage(`{"task":"review","profile":"research"}`)); err != nil {
		t.Fatal(err)
	}
	got := childProvider.lastRequest(t)
	if got.Model != "research-upstream" || got.MaxTokens != 333 {
		t.Fatalf("child upstream = %s/%d, want research-upstream/333", got.Model, got.MaxTokens)
	}

	cfg.Agent.Profiles["research"] = config.AgentProfile{Model: "researcher", MaxTokens: 444}
	profiles := childProfilesFromConfig(cfg, nil)
	if got := profiles["research"].MaxTokens; got != 444 {
		t.Fatalf("explicit child profile max_tokens = %d, want 444", got)
	}
}

func TestChatModelAliasBannerShowsConnectionAndType(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[providers.local]
type = "openai"
base_url = "http://127.0.0.1:11434/v1"

[models.writer]
provider = "local"
model = "qwen3:32b"

[agent]
default_model = "writer"
subagents = false
learn = false
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := chatCmd(context.Background(), nil, strings.NewReader(""), &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "writer via local (openai)") {
		t.Fatalf("chat banner = %q", got)
	}
}

func TestChatProviderlessConfigStopsWithSetupGuidance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := chatCmd(context.Background(), nil, strings.NewReader("hey\n"), &stdout, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no provider configured") ||
		!strings.Contains(err.Error(), "sudo waffle provider add") {
		t.Fatalf("providerless chat error = %v, want provider setup guidance", err)
	}
	if strings.Contains(stdout.String(), "anthropic") {
		t.Fatalf("providerless chat selected Anthropic: %q", stdout.String())
	}
}

func TestChatExplicitEmptyProviderRegistryOverridesLegacyFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	configBody := "[provider]\nname = \"anthropic\"\nmodel = \"legacy\"\n\n[providers]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := chatCmd(context.Background(), nil, strings.NewReader("hey\n"), &stdout, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "sudo waffle provider add") {
		t.Fatalf("explicit-empty provider chat error = %v, want provider setup guidance", err)
	}
	if strings.Contains(stdout.String(), "anthropic") {
		t.Fatalf("explicit-empty provider registry selected legacy Anthropic: %q", stdout.String())
	}
}

func TestChatLegacyProviderConfigurationStillStarts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "legacy-test-key")
	configBody := "[provider]\nname = \"anthropic\"\nmodel = \"legacy\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := chatCmd(context.Background(), nil, strings.NewReader(""), &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("legacy provider chat: %v", err)
	}
	if !strings.Contains(stdout.String(), "legacy via default (anthropic)") {
		t.Fatalf("legacy chat banner = %q", stdout.String())
	}
}

func loadRuntimeConfig(t *testing.T, body string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
