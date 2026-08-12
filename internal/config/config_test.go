package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("got %+v, want defaults %+v", cfg, Default())
	}
}

func TestDashboardDefaultsDisabled(t *testing.T) {
	if Default().Dashboard.Enabled {
		t.Fatal("dashboard must default disabled")
	}
	if Default().Dashboard.SkillImportRoots != nil {
		t.Fatalf("dashboard skill import roots = %v, want nil deny-by-default", Default().Dashboard.SkillImportRoots)
	}
	if Default().Dashboard.SkillGitHosts != nil {
		t.Fatalf("dashboard skill Git hosts = %v, want nil deny-by-default", Default().Dashboard.SkillGitHosts)
	}
}

func TestDashboardEnabledLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[dashboard]\nenabled = true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.Enabled {
		t.Fatal("dashboard.enabled = false, want true")
	}
}

func TestDashboardSkillSourceAllowlistsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[dashboard]
enabled = true
skill_import_roots = ["/srv/reviewed-skills", "/opt/waffle-skills"]
skill_git_hosts = ["github.com"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/srv/reviewed-skills", "/opt/waffle-skills"}; !reflect.DeepEqual(cfg.Dashboard.SkillImportRoots, want) {
		t.Fatalf("skill import roots = %v, want %v", cfg.Dashboard.SkillImportRoots, want)
	}
	if want := []string{"github.com"}; !reflect.DeepEqual(cfg.Dashboard.SkillGitHosts, want) {
		t.Fatalf("skill Git hosts = %v, want %v", cfg.Dashboard.SkillGitHosts, want)
	}
}

func TestExampleDashboardSkillSourcesAreCommentedChoices(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		`# skill_import_roots = ["/absolute/path/to/reviewed-skills"]`,
		`# skill_git_hosts = ["github.com"]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("example config missing %q", want)
		}
	}
}

func TestLoadChatSocketRequiresAbsoluteCleanPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[chat]\nsocket = \"relative.sock\"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative socket = %v", err)
	}
	writeFile(t, path, "[chat]\nsocket = \"/tmp/waffle-chat.sock\"\n")
	cfg, err := Load(path)
	if err != nil || cfg.Chat.Socket != "/tmp/waffle-chat.sock" {
		t.Fatalf("chat config = %+v, %v", cfg.Chat, err)
	}
}

func TestLoadChatSocketRejectsUncleanOrNULPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, socket := range []string{"/tmp/../tmp/waffle-chat.sock", "/tmp/waffle\\u0000chat.sock"} {
		writeFile(t, path, "[chat]\nsocket = \""+socket+"\"\n")
		if _, err := Load(path); err == nil {
			t.Fatalf("Load accepted socket path %q", socket)
		}
	}
}

func TestJobRetryPolicyParsesWithFireOnceDefaults(t *testing.T) {
	defaults := Default().Jobs
	if defaults.MaxAttempts != 1 || defaults.BaseBackoff != "10s" || defaults.MaxBackoff != "10m" || defaults.StallTimeout != "5m" {
		t.Fatalf("job defaults=%+v", defaults)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[jobs]\nmax_attempts = 4\nbase_backoff = \"2s\"\nmax_backoff = \"30s\"\nstall_timeout = \"45s\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jobs.MaxAttempts != 4 || cfg.Jobs.BaseBackoff != "2s" || cfg.Jobs.MaxBackoff != "30s" || cfg.Jobs.StallTimeout != "45s" {
		t.Fatalf("jobs=%+v", cfg.Jobs)
	}
}

func TestDefaultSandboxImageIncludesWorkspaceTools(t *testing.T) {
	if got := Default().Sandbox.Image; got != "buildpack-deps:bookworm-scm" {
		t.Fatalf("Sandbox.Image = %q, want default image containing Git", got)
	}
}

func TestSandboxResourceLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[sandbox]\nmemory = \"3g\"\ncpus = 1.5\npids = 256\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sandbox.Memory != "3g" || cfg.Sandbox.CPUs != 1.5 || cfg.Sandbox.PIDs != 256 {
		t.Errorf("Sandbox = %+v, want configured resource limits", cfg.Sandbox)
	}
	writeFile(t, path, "[sandbox]\nmemory = \"banana\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted invalid sandbox memory")
	}
}

func TestLifecycleAndGitHubAppConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[workspace]\nidle_timeout = \"1h\"\nclose_ttl = \"48h\"\n[store]\nretain = \"365d\"\n[github.app]\napp_id = 42\ninstallation_id = 7\nprivate_key = \"secret://github/app-key\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.IdleTimeout != "1h" || cfg.Workspace.CloseTTL != "48h" || cfg.Store.Retain != "365d" || cfg.GitHub.App.AppID != 42 {
		t.Fatalf("config = %+v", cfg)
	}
	writeFile(t, path, "[store]\nretain = \"banana\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("invalid retention accepted")
	}
	writeFile(t, path, "[github.app]\napp_id = 42\n")
	if _, err := Load(path); err == nil {
		t.Fatal("incomplete github app accepted")
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[gateway]\nlisten = \"127.0.0.1:9999\"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Gateway.Listen)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want default info", cfg.Log.Level)
	}
}

func TestMemoryInjectBudget(t *testing.T) {
	if got := Default().Memory.InjectBudget; got != 8192 {
		t.Fatalf("default inject_budget = %d, want 8192", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[memory]\ninject_budget = 4096\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.InjectBudget != 4096 {
		t.Fatalf("inject_budget = %d, want 4096", cfg.Memory.InjectBudget)
	}
	writeFile(t, path, "[memory]\ninject_budget = -1\n")
	if _, err := Load(path); err == nil {
		t.Fatal("negative inject_budget accepted")
	}
}

func TestReflectAfterZeroDisables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[memory]\nreflect_after = \"0\"\nreflect_every = \"0\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Memory.ReflectAfter != "0" {
		t.Fatalf("ReflectAfter = %q", cfg.Memory.ReflectAfter)
	}
	// "0" is a disable sentinel; positive durations still required for other values.
	writeFile(t, path, "[memory]\nreflect_after = \"30m\"\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ReflectAfter != "30m" {
		t.Fatalf("ReflectAfter = %q", cfg.Memory.ReflectAfter)
	}
}

func TestDefaultStatusListenerIsLoopback(t *testing.T) {
	if got := Default().Gateway.StatusListen; got != "127.0.0.1:8422" {
		t.Errorf("Gateway.StatusListen = %q, want 127.0.0.1:8422", got)
	}
}

func TestLoadRejectsNonLoopbackStatusListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[gateway]\nstatus_listen = \"0.0.0.0:8422\"\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted non-loopback status listener, want error")
	}
}

func TestLoadDashboardTailnetOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[dashboard]
enabled = true

[dashboard.tailnet]
enabled = true
serve_host = "waffle.tail848095.ts.net"
allowed_logins = ["matt-riley@github"]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Dashboard.Tailnet.Enabled {
		t.Error("Dashboard.Tailnet.Enabled = false, want true")
	}
	if got, want := cfg.Dashboard.Tailnet.ServeHost, "waffle.tail848095.ts.net"; got != want {
		t.Errorf("ServeHost = %q, want %q", got, want)
	}
	// SSO logins are not email addresses and must survive load unchanged.
	if got, want := cfg.Dashboard.Tailnet.AllowedLogins, []string{"matt-riley@github"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("AllowedLogins = %v, want %v", got, want)
	}
	// The tailnet opt-in must never move the bind address off loopback.
	if got, want := cfg.Gateway.StatusListen, "127.0.0.1:8422"; got != want {
		t.Errorf("StatusListen = %q, want %q", got, want)
	}
}

func TestLoadRejectsIncompleteDashboardTailnet(t *testing.T) {
	tests := []struct {
		name string
		toml string
	}{
		{
			name: "missing serve host",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "serve host is an ip",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"100.64.0.1\"\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "serve host carries a port",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle.tail848095.ts.net:443\"\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "serve host carries a scheme",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"https://waffle.tail848095.ts.net\"\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "serve host is not fully qualified",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle\"\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "no allowed logins",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle.tail848095.ts.net\"\n",
		},
		{
			name: "empty allowed login",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle.tail848095.ts.net\"\nallowed_logins = [\"\"]\n",
		},
		{
			name: "duplicate allowed login",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle.tail848095.ts.net\"\nallowed_logins = [\"matt-riley@github\", \"Matt-Riley@GitHub\"]\n",
		},
		{
			name: "tailnet access without the dashboard",
			toml: "[dashboard]\nenabled = false\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle.tail848095.ts.net\"\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "settings without the opt-in",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nserve_host = \"waffle.tail848095.ts.net\"\nallowed_logins = [\"matt-riley@github\"]\n",
		},
		{
			name: "unknown tailnet key",
			toml: "[dashboard]\nenabled = true\n\n[dashboard.tailnet]\nenabled = true\nserve_host = \"waffle.tail848095.ts.net\"\nallowed_logins = [\"matt-riley@github\"]\nallow_all = true\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tt.toml)
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted an incomplete dashboard.tailnet opt-in, want error")
			}
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[gateway]\nlisten = \"x\"\nlistne_typo = true\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted unknown key, want error")
	}

}

func TestLoadProviderRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.anthropic]
type = "anthropic"
api_key = "secret://provider/anthropic/api-key"
max_tokens = 4096

[providers.openrouter]
type = "openai"
api_key = "secret://provider/openrouter/api-key"
base_url = "https://openrouter.ai/api/v1"

[models.claude]
provider = "anthropic"
model = "claude-sonnet-4-6"

[models.claude-fast]
provider = "anthropic"
model = "claude-haiku-4-5"
max_tokens = 1024

[models.gpt]
provider = "openrouter"
model = "openai/gpt-5.4"

[agent]
default_model = "claude"
utility_model = "gpt"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("Providers = %#v, want two connections", cfg.Providers)
	}
	if got := cfg.Providers["anthropic"]; got.Type != "anthropic" || got.MaxTokens != 4096 {
		t.Fatalf("anthropic connection = %#v", got)
	}
	if got := cfg.Providers["openrouter"]; got.Type != "openai" || got.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("openrouter connection = %#v", got)
	}
	if cfg.Agent.DefaultModel != "claude" || cfg.Agent.UtilityModel != "gpt" {
		t.Fatalf("agent model aliases = default %q utility %q", cfg.Agent.DefaultModel, cfg.Agent.UtilityModel)
	}

	// Provider configuration is optional: this is the normal Installed state.
	writeFile(t, path, "[log]\nlevel = \"debug\"\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load provider-empty config: %v", err)
	}
	if cfg.Provider.Name != "" || len(cfg.Providers) != 0 || len(cfg.Models) != 0 {
		t.Fatalf("provider-empty config gained a provider: legacy=%#v providers=%#v models=%#v", cfg.Provider, cfg.Providers, cfg.Models)
	}
}

func TestResolveModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.anthropic]
type = "anthropic"
api_key = "secret://provider/anthropic/api-key"
max_tokens = 4096

[providers.local]
type = "openai"
base_url = "http://127.0.0.1:11434/v1"
max_tokens = 2048

[models.primary]
provider = "anthropic"
model = "claude-sonnet-4-6"

[models.fast]
provider = "anthropic"
model = "claude-haiku-4-5"
max_tokens = 512

[models.local]
provider = "local"
model = "qwen3:32b"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		alias          string
		connectionName string
		providerType   string
		upstreamModel  string
		maxTokens      int
	}{
		{alias: "primary", connectionName: "anthropic", providerType: "anthropic", upstreamModel: "claude-sonnet-4-6", maxTokens: 4096},
		{alias: "fast", connectionName: "anthropic", providerType: "anthropic", upstreamModel: "claude-haiku-4-5", maxTokens: 512},
		{alias: "local", connectionName: "local", providerType: "openai", upstreamModel: "qwen3:32b", maxTokens: 2048},
	}
	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			got, err := cfg.ResolveModel(tt.alias)
			if err != nil {
				t.Fatalf("ResolveModel: %v", err)
			}
			if got.Alias != tt.alias || got.ConnectionName != tt.connectionName || got.Connection.Type != tt.providerType || got.UpstreamModel != tt.upstreamModel || got.MaxTokens != tt.maxTokens {
				t.Fatalf("ResolveModel(%q) = %#v", tt.alias, got)
			}
		})
	}
	if _, err := cfg.ResolveModel("missing"); err == nil || !strings.Contains(err.Error(), "unknown model alias") {
		t.Fatalf("unknown alias error = %v", err)
	}
}

func TestLoadProviderRegistryRejectsInvalidReferences(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown provider reference",
			body: "[models.main]\nprovider = \"missing\"\nmodel = \"model-1\"\n",
			want: "unknown provider",
		},
		{
			name: "invalid connection name",
			body: "[providers.\"../escape\"]\ntype = \"anthropic\"\n",
			want: "invalid connection name",
		},
		{
			name: "unsupported provider type",
			body: "[providers.vendor]\ntype = \"vendor-native\"\n",
			want: "unsupported type",
		},
		{
			name: "missing upstream model",
			body: "[providers.local]\ntype = \"openai\"\n[models.main]\nprovider = \"local\"\n",
			want: "model is required",
		},
		{
			name: "invalid alias",
			body: "[providers.local]\ntype = \"openai\"\n[models.\"bad alias\"]\nprovider = \"local\"\nmodel = \"model-1\"\n",
			want: "invalid model alias",
		},
		{
			name: "unknown default alias",
			body: "[providers.local]\ntype = \"openai\"\n[agent]\ndefault_model = \"missing\"\n",
			want: "agent.default_model",
		},
		{
			name: "unknown utility alias",
			body: "[providers.local]\ntype = \"openai\"\n[agent]\nutility_model = \"missing\"\n",
			want: "agent.utility_model",
		},
		{
			name: "unknown provider key",
			body: "[providers.local]\ntype = \"openai\"\ntypo = true\n",
			want: "unknown keys",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tt.body)
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestProviderRegistryAPIKeyReferences(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		wantErr string
	}{
		{name: "auth-free endpoint", apiKey: ""},
		{name: "connection-scoped secret", apiKey: "secret://provider/local/api-key"},
		{name: "raw key", apiKey: "sk-live-secret", wantErr: "must be empty or secret://provider/local/api-key"},
		{name: "malformed secret path", apiKey: "secret://provider/local/token", wantErr: "must be empty or secret://provider/local/api-key"},
		{name: "different connection secret", apiKey: "secret://provider/other/api-key", wantErr: "must be empty or secret://provider/local/api-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[providers.local]\ntype = \"openai\"\n"
			if tt.apiKey != "" {
				body += "api_key = \"" + tt.apiKey + "\"\n"
			}
			writeFile(t, path, body)
			_, err := Load(path)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Load error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}

	// Raw keys in the singular legacy table retain their historical behavior;
	// the first provider-management write migrates them to encrypted storage.
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[provider]\nname = \"openai\"\napi_key = \"legacy-raw-key\"\nmodel = \"legacy-model\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy raw key: %v", err)
	}
	resolved, err := cfg.ResolveModel("default")
	if err != nil || resolved.Connection.APIKey != "legacy-raw-key" {
		t.Fatalf("ResolveModel legacy raw key = %#v, %v", resolved, err)
	}
}

func TestLegacyProviderCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[provider]
name = "openai"
api_key = "secret://legacy/api-key"
base_url = "http://127.0.0.1:8080/v1"
model = "legacy-main"
utility_model = "legacy-utility"
max_tokens = 1234
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Provider.Name != "openai" || cfg.Provider.Model != "legacy-main" || cfg.Provider.UtilityModel != "legacy-utility" {
		t.Fatalf("legacy provider changed: %#v", cfg.Provider)
	}
	if cfg.Agent.DefaultModel != "default" || cfg.Agent.UtilityModel != "utility" {
		t.Fatalf("legacy aliases = default %q utility %q", cfg.Agent.DefaultModel, cfg.Agent.UtilityModel)
	}
	main, err := cfg.ResolveModel("default")
	if err != nil {
		t.Fatalf("ResolveModel(default): %v", err)
	}
	utility, err := cfg.ResolveModel("utility")
	if err != nil {
		t.Fatalf("ResolveModel(utility): %v", err)
	}
	if main.ConnectionName != "default" || main.Connection.Type != "openai" || main.UpstreamModel != "legacy-main" || main.MaxTokens != 1234 {
		t.Fatalf("normalized main = %#v", main)
	}
	if utility.ConnectionName != "default" || utility.UpstreamModel != "legacy-utility" {
		t.Fatalf("normalized utility = %#v", utility)
	}

	// A partial legacy table retains the historical Anthropic defaults.
	writeFile(t, path, "[provider]\nmodel = \"custom-main\"\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load partial legacy provider: %v", err)
	}
	if cfg.Provider.Name != "anthropic" || cfg.Provider.APIKey != "secret://anthropic/api-key" || cfg.Provider.MaxTokens != 64000 {
		t.Fatalf("partial legacy defaults changed: %#v", cfg.Provider)
	}
	if got, err := cfg.ResolveModel("default"); err != nil || got.UpstreamModel != "custom-main" {
		t.Fatalf("partial legacy default resolved to %#v, %v", got, err)
	}
}

func TestLegacyProviderExplicitEmptyRegistryTakesPrecedence(t *testing.T) {
	for _, registry := range []string{"providers", "models"} {
		t.Run(registry, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, `
[provider]
name = "anthropic"
model = "legacy-main"

[`+registry+`]
`)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Providers) != 0 || len(cfg.Models) != 0 {
				t.Fatalf("explicit empty [%s] did not suppress legacy normalization: providers=%#v models=%#v", registry, cfg.Providers, cfg.Models)
			}
			if cfg.Agent.DefaultModel != "" || cfg.Agent.UtilityModel != "" {
				t.Fatalf("explicit empty [%s] gained legacy aliases: default=%q utility=%q", registry, cfg.Agent.DefaultModel, cfg.Agent.UtilityModel)
			}
		})
	}
}

func TestProviderRegistrySourceTracksLoadPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want ProviderRegistrySource
	}{
		{name: "none", body: "[log]\nlevel = \"debug\"\n", want: ProviderRegistryNone},
		{name: "legacy", body: "[provider]\nname = \"anthropic\"\nmodel = \"legacy\"\n", want: ProviderRegistryLegacy},
		{name: "explicit", body: "[providers.local]\ntype = \"openai\"\n[models.local]\nprovider = \"local\"\nmodel = \"qwen\"\n", want: ProviderRegistryExplicit},
		{name: "explicit-empty", body: "[provider]\nname = \"anthropic\"\nmodel = \"legacy\"\n[providers]\n", want: ProviderRegistryExplicit},
		{name: "mixed", body: "[provider]\nname = \"anthropic\"\nmodel = \"legacy\"\n[providers.local]\ntype = \"openai\"\n[models.local]\nprovider = \"local\"\nmodel = \"qwen\"\n", want: ProviderRegistryExplicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tc.body)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.ProviderRegistrySource(); got != tc.want {
				t.Fatalf("ProviderRegistrySource = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelfdevDefaultsAndApproval(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Selfdev.Approval != "manual" || !cfg.Selfdev.Verify {
		t.Errorf("Selfdev defaults = %+v, want manual and verify=true", cfg.Selfdev)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[selfdev]\napproval = \"bogus\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted unknown selfdev approval")
	}
}

func TestHomeRespectsEnv(t *testing.T) {
	t.Setenv("WAFFLE_HOME", "/tmp/waffle-test-home")
	h, err := Home()
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	if h != "/tmp/waffle-test-home" {
		t.Errorf("Home = %q, want WAFFLE_HOME value", h)
	}
}

func TestPathPrecedence(t *testing.T) {
	// Reset process override so cases do not leak across subtests.
	SetConfigPath("")
	t.Cleanup(func() { SetConfigPath("") })

	t.Setenv("WAFFLE_HOME", "/tmp/waffle-path-home")
	t.Setenv("WAFFLE_CONFIG", "")

	// Default: $WAFFLE_HOME/config.toml
	got, err := Path()
	if err != nil {
		t.Fatalf("Path default: %v", err)
	}
	want := filepath.Join("/tmp/waffle-path-home", "config.toml")
	if got != want {
		t.Errorf("Path default = %q, want %q", got, want)
	}

	// Env overrides default.
	t.Setenv("WAFFLE_CONFIG", "/custom/via-env.toml")
	got, err = Path()
	if err != nil {
		t.Fatalf("Path env: %v", err)
	}
	if got != "/custom/via-env.toml" {
		t.Errorf("Path env = %q, want /custom/via-env.toml", got)
	}

	// SetConfigPath (CLI flag) overrides env.
	SetConfigPath("/custom/via-flag.toml")
	got, err = Path()
	if err != nil {
		t.Fatalf("Path flag: %v", err)
	}
	if got != "/custom/via-flag.toml" {
		t.Errorf("Path flag = %q, want /custom/via-flag.toml", got)
	}

	// Clearing override restores env precedence.
	SetConfigPath("")
	got, err = Path()
	if err != nil {
		t.Fatalf("Path after clear: %v", err)
	}
	if got != "/custom/via-env.toml" {
		t.Errorf("Path after clear = %q, want /custom/via-env.toml", got)
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv("WAFFLE_HOME", "/tmp/waffle-resolve-home")
	t.Setenv("WAFFLE_CONFIG", "/env/config.toml")

	got, err := ResolvePath("/flag/config.toml")
	if err != nil {
		t.Fatalf("ResolvePath flag: %v", err)
	}
	if got != "/flag/config.toml" {
		t.Errorf("ResolvePath flag = %q, want /flag/config.toml", got)
	}

	got, err = ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath env: %v", err)
	}
	if got != "/env/config.toml" {
		t.Errorf("ResolvePath env = %q, want /env/config.toml", got)
	}

	t.Setenv("WAFFLE_CONFIG", "")
	got, err = ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath default: %v", err)
	}
	want := filepath.Join("/tmp/waffle-resolve-home", "config.toml")
	if got != want {
		t.Errorf("ResolvePath default = %q, want %q", got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPolicyDefaults(t *testing.T) {
	cfg := Default() // Sandbox.Mode = "host", no groups

	main := cfg.AgentPolicy(GroupMain)
	if main.Mode != "host" {
		t.Errorf("main mode = %q, want host", main.Mode)
	}
	if len(main.Deny) != 0 {
		t.Errorf("main deny = %v, want none", main.Deny)
	}

	// The unattended cron tier denies host bash by default, even with no
	// [agent.group.cron] configured.
	cron := cfg.AgentPolicy(GroupCron)
	if cron.Mode != "host" {
		t.Errorf("cron mode = %q, want host (inherits [sandbox])", cron.Mode)
	}
	if !contains(cron.Deny, "bash") {
		t.Errorf("cron deny = %v, want it to include bash", cron.Deny)
	}
	// Issue intake shares the restricted unattended defaults (#51).
	issue := cfg.AgentPolicy(GroupIssue)
	if !contains(issue.Deny, "bash") || !contains(issue.Deny, "remember") || !contains(issue.Deny, "memory_update") || !contains(issue.Deny, "distill_skill") || !contains(issue.Deny, "workspace_update") {
		t.Errorf("issue deny = %v, want bash/remember/memory_update/distill_skill/workspace_update", issue.Deny)
	}
	// Multi-party channel chats share the same restricted defaults (#34).
	group := cfg.AgentPolicy(GroupGroup)
	if !contains(group.Deny, "bash") || !contains(group.Deny, "remember") || !contains(group.Deny, "memory_update") || !contains(group.Deny, "distill_skill") || !contains(group.Deny, "workspace_update") {
		t.Errorf("group deny = %v, want bash/remember/memory_update/distill_skill/workspace_update", group.Deny)
	}

	// An unknown group falls back to the global sandbox policy (no bash deny).
	other := cfg.AgentPolicy("adhoc")
	if contains(other.Deny, "bash") {
		t.Errorf("unknown group denied bash unexpectedly: %v", other.Deny)
	}
}

// The host-side GitHub tools must be nameable in profiles so the Desk
// profile editor can offer them (issue #252).
func TestProfileToolNamesIncludesGitHubHostTools(t *testing.T) {
	want := []string{"github_pr", "github_pr_get", "github_pr_diff", "github_pr_comments", "github_comment", "github_checks", "github_issue_get"}
	names := ProfileToolNames()
	for _, name := range want {
		if !ValidProfileTool(name) {
			t.Errorf("%q is not a valid profile tool", name)
		}
		found := false
		for _, n := range names {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProfileToolNames missing %q", name)
		}
	}
}

// github_comment is a public, permanent publish action: denied by default for
// the unattended cron/issue/group tiers, unless an explicit tool policy opts
// out (#252).
func TestGitHubCommentDeniedByDefaultForRestrictedTiers(t *testing.T) {
	cfg := Default()
	for _, group := range []string{GroupCron, GroupIssue, GroupGroup} {
		pol := cfg.AgentPolicy(group)
		if !contains(pol.Deny, "github_comment") {
			t.Errorf("%s deny = %v, want github_comment denied by default", group, pol.Deny)
		}
	}
	if contains(cfg.AgentPolicy(GroupMain).Deny, "github_comment") {
		t.Error("main tier must keep github_comment available")
	}
	// An explicit group tool policy opts out of the defaults, so an operator
	// can explicitly allow the write tool for a tier.
	cfg.Agent.Groups = map[string]AgentGroup{
		GroupCron: {Tools: ToolPolicy{Allow: []string{"github_comment"}}},
	}
	if contains(cfg.AgentPolicy(GroupCron).Deny, "github_comment") {
		t.Error("explicit allow for cron must lift the default github_comment deny")
	}
}

func TestIntakeAndHooksConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[[intake.github]]
repo = "acme/widgets"
label = "agent-ok"
max_concurrency = 2
deliver = "telegram:1"
poll_interval = "30s"

[workspace.hooks]
after_create = "go mod download"
before_run = "git status"
timeout = "2m"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Intake.GitHub) != 1 || cfg.Intake.GitHub[0].Repo != "acme/widgets" || cfg.Intake.GitHub[0].MaxConcurrency != 2 {
		t.Fatalf("intake = %#v", cfg.Intake)
	}
	if cfg.Workspace.Hooks.AfterCreate != "go mod download" || cfg.Workspace.Hooks.Timeout != "2m" {
		t.Fatalf("hooks = %#v", cfg.Workspace.Hooks)
	}
}

func TestIntakeConfigRejectsBadRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[[intake.github]]
repo = "not-a-repo"
max_concurrency = 1
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func TestAgentPolicyExplicitGroupWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[sandbox]
mode = "host"

[agent.group.cron]
sandbox = "docker"
[agent.group.cron.tools]
deny = ["fetch"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cron := cfg.AgentPolicy(GroupCron)
	if cron.Mode != "docker" {
		t.Errorf("cron mode = %q, want docker (explicit)", cron.Mode)
	}
	// An explicit tool policy replaces the default: bash is no longer force-
	// denied, but the operator's own deny (fetch) applies.
	if !contains(cron.Deny, "fetch") {
		t.Errorf("cron deny = %v, want fetch", cron.Deny)
	}
	if contains(cron.Deny, "bash") {
		t.Errorf("explicit cron policy should not carry the default bash deny: %v", cron.Deny)
	}
}

// TestAgentPolicyFileRoots covers the file-tool boundary (#269): global roots
// apply everywhere, a group may narrow them, and the restricted tiers fall
// back to the work dir rather than running unbounded.
func TestAgentPolicyFileRoots(t *testing.T) {
	tests := []struct {
		name  string
		toml  string
		group string
		want  []string
	}{
		{
			name:  "unset stays unbounded",
			toml:  "[sandbox]\nmode = \"host\"\n",
			group: GroupMain,
		},
		{
			name:  "global roots apply",
			toml:  "[sandbox]\nmode = \"host\"\nfile_roots = [\"/srv/work\"]\n",
			group: GroupMain,
			want:  []string{"/srv/work"},
		},
		{
			name:  "group narrows",
			toml:  "[sandbox]\nmode = \"host\"\nfile_roots = [\"/srv/work\"]\n\n[agent.group.cron.tools]\nfile_roots = [\"/srv/work/cron\"]\n",
			group: GroupCron,
			want:  []string{"/srv/work/cron"},
		},
		{
			name:  "restricted tier defaults to work dir",
			toml:  "[sandbox]\nmode = \"host\"\nwork_dir = \"/srv/work\"\n",
			group: GroupCron,
			want:  []string{"/srv/work"},
		},
		{
			name:  "owner tier keeps no default boundary",
			toml:  "[sandbox]\nmode = \"host\"\nwork_dir = \"/srv/work\"\n",
			group: GroupMain,
		},
		{
			name:  "no work dir means no default boundary",
			toml:  "[sandbox]\nmode = \"host\"\n",
			group: GroupCron,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tc.toml)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.AgentPolicy(tc.group).FileRoots
			if len(got) != len(tc.want) {
				t.Fatalf("file roots = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("file roots = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestAgentPolicyCronSandboxOnlyKeepsBashDeny guards the regression where
// configuring [agent.group.cron] just to set the sandbox mode (no tool policy)
// silently dropped the default bash deny and re-enabled host shell.
func TestAgentPolicyCronSandboxOnlyKeepsBashDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[sandbox]
mode = "host"

[agent.group.cron]
sandbox = "host"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cron := cfg.AgentPolicy(GroupCron)
	if !contains(cron.Deny, "bash") {
		t.Errorf("cron with only a sandbox override dropped the default bash deny: %v", cron.Deny)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestDefaultNoProfilesIsMain(t *testing.T) {
	cfg := Default()
	p, ok := cfg.Profile("")
	if !ok {
		t.Fatal("default profile should resolve")
	}
	if p.Model != "" || p.Sandbox != "" || len(p.Tools.Allow) != 0 {
		t.Fatalf("zero main profile expected, got %+v", p)
	}
	// Missing file = defaults, same effective main posture.
	loaded, err := Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Profile("main"); !ok {
		t.Fatal("main missing")
	}
}

func TestProfilesLoadRegistryAndDenyOverridesAllowStar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[agent.profile.reviewer]
system = "review carefully"
model = "claude-review"
sandbox = "docker"
max_tokens = 1000
max_iterations = 20
allowed_children = ["reader"]
[agent.profile.reviewer.tools]
allow = ["*"]
deny = ["bash", "write_file"]

[agent.profile.reader]
system = "read only"
[agent.profile.reader.tools]
allow = ["read_file", "search"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Agent.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(cfg.Agent.Profiles))
	}
	rev, ok := cfg.Profile("reviewer")
	if !ok || rev.Model != "claude-review" || rev.Sandbox != "docker" || rev.MaxTokens != 1000 || rev.MaxIterations != 20 {
		t.Fatalf("reviewer = %+v ok=%v", rev, ok)
	}
	// Deny wins over allow=["*"] (policy semantics tested in tool package; config stores both).
	if len(rev.Tools.Allow) != 1 || rev.Tools.Allow[0] != "*" || !contains(rev.Tools.Deny, "bash") {
		t.Fatalf("reviewer tools = %+v", rev.Tools)
	}
}

func TestProfileInvalidNamesRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// Quoted invalid keys
	cases := []string{
		`[agent.profile."bad name"]` + "\nsystem = \"x\"\n",
		`[agent.profile."../escape"]` + "\nsystem = \"x\"\n",
		`[agent.profile."a;rm"]` + "\nsystem = \"x\"\n",
		`[agent.profile."` + strings.Repeat("a", 65) + `"]` + "\nsystem = \"x\"\n",
	}
	for _, body := range cases {
		writeFile(t, path, body)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid profile:\n%s", body)
		}
	}
	// Valid short slug
	writeFile(t, path, "[agent.profile.ok]\nsystem = \"x\"\n")
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestProfileDuplicateNamesRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[agent.profile.dup]
system = "a"
[agent.profile.dup]
system = "b"
`)
	// BurntSushi/toml rejects redefined table keys before our validator runs.
	_, err := Load(path)
	if err == nil {
		t.Fatal("want error for duplicate profile table")
	}
	msg := err.Error()
	if !strings.Contains(msg, "already been defined") && !strings.Contains(msg, "duplicate") {
		t.Fatalf("want duplicate/already-defined error, got %v", err)
	}
}

func TestProfileUnknownToolRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[agent.profile.reviewer.tools]
allow = ["not_a_real_tool"]
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "reviewer") || !strings.Contains(err.Error(), "not_a_real_tool") {
		t.Fatalf("want unknown tool with profile name, got %v", err)
	}
}

func TestProfileNestedUnknownKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[agent.profile.reviewer]
system = "x"
typo_key = true
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("want unknown key error, got %v", err)
	}
}

func TestProfileModelAndLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[provider]
model = "default-model"
utility_model = "cheap"
max_tokens = 100

[agent.profile.special]
model = "special-model"
max_tokens = 50
max_iterations = 7
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Profile("special")
	if p.Model != "special-model" || p.MaxTokens != 50 || p.MaxIterations != 7 {
		t.Fatalf("%+v", p)
	}
	// Utility model stays on provider, not profile.
	if cfg.Provider.UtilityModel != "cheap" {
		t.Fatal(cfg.Provider.UtilityModel)
	}
	// Negative rejected
	writeFile(t, path, "[agent.profile.x]\nmax_tokens = -1\n")
	if _, err := Load(path); err == nil {
		t.Fatal("negative max_tokens accepted")
	}
}

func TestUsageAlertThresholdConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[limits]\ntokens_per_day = 1000\nalert_threshold_percent = 65\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.AlertThresholdPercent != 65 {
		t.Fatalf("alert threshold = %d, want 65", cfg.Limits.AlertThresholdPercent)
	}
	writeFile(t, path, "[limits]\nalert_threshold_percent = 101\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "alert_threshold_percent") {
		t.Fatalf("invalid alert threshold error = %v", err)
	}
}

func TestProfileUtilityModelSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[provider]
model = "main-model"
utility_model = "cheap-model"

[agent.profile.with-default]
model = "default"
system = "ok"

[agent.profile.with-utility]
model = "utility"
system = "ok"

[agent.profile.with-explicit]
model = "claude-special"
system = "ok"

[agent.profile.empty-system]
system = ""
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// default class
	p, _ := cfg.Profile("with-default")
	got, err := cfg.ResolveProfileModel(p)
	if err != nil || got != "main-model" {
		t.Fatalf("default → %q %v, want main-model", got, err)
	}
	// empty model also default
	p, _ = cfg.Profile("empty-system")
	got, err = cfg.ResolveProfileModel(p)
	if err != nil || got != "main-model" {
		t.Fatalf("empty model → %q %v", got, err)
	}
	// utility class
	p, _ = cfg.Profile("with-utility")
	got, err = cfg.ResolveProfileModel(p)
	if err != nil || got != "cheap-model" {
		t.Fatalf("utility → %q %v, want cheap-model", got, err)
	}
	// explicit
	p, _ = cfg.Profile("with-explicit")
	got, err = cfg.ResolveProfileModel(p)
	if err != nil || got != "claude-special" {
		t.Fatalf("explicit → %q %v", got, err)
	}
	// missing utility_model is a hard error
	writeFile(t, path, `
[provider]
model = "main-model"

[agent.profile.needs-util]
model = "utility"
`)
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = cfg2.Profile("needs-util")
	if _, err := cfg2.ResolveProfileModel(p); err == nil || !strings.Contains(err.Error(), "utility_model") {
		t.Fatalf("want utility_model error, got %v", err)
	}
}

func TestProfileEmptySystemAllowedMissingFileError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// Explicit empty system is valid config.
	writeFile(t, path, `
[agent.profile.empty]
system = ""
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Profile("empty")
	if !ok || p.System != "" {
		t.Fatalf("empty profile = %+v ok=%v", p, ok)
	}
	// loadProfileSystem is in main; here we only assert config accepts system="".
	// Missing prompt file is rejected at agent construction (see agent_group_test).
}

func TestProfileDocumentationAcceptance(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	plan := read("../../docs/plan.md")
	// README.md is intentionally a pointer now; profile documentation is
	// required from docs/plan.md and the example config instead.
	example := read("../../config.example.toml")

	for _, want := range []string{
		"Profiles are a **trust boundary in config**, not personality presets",
		"system prompt, model class, sandbox mode, tool",
		"allowed_children",
		"**#33 (agent-group trust tiers):**",
		"**#53 (repo WAFFLE.md / AGENT.md):**",
		"**#66 (action-level policy):**",
		"**#68 (working-set broadcast):**",
		"migration required**",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("docs/plan.md missing %q", want)
		}
	}
	// The effective-`main` and migration guarantees the README used to carry
	// are still required — from the durable docs, so removing them from docs
	// (or the code) trips CI rather than silently losing the guidance.
	for _, want := range []string{
		"trust boundary",
		"effective `main`",
		"migration required",
		"#33", "#53", "#66", "#68",
	} {
		if !strings.Contains(plan, want) && !strings.Contains(example, want) {
			t.Errorf("neither docs/plan.md nor config.example.toml documents %q", want)
		}
	}
	for _, want := range []string{"[agent.profile.main]", "[agent.profile.researcher]", "[agent.profile.reviewer]", "No change required"} {
		if !strings.Contains(example, want) {
			t.Errorf("config.example.toml missing %q", want)
		}
	}
}

func TestActiveDocsAndSourceDoNotReferenceWorkweaveRouter(t *testing.T) {
	paths := []string{
		"../../README.md",
		"../../docs/research.md",
		"../../docs/plan.md",
		"config.go",
		"../llm/openaip/openai.go",
	}
	forbidden := []string{
		"workweave/router",
		"weave-router",
		"weave router",
		"ollama/router",
		"router's two-tier key model",
	}

	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lower := strings.ToLower(string(body))
		for _, phrase := range forbidden {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s contains retired Router reference %q", path, phrase)
			}
		}
	}
}

func TestIntakeConfigRejectsBadConcurrencyAndLabelRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `[[intake.github]]
repo = "owner/name"
label = "agent-ok"
max_concurrency = 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Intake.GitHub[0].Label != "agent-ok" || cfg.Intake.GitHub[0].MaxConcurrency != 3 {
		t.Fatalf("%+v", cfg.Intake.GitHub[0])
	}
	writeFile(t, path, `[[intake.github]]
repo = "owner/name"
max_concurrency = 0
`)
	if _, err := Load(path); err == nil {
		t.Fatal("max_concurrency 0 accepted")
	}
}

func TestPolicyRulesParseAndUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[sandbox]
enforcer = "feedback"

[[policy.rule]]
name = "no-rm"
tool = "bash"
match = "rm -rf"
action = "deny"
guidance = "use safer cleanup"

[[policy.rule]]
name = "no-curl-http"
tool = "bash"
regex = "curl\\s+http://"
action = "deny"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Enforcer != "feedback" {
		t.Fatalf("enforcer = %q", cfg.Sandbox.Enforcer)
	}
	if len(cfg.Policy.Rule) != 2 {
		t.Fatalf("rules = %d", len(cfg.Policy.Rule))
	}
	if cfg.Policy.Rule[0].Name != "no-rm" || cfg.Policy.Rule[0].Action != "deny" {
		t.Fatalf("%+v", cfg.Policy.Rule[0])
	}
	// Unknown key on a rule table is rejected.
	writeFile(t, path, `
[[policy.rule]]
name = "x"
tool = "bash"
action = "deny"
not_a_real_key = true
`)
	if _, err := Load(path); err == nil {
		t.Fatal("unknown policy.rule key accepted")
	}
	// Bad action rejected.
	writeFile(t, path, `
[[policy.rule]]
name = "x"
tool = "bash"
action = "maybe"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("bad action accepted")
	}
	// require without requires rejected.
	writeFile(t, path, `
[[policy.rule]]
name = "need-tests"
tool = "bash"
match = "git commit"
action = "require"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("require without requires accepted")
	}
	// require with requires accepted.
	writeFile(t, path, `
[[policy.rule]]
name = "go-test-green"
tool = "bash"
match = "go test"
action = "allow"

[[policy.rule]]
name = "tests-before-commit"
tool = "bash"
match = "git commit"
action = "require"
requires = "go-test-green"
guidance = "run go test after edits"
`)
	cfgReq, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgReq.Policy.Rule) != 2 || cfgReq.Policy.Rule[1].Requires != "go-test-green" {
		t.Fatalf("require rules = %+v", cfgReq.Policy.Rule)
	}
	// Bad enforcer rejected.
	writeFile(t, path, "[sandbox]\nenforcer = \"hard\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("bad enforcer accepted")
	}
}

func TestPolicyRulesRejectEmptySelectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// Rule with only name+action (no tool/match/regex) must fail closed at Load.
	writeFile(t, path, `
[[policy.rule]]
name = "x"
action = "deny"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("empty tool+match+regex policy rule accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "x") {
		t.Fatalf("error should name the rule: %q", msg)
	}
	if !strings.Contains(msg, "tool") || !strings.Contains(msg, "match") || !strings.Contains(msg, "regex") {
		t.Fatalf("error should mention tool/match/regex: %q", msg)
	}
}

func TestUsesDocker(t *testing.T) {
	// Global host mode, no groups -> no docker.
	cfg := Default()
	if cfg.UsesDocker() {
		t.Error("default (host) reports docker in use")
	}

	// A group opting into docker while global stays host must be detected,
	// so doctor's runner check still fires (#33 tiering + #42 guard).
	cfg.Agent.Groups = map[string]AgentGroup{"cron": {Sandbox: "docker"}}
	if !cfg.UsesDocker() {
		t.Error("group sandbox=docker not detected while global mode is host")
	}

	// Global docker mode alone is enough.
	cfg2 := Default()
	cfg2.Sandbox.Mode = "docker"
	if !cfg2.UsesDocker() {
		t.Error("global docker mode not detected")
	}
}

func TestCodeIntelMCPValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// Host codeintel without allow_host_mcp fails.
	writeFile(t, path, `
[[mcp]]
name = "codeintel"
command = "/bin/true"
execution = "host"
tools = ["code_find_symbol"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected host codeintel rejection")
	}
	// Sandbox codeintel OK at config load (runtime may fall back).
	writeFile(t, path, `
[[mcp]]
name = "codeintel"
command = "/bin/true"
execution = "sandbox"
tools = ["code_find_symbol"]
`)
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	// Secret env rejected.
	writeFile(t, path, `
[codeintel]
allow_host_mcp = true
[[mcp]]
name = "codeintel"
command = "/bin/true"
execution = "host"
tools = ["code_find_symbol"]
env = ["GITHUB_TOKEN"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected secret env rejection")
	}
}
