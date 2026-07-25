package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/observability"
)

type connectionHealthStub struct {
	health observability.Health
	err    error
}

func (s connectionHealthStub) HealthSnapshot(context.Context, time.Duration) (observability.Health, error) {
	return s.health, s.err
}

func TestConnectionsJSONIsAllowlistedAndRedacted(t *testing.T) {
	private := []string{
		"secret://providers/primary",
		"sk-super-private",
		"https://provider.private.example",
		"secret://telegram/token",
		"https://telegram.private.example",
		"node",
		"--dangerous-raw-command",
		"/srv/private/mcp.js",
		"PRIVATE_TOKEN",
		"private_tool",
		"private-group",
		"@/Users/private/system.md",
		"private-model",
		"bash",
		"rm -rf",
		"operator-authored-guidance",
		"/Users/private/work",
		"private.example",
		"/srv/private/hook.sh",
	}
	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{
			"primary": {
				Type:    "openai",
				APIKey:  private[0] + private[1],
				BaseURL: private[2],
			},
		},
		Channel: config.Channels{Telegram: config.Telegram{
			Enabled: true,
			Token:   private[3],
			BaseURL: private[4],
		}},
		MCP: []config.MCPServer{{
			Name:    "filesystem",
			Command: private[5],
			Args:    []string{private[6], private[7]},
			Env:     []string{private[8]},
			Tools:   []string{private[9]},
			Groups:  []string{private[10]},
		}},
		Sandbox: config.Sandbox{
			Mode:    "host",
			WorkDir: private[16],
			Allow:   []string{private[12]},
			Deny:    []string{private[13]},
		},
		Workspace: config.Workspace{
			Egress:    "allowlist",
			Allowlist: []string{private[17]},
			Hooks: config.WorkspaceHooks{
				AfterCreate: private[18],
			},
		},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				"cron": {
					Sandbox: "docker",
					Tools: config.ToolPolicy{
						Allow:        []string{private[9]},
						Deny:         []string{private[12]},
						DenyPrefixes: []string{private[13]},
						Guidance:     private[14],
					},
				},
			},
			Profiles: map[string]config.AgentProfile{
				"review": {
					System:       private[11],
					Model:        private[12],
					Sandbox:      "docker",
					Tools:        config.ToolPolicy{Allow: []string{private[9]}, Guidance: private[14]},
					DenyPrefixes: []string{private[13]},
					Guidance:     private[14],
				},
			},
		},
		Policy: config.PolicyConfig{Rule: []config.PolicyRule{{
			Name:     "private-rule",
			Tool:     private[9],
			Match:    private[13],
			Guidance: private[14],
		}}},
	}
	source := NewConnectionSource(cfg, connectionHealthStub{health: observability.Health{
		Healthy:  false,
		Database: false,
		Adapters: map[string]observability.AdapterHealth{
			"telegram": {Stale: false},
		},
	}}, nil)
	mux := http.NewServeMux()
	RegisterConnectionsRoutes(mux, source)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/desk/connections", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	body := response.Body.String()
	for _, value := range private {
		if strings.Contains(body, value) {
			t.Errorf("response leaked private value %q: %s", value, body)
		}
	}

	var raw []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	allowedKeys := map[string]bool{
		"name": true, "kind": true, "status": true, "profile": true,
		"sandbox_mode": true, "egress": true, "guidance": true,
		"label": true, "concurrency": true,
	}
	for index, record := range raw {
		for key := range record {
			if !allowedKeys[key] {
				t.Errorf("record %d exposed non-allowlisted key %q", index, key)
			}
		}
	}

	var got []ConnectionView
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []ConnectionView{
		{Name: "primary", Kind: "provider", Status: "configured"},
		{Name: "telegram", Kind: "adapter", Status: "healthy"},
		{Name: "filesystem", Kind: "mcp", Status: "configured"},
		{Name: "cron", Kind: "profile", Status: "configured", Profile: "cron", SandboxMode: "docker", Egress: "restricted", Guidance: "Runs in a sandbox."},
		{Name: "review", Kind: "profile", Status: "configured", Profile: "review", SandboxMode: "docker", Egress: "restricted", Guidance: "Runs in a sandbox."},
		{Name: "github", Kind: "github", Status: "unconfigured", Guidance: "Configure [github.app] to give workspaces git access."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("connections = %#v, want %#v", got, want)
	}
}

type stubConnectionSource struct {
	records []ConnectionView
}

func (s stubConnectionSource) Connections(context.Context) ([]ConnectionView, error) {
	return s.records, nil
}

func TestConnectionsReturnsStableEmptyArray(t *testing.T) {
	mux := http.NewServeMux()
	RegisterConnectionsRoutes(mux, stubConnectionSource{})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/desk/connections", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace(response.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

// An empty config still reports GitHub so an operator can tell "not
// configured" from "not surfaced at all" (#182 AC1).
func TestConnectionsAlwaysReportsGitHubRecord(t *testing.T) {
	got, err := NewConnectionSource(config.Config{}, nil, nil).Connections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []ConnectionView{{
		Name:     "github",
		Kind:     "github",
		Status:   "unconfigured",
		Guidance: "Configure [github.app] to give workspaces git access.",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("connections = %#v, want %#v", got, want)
	}
}

type githubProbeStub struct {
	err   error
	calls int
}

func (p *githubProbeStub) Verify(context.Context) error {
	p.calls++
	return p.err
}

// githubAppConfig is a fully populated [github.app] whose every field is a
// distinctive canary, so AC2 can assert none of it reaches the payload.
func githubAppConfig() config.Config {
	return config.Config{GitHub: config.GitHub{App: config.GitHubApp{
		AppID:          987654321,
		InstallationID: 123456789,
		PrivateKey:     "secret://github/app-private-key-canary",
		BaseURL:        "https://github.private.example/api/v3",
	}}}
}

func TestConnectionsReportsGitHubStates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        config.Config
		probe      *githubProbeStub
		wantStatus string
	}{
		{name: "not configured", cfg: config.Config{}, wantStatus: "unconfigured"},
		{
			name: "configured without probe",
			cfg:  githubAppConfig(),
			// A missing probe is not evidence of failure: report configured.
			wantStatus: "configured",
		},
		{
			name:       "configured but failing",
			cfg:        githubAppConfig(),
			probe:      &githubProbeStub{err: errors.New("secret://github/app-private-key-canary is invalid")},
			wantStatus: "stale",
		},
		{name: "healthy", cfg: githubAppConfig(), probe: &githubProbeStub{}, wantStatus: "healthy"},
		{
			name: "incomplete app is not configured",
			cfg: config.Config{GitHub: config.GitHub{App: config.GitHubApp{
				PrivateKey: "secret://github/app-private-key-canary",
			}}},
			probe:      &githubProbeStub{},
			wantStatus: "unconfigured",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probe GitHubProbe
			if tc.probe != nil {
				probe = tc.probe
			}
			mux := http.NewServeMux()
			RegisterConnectionsRoutes(mux, NewConnectionSource(tc.cfg, nil, probe))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/desk/connections", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}

			var got []ConnectionView
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Kind != "github" {
				t.Fatalf("connections = %#v, want a single github record", got)
			}
			if got[0].Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got[0].Status, tc.wantStatus)
			}

			// AC2: no app ID, installation ID, private key, token, or base URL.
			body := response.Body.String()
			for _, canary := range []string{
				"987654321", "123456789",
				"app-private-key-canary", "secret://",
				"github.private.example", "api/v3",
			} {
				if strings.Contains(body, canary) {
					t.Errorf("payload leaked %q: %s", canary, body)
				}
			}
		})
	}
}

func TestConnectionsCachesGitHubProbe(t *testing.T) {
	probe := &githubProbeStub{}
	source := NewConnectionSource(githubAppConfig(), nil, probe)
	for range 3 {
		if _, err := source.Connections(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1 within the TTL", probe.calls)
	}

	// Past the TTL the probe runs again, so a recovered installation stops
	// reporting stale without a restart.
	concrete, ok := source.(*configuredConnectionSource)
	if !ok {
		t.Fatalf("source type = %T", source)
	}
	concrete.now = func() time.Time { return time.Now().Add(2 * githubProbeTTL) }
	if _, err := source.Connections(t.Context()); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 2 {
		t.Fatalf("probe calls = %d, want 2 after the TTL", probe.calls)
	}
}

func TestConnectionsProjectsIntakeWatchersWithoutSecrets(t *testing.T) {
	cfg := config.Config{Intake: config.Intake{GitHub: []config.GitHubWatch{
		{
			Repo:           "owner/zulu",
			Label:          "waffle",
			MaxConcurrency: 3,
			Deliver:        "telegram:private-canary",
			PollInterval:   "37s",
			Token:          "secret://intake/token-canary",
		},
		{
			Repo:           "owner/alpha",
			Label:          "agent",
			MaxConcurrency: 1,
			Deliver:        "telegram:private-canary",
			Token:          "secret://intake/token-canary",
		},
		// An unnamed watcher is not a connection: it is skipped.
		{Label: "orphan"},
	}}}
	mux := http.NewServeMux()
	RegisterConnectionsRoutes(mux, NewConnectionSource(cfg, nil, nil))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/desk/connections", nil))

	var got []ConnectionView
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	intake := make([]ConnectionView, 0, len(got))
	for _, record := range got {
		if record.Kind == "intake" {
			intake = append(intake, record)
		}
	}
	want := []ConnectionView{
		{
			Name: "owner/alpha", Kind: "intake", Status: "configured",
			Label: "agent", Concurrency: 1,
			Guidance: "Issues matching this label are picked up by the issue profile.",
		},
		{
			Name: "owner/zulu", Kind: "intake", Status: "configured",
			Label: "waffle", Concurrency: 3,
			Guidance: "Issues matching this label are picked up by the issue profile.",
		},
	}
	if !reflect.DeepEqual(intake, want) {
		t.Fatalf("intake = %#v, want %#v", intake, want)
	}
	for _, canary := range []string{"private-canary", "token-canary", "37s", "orphan"} {
		if strings.Contains(response.Body.String(), canary) {
			t.Errorf("payload leaked %q: %s", canary, response.Body.String())
		}
	}
}

func TestConnectionsUsesAdapterHealthOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health map[string]observability.AdapterHealth
		status string
	}{
		{name: "healthy", health: map[string]observability.AdapterHealth{"telegram": {Stale: false}}, status: "healthy"},
		{name: "stale", health: map[string]observability.AdapterHealth{"telegram": {Stale: true}}, status: "stale"},
		{name: "not registered", health: map[string]observability.AdapterHealth{}, status: "configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Providers: map[string]config.ProviderConnection{"provider": {Type: "openai"}},
				Channel:   config.Channels{Telegram: config.Telegram{Enabled: true}},
				MCP:       []config.MCPServer{{Name: "tools"}},
			}
			source := NewConnectionSource(cfg, connectionHealthStub{health: observability.Health{
				Healthy:  tc.name != "stale",
				Database: tc.name == "healthy",
				Adapters: tc.health,
			}}, nil)
			got, err := source.Connections(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Status != "configured" || got[1].Status != tc.status || got[2].Status != "configured" {
				t.Fatalf("statuses = %q, %q, %q", got[0].Status, got[1].Status, got[2].Status)
			}
		})
	}
}

func TestConnectionsSortsRecordsAndUsesClosedSummaries(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{
			"zulu":  {Type: "openai"},
			"alpha": {Type: "anthropic"},
		},
		MCP: []config.MCPServer{{Name: "zeta"}, {Name: "beta"}},
		Sandbox: config.Sandbox{
			Mode: "unexpected-private-mode",
		},
		Workspace: config.Workspace{Egress: "unexpected-private-egress"},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				"zeta":  {},
				"alpha": {},
			},
			Profiles: map[string]config.AgentProfile{
				"review": {Sandbox: "unexpected-profile-mode"},
			},
		},
	}
	source := NewConnectionSource(cfg, nil, nil)

	first, err := source.Connections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Connections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("records are not deterministic: %#v then %#v", first, second)
	}
	names := make([]string, 0, len(first))
	for _, record := range first {
		names = append(names, record.Name)
		if record.Kind == "profile" {
			if record.SandboxMode != "host" || record.Egress != "disabled" || record.Guidance != "Tool policy is enforced." {
				t.Errorf("unsafe profile summary: %#v", record)
			}
		}
	}
	wantNames := []string{"alpha", "zulu", "beta", "zeta", "alpha", "review", "zeta", "github"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("names = %#v, want %#v", names, wantNames)
	}
}

func TestConnectionsProfileSandboxUsesOwnGroupNotMain(t *testing.T) {
	cfg := config.Config{
		Sandbox: config.Sandbox{Mode: "host"},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				config.GroupMain: {Sandbox: "host"},
				config.GroupCron: {Sandbox: "docker"},
			},
			Profiles: map[string]config.AgentProfile{
				// Same name as the cron group: must keep cron's docker mode,
				// not collapse to GroupMain host (#155).
				config.GroupCron: {},
				// Profile-only posture inherits main, then applies override.
				"review": {Sandbox: "docker"},
				// Profile-only without override inherits main (host).
				"planner": {},
			},
		},
	}
	source := NewConnectionSource(cfg, nil, nil)
	got, err := source.Connections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]ConnectionView, len(got))
	for _, record := range got {
		if record.Kind == "profile" {
			byName[record.Name] = record
		}
	}
	if record := byName[config.GroupCron]; record.SandboxMode != "docker" || record.Guidance != "Runs in a sandbox." {
		t.Fatalf("cron summary = %#v, want docker from group policy", record)
	}
	if record := byName["review"]; record.SandboxMode != "docker" {
		t.Fatalf("review summary = %#v, want docker from profile override", record)
	}
	if record := byName["planner"]; record.SandboxMode != "host" || record.Guidance != "Tool policy is enforced." {
		t.Fatalf("planner summary = %#v, want host inherited from main", record)
	}
	if record := byName[config.GroupMain]; record.SandboxMode != "host" {
		t.Fatalf("main summary = %#v, want host", record)
	}
}

func TestConnectionsFailureIsClosedAndRoutesAreGETOnly(t *testing.T) {
	mux := http.NewServeMux()
	RegisterConnectionsRoutes(mux, NewConnectionSource(
		config.Config{Channel: config.Channels{Telegram: config.Telegram{Enabled: true}}},
		connectionHealthStub{err: errors.New("secret://health/private")},
		nil,
	))

	failed := httptest.NewRecorder()
	mux.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/api/v1/desk/connections", nil))
	if failed.Code != http.StatusServiceUnavailable || strings.TrimSpace(failed.Body.String()) != "connections_unavailable" {
		t.Fatalf("failure status=%d body=%q", failed.Code, failed.Body.String())
	}
	if strings.Contains(failed.Body.String(), "secret://health/private") {
		t.Fatal("source error leaked into response")
	}

	for _, tc := range []struct {
		request *http.Request
		want    int
	}{
		{request: httptest.NewRequest(http.MethodPost, "/api/v1/desk/connections", strings.NewReader(`{}`)), want: http.StatusMethodNotAllowed},
		{request: httptest.NewRequest(http.MethodGet, "/api/v1/desk/connections/extra", nil), want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, tc.request)
		if response.Code != tc.want {
			t.Errorf("%s %s status = %d, want %d", tc.request.Method, tc.request.URL.Path, response.Code, tc.want)
		}
	}
}
