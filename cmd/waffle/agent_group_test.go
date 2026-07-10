package main

import (
	"context"
	"path/filepath"
	"testing"

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
	defer st.Close() //nolint:errcheck // test teardown
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
	}
}

func TestConfiguredGatewayGroupBuildsRegistryEntry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test teardown

	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	cfg.Agent.Groups = map[string]config.AgentGroup{
		"restricted": {Tools: config.ToolPolicy{Deny: []string{"bash"}}},
	}

	agents, _, cleanup, err := buildGatewayAgents(ctx, cfg, memory.Workspace{Dir: t.TempDir()}, nil, session.New(st))
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
	defer st.Close() //nolint:errcheck // test teardown

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
