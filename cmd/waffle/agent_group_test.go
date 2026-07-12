package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// TestBuildAgentCronTierExcludesBash is the headline of #33: an unattended
// cron-group agent must not carry host bash, while the owner's main-group
// agent does — from the same default config, no [agent.group.*] required.
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
