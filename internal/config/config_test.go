package config

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[gateway]\nlisten = \"x\"\nlistne_typo = true\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted unknown key, want error")
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

	// An unknown group falls back to the global sandbox policy (no bash deny).
	other := cfg.AgentPolicy("adhoc")
	if contains(other.Deny, "bash") {
		t.Errorf("unknown group denied bash unexpectedly: %v", other.Deny)
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
