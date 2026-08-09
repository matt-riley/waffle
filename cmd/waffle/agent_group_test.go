package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestBuildAgentCronTierExcludesBash(t *testing.T) {
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
	sessions := session.New(st)
	ws := memory.Workspace{Dir: t.TempDir()}

	cfg := config.Default()
	cfg.Provider.APIKey = "test-key" // literal, avoids secret-store lookup
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false

	mainAgent, mainCleanup, err := buildAgent(ctx, cfg, ws, nil, sessions, config.GroupMain)
	if err != nil {
		t.Fatalf("build main agent: %v", err)
	}
	defer mainCleanup()

	cronAgent, cronCleanup, err := buildAgent(ctx, cfg, ws, nil, sessions, config.GroupCron)
	if err != nil {
		t.Fatalf("build cron agent: %v", err)
	}
	defer cronCleanup()

	mainDefs := mainAgent.Tools.Defs()
	cronDefs := cronAgent.Tools.Defs()

	mainHasBash := false
	for _, d := range mainDefs {
		if d.Name == "bash" {
			mainHasBash = true
		}
	}
	if !mainHasBash {
		t.Error("main agent is missing bash; expected the owner tier to keep host shell")
	}

	for _, d := range cronDefs {
		if d.Name == "bash" {
			t.Error("cron agent exposes bash; the unattended tier must deny host shell by default")
		}
		if d.Name == "workspace_update" {
			t.Error("cron agent exposes workspace_update; restricted tiers deny working-set mutation")
		}
	}
	mainHasWS, mainHasExpand := false, false
	for _, d := range mainDefs {
		if d.Name == "workspace_update" {
			mainHasWS = true
		}
		if d.Name == "expand_output" || d.Name == "expand_context" {
			mainHasExpand = true
		}
	}
	if !mainHasWS {
		t.Error("main agent missing workspace_update")
	}
	if !mainHasExpand {
		t.Error("main agent missing expand_output/expand_context")
	}
	if mainAgent.Spill == nil {
		t.Error("main agent missing Spill store")
	}
}

func TestConfiguredGatewayGroupBuildsRegistryEntry(t *testing.T) {
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
	cfg.Agent.Groups = map[string]config.AgentGroup{
		"restricted": {Tools: config.ToolPolicy{Deny: []string{"bash"}}},
	}

	agents, _, _, _, _, cleanup, err := buildGatewayAgents(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st))
	if err != nil {
		t.Fatalf("build gateway agents: %v", err)
	}
	defer cleanup()

	if agents["restricted"] == nil {
		t.Fatal("restricted group is missing from the gateway registry")
	}
	if agents[config.GroupMain] == nil {
		t.Fatal("main group is missing from the gateway registry")
	}

	for _, d := range agents["restricted"].Tools.Defs() {
		if d.Name == "bash" {
			t.Error("restricted gateway group exposes bash")
		}
	}
	mainHasBash := false
	for _, d := range agents[config.GroupMain].Tools.Defs() {
		if d.Name == "bash" {
			mainHasBash = true
		}
	}
	if !mainHasBash {
		t.Error("main gateway group is missing bash")
	}
}

// TestBuildAgentGroupTierRestrictsTools is #34's security gate: multi-party
// group chats must not expose host bash or durable memory writes.
func TestBuildAgentGroupTierRestrictsTools(t *testing.T) {
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
	cfg.Agent.Learn = true
	a, cleanup, err := buildAgent(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupGroup)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, d := range a.Tools.Defs() {
		switch d.Name {
		case "bash", "remember", "memory_update", "distill_skill":
			t.Errorf("group tier exposes %s", d.Name)
		}
	}
}

// TestBuildGatewayAgentsIncludesGroupTier ensures the gateway registry always
// has the multi-party "group" agent ready for Telegram group chats.
func TestBuildGatewayAgentsIncludesGroupTier(t *testing.T) {
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
	agents, _, _, _, _, cleanup, err := buildGatewayAgents(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st))
	if err != nil {
		t.Fatalf("buildGatewayAgents: %v", err)
	}
	defer cleanup()
	if agents[config.GroupGroup] == nil {
		t.Fatal("group tier missing from gateway registry")
	}
}

// TestBuildAgentIssueTierRestrictsTools is #51's security gate: issue-driven
// runs must not expose host bash or durable memory writes.
func TestBuildAgentIssueTierRestrictsTools(t *testing.T) {
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
	cfg.Agent.Learn = true
	a, cleanup, err := buildAgent(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupIssue)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, d := range a.Tools.Defs() {
		switch d.Name {
		case "bash", "remember", "memory_update", "distill_skill":
			t.Errorf("issue tier exposes %s", d.Name)
		}
	}
}

func TestBuildAgentWithProfileSpecialist(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Prompt file under WAFFLE_HOME.
	promptPath := filepath.Join(home, "reviewer.md")
	if err := os.WriteFile(promptPath, []byte("You are a reviewer."), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Provider.Model = "default-model"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"reviewer": {
			System:        "@" + promptPath,
			Model:         "special-model",
			MaxTokens:     1234,
			MaxIterations: 9,
			Tools:         config.ToolPolicy{Allow: []string{"read_file", "search"}, Deny: []string{"bash", "write_file", "edit_file"}},
		},
		"readonly": {
			System: "inline only",
			Tools:  config.ToolPolicy{Allow: []string{"read_file", "search", "fetch", "recall", "expand_output", "expand_context"}},
		},
	}

	mainA, cleanup, err := buildAgent(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if mainA.Model != "default-model" {
		t.Fatalf("main model = %q", mainA.Model)
	}

	spec, cleanup2, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if spec.Model != "special-model" || spec.MaxTokens != 1234 || spec.MaxIterations != 9 {
		t.Fatalf("specialist = model=%s tokens=%d iter=%d", spec.Model, spec.MaxTokens, spec.MaxIterations)
	}
	if !strings.Contains(spec.System, "You are a reviewer.") {
		t.Fatalf("system missing file prompt: %q", spec.System)
	}
	for _, d := range spec.Tools.Defs() {
		if d.Name == "bash" || d.Name == "write_file" {
			t.Errorf("reviewer exposes %s", d.Name)
		}
	}

	ro, cleanup3, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "readonly")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup3()
	for _, d := range ro.Tools.Defs() {
		switch d.Name {
		case "bash", "write_file", "edit_file", "remember", "memory_update", "distill_skill", "workspace_update":
			t.Errorf("readonly profile exposes mutation tool %s", d.Name)
		}
	}

	// Missing prompt file errors.
	cfg.Agent.Profiles["broken"] = config.AgentProfile{System: "@" + filepath.Join(home, "missing.md")}
	if _, _, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "broken"); err == nil {
		t.Fatal("missing prompt file accepted")
	}
	// Path outside WAFFLE_HOME errors.
	outside := filepath.Join(t.TempDir(), "escape.md")
	_ = os.WriteFile(outside, []byte("x"), 0o600)
	cfg.Agent.Profiles["escape"] = config.AgentProfile{System: "@" + outside}
	if _, _, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "escape"); err == nil {
		t.Fatal("outside-root prompt accepted")
	}
	// A path under WAFFLE_HOME that cannot be read as a prompt file also errors.
	unreadable := filepath.Join(home, "prompt-directory")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Profiles["unreadable"] = config.AgentProfile{System: "@" + unreadable}
	if _, _, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "unreadable"); err == nil {
		t.Fatal("unreadable prompt path accepted")
	}
	// The selected profile's sandbox posture reaches agent construction.
	cfg.Agent.Profiles["bad-sandbox"] = config.AgentProfile{System: "x", Sandbox: "invalid-mode"}
	if _, _, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "bad-sandbox"); err == nil || !strings.Contains(err.Error(), "invalid-mode") {
		t.Fatalf("profile sandbox posture error = %v", err)
	}
	// Explicit empty system is allowed.
	cfg.Agent.Profiles["empty-sys"] = config.AgentProfile{System: ""}
	emptyA, cleanup4, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "empty-sys")
	if err != nil {
		t.Fatalf("empty system: %v", err)
	}
	cleanup4()
	if emptyA.Profile != "empty-sys" {
		t.Fatalf("profile name = %q", emptyA.Profile)
	}
}

func TestDefaultMainProfilePreservesAgentConstruction(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Provider.Model = "default-model"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	ws := memory.Workspace{Dir: t.TempDir()}
	sessions := session.New(st)

	legacy, cleanupLegacy, err := buildAgent(ctx, cfg, ws, nil, sessions, config.GroupMain)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupLegacy()
	profiled, cleanupProfiled, err := buildAgentWithProfile(ctx, cfg, ws, nil, sessions, config.GroupMain, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupProfiled()

	if fmt.Sprintf("%T", legacy.Provider) != fmt.Sprintf("%T", profiled.Provider) || legacy.Model != profiled.Model || legacy.System != profiled.System ||
		legacy.MaxTokens != profiled.MaxTokens || legacy.MaxIterations != profiled.MaxIterations ||
		(legacy.Redact == nil) != (profiled.Redact == nil) {
		t.Fatalf("default main changed agent construction: legacy=%+v profiled=%+v", legacy, profiled)
	}
	if legacy.Redact != nil && legacy.Redact("test-key visible") != profiled.Redact("test-key visible") {
		t.Fatal("default main changed redactor behavior")
	}
	legacyDefs, profiledDefs := legacy.Tools.Defs(), profiled.Tools.Defs()
	if len(legacyDefs) != len(profiledDefs) {
		t.Fatalf("toolbox size changed: legacy=%d profiled=%d", len(legacyDefs), len(profiledDefs))
	}
	for i := range legacyDefs {
		if legacyDefs[i].Name != profiledDefs[i].Name {
			t.Fatalf("toolbox changed at %d: legacy=%q profiled=%q", i, legacyDefs[i].Name, profiledDefs[i].Name)
		}
	}
}

func TestBuildAgentActionRuleDenialIncludesProfileProvenanceWithoutInput(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"reviewer": {System: "review", Tools: config.ToolPolicy{Allow: []string{"read_file"}}},
	}
	cfg.Policy.Rule = []config.PolicyRule{{Name: "no-private-read", Tool: "read_file", Action: "deny"}}
	a, cleanup, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	_, err = a.Tools.Run(ctx, "read_file", json.RawMessage(`{"path":"PRIVATE_INPUT_PATH"}`))
	if err == nil {
		t.Fatal("action rule allowed read_file")
	}
	denial := err.Error()
	for _, want := range []string{`profile "reviewer"`, `policy source "policy.rule"`, `rule "no-private-read"`} {
		if !strings.Contains(denial, want) {
			t.Fatalf("denial missing %q: %s", want, denial)
		}
	}
	if strings.Contains(denial, "PRIVATE_INPUT_PATH") {
		t.Fatalf("tool input leaked into denial: %s", denial)
	}
}

func TestProfileUtilityModelSelectionBuild(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Provider.Model = "main-model"
	cfg.Provider.UtilityModel = "cheap-model"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"defaultish": {System: "d", Model: "default"},
		"cheap":      {System: "u", Model: "utility"},
		"explicit":   {System: "e", Model: "claude-x"},
	}
	a, c1, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "defaultish")
	if err != nil {
		t.Fatal(err)
	}
	c1()
	if a.Model != "main-model" {
		t.Fatalf("default model = %q", a.Model)
	}
	a, c2, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "cheap")
	if err != nil {
		t.Fatal(err)
	}
	c2()
	if a.Model != "cheap-model" {
		t.Fatalf("utility model = %q", a.Model)
	}
	a, c3, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "explicit")
	if err != nil {
		t.Fatal(err)
	}
	c3()
	if a.Model != "claude-x" {
		t.Fatalf("explicit model = %q", a.Model)
	}
	// Missing utility_model errors at build.
	cfg.Provider.UtilityModel = ""
	if _, _, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "cheap"); err == nil {
		t.Fatal("missing utility_model accepted")
	}
}

// TestWorkspaceOrChatProfileAffectsAgent proves a stored workspace profile (or
// chat --profile) selects the system prompt and toolbox used by the run (#71).
func TestWorkspaceOrChatProfileAffectsAgent(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Provider.Model = "default-model"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"researcher": {
			System: "You are a research specialist.",
			Model:  "research-model",
			Tools:  config.ToolPolicy{Allow: []string{"read_file", "fetch", "search", "recall"}, Deny: []string{"bash", "write_file", "edit_file"}},
		},
	}
	bound := workspace.Workspace{Profile: "researcher"}
	a, cleanup, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, bound.Profile)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if a.Profile != "researcher" {
		t.Fatalf("Profile = %q", a.Profile)
	}
	if a.Model != "research-model" {
		t.Fatalf("Model = %q", a.Model)
	}
	if !strings.Contains(a.System, "research specialist") {
		t.Fatalf("system = %q", a.System)
	}
	for _, d := range a.Tools.Defs() {
		if d.Name == "bash" || d.Name == "write_file" {
			t.Errorf("researcher exposes %s", d.Name)
		}
	}
	// Direct call to denied tool fails with profile name in denial.
	_, err = a.Tools.Run(ctx, "bash", nil)
	if err == nil || !strings.Contains(err.Error(), `profile "researcher"`) {
		t.Fatalf("denial = %v", err)
	}
}

// TestBuildGatewayAgentsBuildsProfiles ensures serve-time wiring pre-builds
// profile agents for channel (main tier) and cron surfaces (#71).
func TestBuildGatewayAgentsBuildsProfiles(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Provider.Model = "default-model"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"reviewer": {
			System: "You are a reviewer profile.",
			Model:  "special-model",
			Tools:  config.ToolPolicy{Allow: []string{"read_file", "search"}},
		},
	}
	agents, cronAgent, profilesMain, profilesGroup, profilesCron, cleanup, err := buildGatewayAgents(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st))
	if err != nil {
		t.Fatalf("buildGatewayAgents: %v", err)
	}
	defer cleanup()
	if agents[config.GroupMain] == nil || cronAgent == nil {
		t.Fatal("missing main/cron agents")
	}
	mainP := profilesMain["reviewer"]
	groupP := profilesGroup["reviewer"]
	cronP := profilesCron["reviewer"]
	if mainP == nil || groupP == nil || cronP == nil {
		t.Fatalf("profiles main=%v group=%v cron=%v", mainP != nil, groupP != nil, cronP != nil)
	}
	if mainP.Model != "special-model" || cronP.Model != "special-model" {
		t.Fatalf("models main=%q cron=%q", mainP.Model, cronP.Model)
	}
	if !strings.Contains(mainP.System, "reviewer profile") {
		t.Fatalf("main profile system = %q", mainP.System)
	}
	for _, a := range []*agent.Agent{mainP, groupP, cronP} {
		for _, d := range a.Tools.Defs() {
			if d.Name == "bash" {
				t.Errorf("profile agent exposes bash")
			}
		}
	}
}

func TestIntakeOnlyFromServePath(t *testing.T) {
	// Document single-owner: issue intake watchers start only under waffle serve
	// (runIntakeWatchers), never from chat/cron CLI entry points.
	// Source-level guard: runIntakeWatchers is only referenced from serve.
	src, err := os.ReadFile("serve_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "runIntakeWatchers") {
		t.Fatal("serve must own intake")
	}
	// Other cmds must not call it.
	for _, f := range []string{"chat_cmd.go", "cron_cmd.go", "main.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if f != "main.go" && strings.Contains(string(b), "runIntakeWatchers") {
			t.Fatalf("%s must not start intake", f)
		}
	}
}

// TestBuildCronRunnerUsesCronTier guards the wiring: `waffle cron run` must
// build its agent on the restricted cron tier (no host bash), matching what
// the scheduler runs under `waffle serve` — not the owner's main tier.
func TestBuildCronRunnerUsesCronTier(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
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

	runner, cleanup, err := buildCronRunner(ctx, cfg, st)
	if err != nil {
		t.Fatalf("buildCronRunner: %v", err)
	}
	defer cleanup()

	for _, d := range runner.Agent.Tools.Defs() {
		if d.Name == "bash" {
			t.Error("cron runner exposes host bash; manual `cron run` must match the restricted scheduled tier")
		}
	}
}

// TestBuildAgentFileRootsConfineFileTools is the #269 wiring proof: a
// configured boundary reaches the tools the agent actually runs, and a profile
// that tries to widen it is refused at build time rather than silently
// granted.
func TestBuildAgentFileRootsConfineFileTools(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Sandbox.FileRoots = []string{root}
	cfg.Agent.Profiles = map[string]config.AgentProfile{
		"narrow": {Tools: config.ToolPolicy{FileRoots: []string{inner}}},
		"escape": {Tools: config.ToolPolicy{FileRoots: []string{outside}}},
	}

	a, cleanup, err := buildAgent(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := a.Tools.Run(ctx, "read_file", json.RawMessage(fmt.Sprintf(`{"path":%q}`, secret))); !errors.Is(err, tool.ErrOutsideRoots) {
		t.Errorf("read outside roots = %v, want ErrOutsideRoots", err)
	}

	narrow, cleanupNarrow, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "narrow")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupNarrow()
	sibling := filepath.Join(root, "sibling.txt")
	if err := os.WriteFile(sibling, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := narrow.Tools.Run(ctx, "read_file", json.RawMessage(fmt.Sprintf(`{"path":%q}`, sibling))); !errors.Is(err, tool.ErrOutsideRoots) {
		t.Errorf("narrowed profile read outside its own root = %v, want ErrOutsideRoots", err)
	}

	if _, _, err := buildAgentWithProfile(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st), config.GroupMain, "escape"); !errors.Is(err, config.ErrProfileWidens) {
		t.Errorf("widening profile built with err = %v, want ErrProfileWidens", err)
	}
}
