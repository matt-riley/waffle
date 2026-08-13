package agentbuild

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

type configuredProfileProvider struct {
	calls int
}

func (p *configuredProfileProvider) Complete(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	p.calls++
	return &llm.Response{
		StopReason: llm.StopEndTurn,
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{
			Type: llm.BlockText,
			Text: "```json\n{\"status\":\"done\",\"summary\":\"read only\"}\n```",
		}}},
	}, nil
}

// TestWorkingSetSubagentRejectsConfiguredWriteCapableProfiles is the #71
// child-profile guard: a spawn may never re-enable a tool the parent is
// denied, even when a config profile declares it (moved from cmd/waffle with
// the builder, #287).
func TestWorkingSetSubagentRejectsConfiguredWriteCapableProfiles(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"wildcard-writer": {
			Tools: config.ToolPolicy{Allow: []string{"*"}},
		},
		"explicit-writer": {
			Tools: config.ToolPolicy{Allow: []string{"read_file", "write_file"}},
		},
		"reader": {
			Tools: config.ToolPolicy{Allow: []string{"read_file", "search"}},
		},
	}
	provider := &configuredProfileProvider{}
	spawn := &workingSetSubagent{
		inner: agent.SubagentTool{
			Provider: provider,
			Tools:    tool.NewRegistry(tool.ReadFile{}, tool.Search{}),
			Model:    "test-model",
		},
		cfg: cfg,
		parentDeny: []string{
			"write_file", "edit_file", "bash", "workspace_update",
			"remember", "memory_update", "distill_skill",
		},
	}

	for _, profile := range []string{"wildcard-writer", "explicit-writer"} {
		_, err := spawn.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"inspect","profile":%q}`, profile)))
		if err == nil || !strings.Contains(err.Error(), "outside the parent toolbox") {
			t.Fatalf("profile %q widening error = %v", profile, err)
		}
	}
	if provider.calls != 0 {
		t.Fatalf("write-capable profiles reached child construction: provider calls = %d", provider.calls)
	}

	result, err := spawn.Run(context.Background(), json.RawMessage(`{"task":"inspect","profile":"reader"}`))
	if err != nil {
		t.Fatalf("read-only profile rejected: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("read-only profile provider calls = %d, want 1", provider.calls)
	}
	if !strings.Contains(result, `"status": "done"`) {
		t.Fatalf("read-only profile result = %s", result)
	}
}

// TestApplyProfileMatchesInstallRepoPolicy is the #287 policy-equivalence
// gate: the post-/repo overlay (installRepo) re-derives tool policy through
// the same ApplyProfile/ApplyRepo helpers as the full builder, so a profile
// with deny-prefixes, guidance, and repo codeintel caps produces one policy.
func TestApplyProfileMatchesInstallRepoPolicy(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"reviewer": {
			System: "review",
			Tools: config.ToolPolicy{
				Allow:        []string{"read_file", "search"},
				DenyPrefixes: []string{"git"},
				Deny:         []string{"bash"},
				Guidance:     "prefer read_file",
			},
			Sandbox: "host",
		},
	}
	// The builder path.
	profile, _ := cfg.Profile("reviewer")
	builderPolicy, _ := ApplyProfile(cfg.AgentPolicy(config.GroupMain), profile)
	builderPolicy = ApplyRepo(builderPolicy, &repopolicy.Policy{
		Tools:         repopolicy.ToolFilter{Allow: []string{"read_file"}},
		CodeIntelCaps: []string{"code_find_symbol"},
	})
	builderPolicy.Profile = "reviewer"

	// The installRepo path (previously re-merged profile allow/deny by hand
	// and missed deny-prefixes/guidance, producing a different policy).
	hostPolicy := cfg.AgentPolicy(config.GroupMain)
	installRepoPolicy, _ := ApplyProfile(hostPolicy, profile)
	installRepoPolicy = ApplyRepo(installRepoPolicy, &repopolicy.Policy{
		Tools:         repopolicy.ToolFilter{Allow: []string{"read_file"}},
		CodeIntelCaps: []string{"code_find_symbol"},
	})
	installRepoPolicy.Profile = "reviewer"

	if !toolPoliciesEqual(builderPolicy, installRepoPolicy) {
		t.Fatalf("builder and installRepo diverged:\n  builder: %+v\n  overlay: %+v", builderPolicy, installRepoPolicy)
	}
	// The profile's deny-prefixes/guidance really landed (the drift #287
	// describes: the old overlay dropped them).
	if len(installRepoPolicy.DenyPrefixes) == 0 {
		t.Fatal("deny-prefixes missing from overlay policy")
	}
	if installRepoPolicy.Guidance == "" {
		t.Fatal("guidance missing from overlay policy")
	}
	if !slicesContains(installRepoPolicy.Deny, "code_references") {
		t.Fatalf("unrequested codeintel caps not denied: %+v", installRepoPolicy.Deny)
	}
	if slicesContains(installRepoPolicy.Deny, "code_find_symbol") {
		t.Fatalf("requested codeintel cap denied: %+v", installRepoPolicy.Deny)
	}
}

func toolPoliciesEqual(a, b tool.Policy) bool {
	return strings.Join(a.Allow, ",") == strings.Join(b.Allow, ",") &&
		strings.Join(a.Deny, ",") == strings.Join(b.Deny, ",") &&
		strings.Join(a.DenyPrefixes, ",") == strings.Join(b.DenyPrefixes, ",") &&
		a.Guidance == b.Guidance && a.Profile == b.Profile
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestSyncWorkspaceOnceRetriesAfterFailure covers #259: a transient reindex
// failure used to mark the workspace done anyway, so memory search returned
// nothing for the life of the process with nothing logged to explain it.
func TestSyncWorkspaceOnceRetriesAfterFailure(t *testing.T) {
	ctx := context.Background()
	ws := memory.Workspace{Dir: t.TempDir()}
	note := "- [id=fts001] 2026-01-01 [trust=owner_stated source=]: the reindex covers pineapples\n"
	if err := os.WriteFile(ws.MemoryPath(), []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}

	// A closed handle stands in for "database is locked at startup".
	closed, err := store.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	syncWorkspaceOnce(&memory.NotesIndex{DB: closed.DB}, memory.DefaultAgent, ws)
	if !strings.Contains(logs.String(), "memory note index sync failed") {
		t.Fatalf("failed sync logged nothing: %s", logs.String())
	}

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	index := &memory.NotesIndex{DB: st.DB}

	// The retry must actually happen: the first attempt failing cannot leave
	// the workspace marked as indexed.
	syncWorkspaceOnce(index, memory.DefaultAgent, ws)
	hits, err := index.Search(ctx, "pineapples", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the note indexed by the retry", hits)
	}
}

func TestSyncWorkspaceOnceSkipsRepeatAfterSuccess(t *testing.T) {
	ctx := context.Background()
	ws := memory.Workspace{Dir: t.TempDir()}
	if err := os.WriteFile(ws.MemoryPath(), []byte("- [id=fts002] 2026-01-01 [trust=owner_stated source=]: kumquats are indexed once\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	index := &memory.NotesIndex{DB: st.DB}

	syncWorkspaceOnce(index, memory.DefaultAgent, ws)
	// Remove the source file: a second full sync would empty the index.
	if err := os.Remove(ws.MemoryPath()); err != nil {
		t.Fatal(err)
	}
	syncWorkspaceOnce(index, memory.DefaultAgent, ws)

	hits, err := index.Search(ctx, "kumquats", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the successful sync not to be repeated", hits)
	}
}

// TestBuildRegistersGitHubHostToolsOnlyWhenAnAppExists is the #252 wiring
// gate: with [github.app] the host toolbox offers the full GitHub surface
// (github_pr plus the read/comment tools); without it none of them are
// offered. The tools live in the host toolbox, never the sandbox executor, so
// their per-call tokens cannot reach a container.
func TestBuildRegistersGitHubHostToolsOnlyWhenAnAppExists(t *testing.T) {
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
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key" // literal, avoids secret-store lookup
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	sessions := session.New(st)
	ws := memory.Workspace{Dir: t.TempDir()}
	runtime := &buildTestRuntime{}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	app, err := gitcred.NewApp(42, 7, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "http://github.test", nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	githubTools := []string{"github_pr", "github_pr_get", "github_pr_diff", "github_pr_comments", "github_comment", "github_checks", "github_issue_get"}
	defNames := func(a *agent.Agent) map[string]bool {
		names := map[string]bool{}
		for _, d := range a.Tools.Defs() {
			names[d.Name] = true
		}
		return names
	}

	withApp, cleanup, err := (&Builder{
		Config: cfg, Sessions: sessions, Workspace: ws, Runtime: runtime,
		GitHubApp: func() (*gitcred.App, error) { return app, nil },
	}).Build(ctx, config.GroupMain, "")
	if err != nil {
		t.Fatalf("build with app: %v", err)
	}
	defer func() { _ = cleanup.Stop() }()
	names := defNames(withApp)
	for _, name := range githubTools {
		if !names[name] {
			t.Errorf("with app: toolbox missing %q", name)
		}
	}

	without, cleanup2, err := (&Builder{
		Config: cfg, Sessions: sessions, Workspace: ws, Runtime: runtime,
	}).Build(ctx, config.GroupMain, "")
	if err != nil {
		t.Fatalf("build without app: %v", err)
	}
	defer func() { _ = cleanup2.Stop() }()
	names = defNames(without)
	for _, name := range githubTools {
		if names[name] {
			t.Errorf("without app: toolbox must not offer %q", name)
		}
	}
}

// buildTestRuntime is a minimal Runtime for Builder tests: it answers model
// calls and resolves a default alias without touching the network.
type buildTestRuntime struct {
	*configuredProfileProvider
}

func (r *buildTestRuntime) Resolve(alias string) (config.ResolvedModel, error) {
	return config.ResolvedModel{
		Alias:          alias,
		ConnectionName: "default",
		Connection: config.ProviderConnection{
			Type:      "anthropic",
			APIKey:    "test-key",
			MaxTokens: 8192,
		},
		UpstreamModel: alias,
		MaxTokens:     8192,
	}, nil
}

func (r *buildTestRuntime) Redact(s string) string { return s }

// web_search is offered only when [search] config exists, a broker is wired,
// and the effective policy permits the tool (config denies it by default for
// the restricted tiers) — matching the "absent config disables the tool"
// contract (#245).
func TestBuildRegistersWebSearchOnlyWithSearchConfigBrokerAndPermission(t *testing.T) {
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
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	sessions := session.New(st)
	ws := memory.Workspace{Dir: t.TempDir()}
	runtime := &buildTestRuntime{}

	b := broker.New(st, nil)
	b.SetAPIFaces([]broker.APIFace{{
		Name: "brave", BaseURL: "https://api.search.brave.com",
		Header: "X-Subscription-Token", Value: "test-key",
		Methods: []string{"GET"}, Paths: []string{"/res/v1/web/search"},
	}})
	searchSpec := &tool.WebSearchSpec{Type: "brave", Face: "brave"}

	build := func(spec *tool.WebSearchSpec, broker *broker.Broker, group string) *agent.Agent {
		t.Helper()
		a, cleanup, err := (&Builder{
			Config: cfg, Sessions: sessions, Workspace: ws, Runtime: runtime,
			Broker: broker, BrokerURL: "http://127.0.0.1:8421", Search: spec,
		}).Build(ctx, group, "")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		t.Cleanup(func() { _ = cleanup.Stop() })
		return a
	}
	hasSearch := func(a *agent.Agent) bool {
		for _, d := range a.Tools.Defs() {
			if d.Name == "web_search" {
				return true
			}
		}
		return false
	}

	// With search config, a broker, and main-tier permission: offered.
	if a := build(searchSpec, b, config.GroupMain); !hasSearch(a) {
		t.Fatal("web_search must be offered with config + broker + main tier")
	}
	// Absent search config: not offered.
	if a := build(nil, b, config.GroupMain); hasSearch(a) {
		t.Fatal("web_search must not be offered without [search] config")
	}
	// Absent broker: not offered.
	if a := build(searchSpec, nil, config.GroupMain); hasSearch(a) {
		t.Fatal("web_search must not be offered without a broker")
	}
	// Restricted tier without explicit allow: not offered (deny-by-default).
	if a := build(searchSpec, b, config.GroupCron); hasSearch(a) {
		t.Fatal("web_search must be denied for cron by default")
	}
}
