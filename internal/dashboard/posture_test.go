package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/policy"
)

type postureAuditStub struct {
	entries     []policy.AuditEntry
	err         error
	gotSession  string
	gotLimit    int
	callCount   int
	returnEmpty bool
}

func (s *postureAuditStub) RecentDenials(_ context.Context, session string, limit int) ([]policy.AuditEntry, error) {
	s.callCount++
	s.gotSession = session
	s.gotLimit = limit
	if s.err != nil {
		return nil, s.err
	}
	if s.returnEmpty {
		return nil, nil
	}
	return s.entries, nil
}

type postureRedactorStub struct{ secret string }

func (r postureRedactorStub) RedactExact(value string) string {
	if r.secret == "" {
		return value
	}
	return strings.ReplaceAll(value, r.secret, "[redacted]")
}

func postureTestConfig() config.Config {
	return config.Config{
		Sandbox: config.Sandbox{Mode: "host", Allow: []string{"bash", "read", "write"}},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				config.GroupMain: {Tools: config.ToolPolicy{
					Allow:        []string{"bash", "read", "write"},
					DenyPrefixes: []string{"rm -rf"},
					Guidance:     "Group guidance.",
				}},
			},
			Profiles: map[string]config.AgentProfile{
				"reviewer": {
					System:          "You review changes.",
					Model:           "primary",
					Sandbox:         "docker",
					Tools:           config.ToolPolicy{Allow: []string{"read"}, Deny: []string{"bash"}},
					DenyPrefixes:    []string{"git push"},
					MaxTokens:       4096,
					MaxIterations:   12,
					AllowedChildren: []string{"reader"},
				},
			},
		},
	}
}

func TestPostureProjectsSystemPromptAndLayers(t *testing.T) {
	cfgForTest := postureTestConfig()
	service := NewPostureService(&cfgForTest, nil, nil)
	view := service.Read("reviewer")

	if !view.Known {
		t.Fatal("configured profile reported as unknown")
	}
	if view.System.Source != config.SystemPromptInline {
		t.Fatalf("system source = %q, want inline", view.System.Source)
	}
	if view.System.Text != "You review changes." {
		t.Fatalf("system text = %q", view.System.Text)
	}
	if view.System.Path != "" {
		t.Fatalf("inline prompt reported a path %q", view.System.Path)
	}

	// AC2: the tiers are named and separate, not flattened into one list.
	names := make([]string, 0, len(view.Layers))
	for _, layer := range view.Layers {
		names = append(names, layer.Name)
	}
	if !reflect.DeepEqual(names, []string{"group", "profile", "repo"}) {
		t.Fatalf("layers = %v, want group, profile, repo", names)
	}
	byName := make(map[string]PostureLayerView, len(view.Layers))
	for _, layer := range view.Layers {
		byName[layer.Name] = layer
	}
	if !reflect.DeepEqual(byName["group"].Allow, []string{"bash", "read", "write"}) {
		t.Fatalf("group allow = %v", byName["group"].Allow)
	}
	if !reflect.DeepEqual(byName["profile"].Allow, []string{"read"}) {
		t.Fatalf("profile layer showed the running total, not its own: %v", byName["profile"].Allow)
	}
	if !reflect.DeepEqual(byName["profile"].DenyPrefixes, []string{"git push"}) {
		t.Fatalf("profile prefixes = %v", byName["profile"].DenyPrefixes)
	}
	if byName["repo"].Applied {
		t.Fatal("repo layer claimed to apply outside a workspace")
	}

	if view.Effective.SandboxMode != "docker" {
		t.Fatalf("effective sandbox = %q", view.Effective.SandboxMode)
	}
	if !reflect.DeepEqual(view.Effective.Allow, []string{"read"}) {
		t.Fatalf("effective allow = %v", view.Effective.Allow)
	}
	// A bash prefix keeps its space rather than being redacted wholesale.
	if !reflect.DeepEqual(view.Effective.DenyPrefixes, []string{"rm -rf", "git push"}) {
		t.Fatalf("effective prefixes = %v", view.Effective.DenyPrefixes)
	}

	want := PostureLimitsView{
		Model: "primary", MaxTokens: 4096, MaxIterations: 12,
		AllowedChildren: []string{"reader"},
	}
	if !reflect.DeepEqual(view.Limits, want) {
		t.Fatalf("limits = %+v, want %+v", view.Limits, want)
	}
}

func TestPostureResolvesFilePromptWithRelativeSourceOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "prompts", "cron.md"), []byte("Run unattended."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Agent: config.Agent{Profiles: map[string]config.AgentProfile{
		"cron": {System: "@prompts/cron.md"},
	}}}

	view := NewPostureService(&cfg, nil, nil).Read("cron")
	if view.System.Source != config.SystemPromptFile {
		t.Fatalf("source = %q, want file", view.System.Source)
	}
	if view.System.Text != "Run unattended." {
		t.Fatalf("text = %q", view.System.Text)
	}
	// AC1: the source is labelled. AC4: it is the only path here, and it is
	// relative, so the home location never reaches the browser.
	if view.System.Path != "prompts/cron.md" {
		t.Fatalf("path = %q, want prompts/cron.md", view.System.Path)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), home) {
		t.Fatalf("payload leaked the home path: %s", payload)
	}
}

func TestPostureReportsUnreadablePromptWithoutLeakingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	cfg := config.Config{Agent: config.Agent{Profiles: map[string]config.AgentProfile{
		"broken": {System: "@prompts/does-not-exist.md"},
	}}}

	view := NewPostureService(&cfg, nil, nil).Read("broken")
	if view.System.Error == "" {
		t.Fatal("unreadable prompt reported no error")
	}
	if view.System.Text != "" {
		t.Fatalf("unreadable prompt still returned text %q", view.System.Text)
	}
	if strings.Contains(view.System.Error, home) ||
		strings.Contains(view.System.Error, "does-not-exist") {
		t.Fatalf("error leaked the path: %q", view.System.Error)
	}
}

func TestPostureRedactsSecretsAndPathShapedIdentifiers(t *testing.T) {
	cfg := config.Config{
		Sandbox: config.Sandbox{Mode: "host"},
		Agent: config.Agent{
			Groups: map[string]config.AgentGroup{
				config.GroupMain: {Tools: config.ToolPolicy{
					// A host path must never pass as a tool name, and a prefix
					// naming one is withheld rather than shown (AC4).
					Allow:        []string{"read", "/Users/private/tool"},
					DenyPrefixes: []string{"git push", "/Users/private/script.sh --run"},
					Guidance:     "Ask before touching sk-super-private.",
				}},
			},
			Profiles: map[string]config.AgentProfile{
				"main": {System: "The key is sk-super-private."},
			},
		},
	}
	service := NewPostureService(&cfg, postureRedactorStub{secret: "sk-super-private"}, nil)
	view := service.Read("main")

	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, canary := range []string{"sk-super-private", "/Users/private"} {
		if strings.Contains(body, canary) {
			t.Errorf("posture leaked %q: %s", canary, body)
		}
	}

	group := view.Layers[0]
	if !reflect.DeepEqual(group.Allow, []string{"read", "[redacted]"}) {
		t.Fatalf("allow = %v, want the path-shaped entry withheld", group.Allow)
	}
	if !reflect.DeepEqual(group.DenyPrefixes, []string{"git push", "[redacted]"}) {
		t.Fatalf("prefixes = %v, want the readable prefix kept and the path withheld", group.DenyPrefixes)
	}
	if !strings.Contains(view.System.Text, "[redacted]") {
		t.Fatalf("system text kept the secret: %q", view.System.Text)
	}
}

func TestPostureUnknownProfileReportsInheritedPosture(t *testing.T) {
	cfgForTest := postureTestConfig()
	view := NewPostureService(&cfgForTest, nil, nil).Read("not-configured")
	if view.Known {
		t.Fatal("unknown profile reported as configured")
	}
	// It still describes what would happen, rather than erroring out.
	if view.Effective.SandboxMode != "host" {
		t.Fatalf("effective sandbox = %q, want the inherited host mode", view.Effective.SandboxMode)
	}
	if len(view.Layers) == 0 {
		t.Fatal("unknown profile returned no layers")
	}
}

func TestPostureTracesDenialsToTheirRule(t *testing.T) {
	audit := &postureAuditStub{entries: []policy.AuditEntry{
		{
			At: "2026-07-25T12:00:00Z", Session: "session-primary", Tool: "bash",
			Command: "git push --force", Rule: "no-force-push", Verdict: "deny",
			Detail: "Force pushes are refused on shared branches.",
		},
	}}
	cfgForTest := postureTestConfig()
	service := NewPostureService(&cfgForTest, nil, audit)

	snapshot, err := service.Denials(t.Context(), " session-primary ")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Denials) != 1 {
		t.Fatalf("denials = %#v", snapshot.Denials)
	}
	// AC3: the rule that refused the call is named alongside it.
	got := snapshot.Denials[0]
	if got.Rule != "no-force-push" || got.Tool != "bash" || got.Verdict != "deny" {
		t.Fatalf("denial = %+v", got)
	}
	if got.Command != "git push --force" {
		t.Fatalf("command = %q", got.Command)
	}
	if got.Detail != "Force pushes are refused on shared branches." {
		t.Fatalf("detail = %q", got.Detail)
	}
	if audit.gotSession != "session-primary" {
		t.Fatalf("session = %q, want it trimmed", audit.gotSession)
	}
	if audit.gotLimit != postureDenialLimit {
		t.Fatalf("limit = %d, want %d", audit.gotLimit, postureDenialLimit)
	}
}

func TestPostureDenialsAreOptionalAndFailClosed(t *testing.T) {
	// No audit source is not an error: the trace is additive.
	cfgForTest := postureTestConfig()
	snapshot, err := NewPostureService(&cfgForTest, nil, nil).Denials(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Denials == nil || len(snapshot.Denials) != 0 {
		t.Fatalf("denials = %#v, want a canonical empty slice", snapshot.Denials)
	}

	audit := &postureAuditStub{err: errors.New("secret://policy/private")}
	denialCfg := postureTestConfig()
	service := NewPostureService(&denialCfg, nil, audit)
	mux := http.NewServeMux()
	RegisterPostureRoutes(mux, PostureRouteConfig{Service: service})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/desk/posture/denials?session=s", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "secret://policy/private") {
		t.Fatalf("source error leaked: %s", response.Body.String())
	}
}

func TestPostureRoutesAreReadOnly(t *testing.T) {
	audit := &postureAuditStub{returnEmpty: true}
	mux := http.NewServeMux()
	RegisterPostureRoutes(mux, PostureRouteConfig{
		Service: func() *PostureService {
			cfgForTest := postureTestConfig()
			return NewPostureService(&cfgForTest, nil, audit)
		}(),
	})

	ok := httptest.NewRecorder()
	mux.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/api/v1/desk/posture?profile=reviewer", nil))
	if ok.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", ok.Code, ok.Body.String())
	}
	var payload struct {
		PostureView
		Profiles []string `json:"profiles"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Profile != "reviewer" {
		t.Fatalf("profile = %q", payload.Profile)
	}
	if !reflect.DeepEqual(payload.Profiles, []string{"main", "reviewer"}) {
		t.Fatalf("profiles = %v", payload.Profiles)
	}

	// AC4: there is no write path on this surface at all.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/desk/posture", strings.NewReader(`{}`)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}

	long := httptest.NewRecorder()
	mux.ServeHTTP(long, httptest.NewRequest(http.MethodGet,
		"/api/v1/desk/posture?profile="+strings.Repeat("a", config.ProfileNameMax+1), nil))
	if long.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized profile status = %d, want 422", long.Code)
	}
}
